package lnruntime

import (
	"bytes"
	"context"
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/rpc/arkchannelrpc"
)

// fundingAuthorizer is local evidence; it is never exposed as a peer event.
type fundingAuthorizer interface {
	AuthorizeFundingBacking(context.Context, arkchannel.Terms,
		arkchannel.VTXOBinding, wire.OutPoint, func() error) error

	SignAuthorizedBacking(context.Context, arkchannel.Terms, chainhash.Hash,
		func() ([]byte, error)) ([]byte, error)
}

// fundingAuthorizer is shared by both native endpoints, including the local
// initiator which does not pass through a funding RPC.
func (e *NativeFundingEndpoint) fundingAuthorizer() (fundingAuthorizer, error) {
	e.mu.RLock()
	authorizer, ok := e.sink.(fundingAuthorizer)
	e.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("durable funding authorizer is required")
	}

	return authorizer, nil
}

// SignBacking signs only the PSBT bound to the immutable local intent.
func (s *FundingPeerRPCServer) SignBacking(ctx context.Context,
	request *arkchannelrpc.SignBackingRequest) (
	*arkchannelrpc.SignBackingResponse, error) {

	id, terms, binding, record, err := s.request(
		ctx, request.GetChannelId(), request.GetTerms(),
		request.GetBinding(),
	)
	if err != nil {
		return nil, err
	}
	packet, err := psbt.NewFromRawBytes(
		bytes.NewReader(
			request.GetFundingPsbt(),
		),
		false,
	)
	if err != nil {
		return nil, fmt.Errorf("decode lnd funding PSBT: %w", err)
	}
	if backing := record.Snapshot.Backing; backing != nil {
		packetCopy, err := cloneFundingPSBT(packet)
		if err != nil {
			return nil, err
		}
		template, err := arkchannel.NewBackingTemplate(
			packetCopy, terms, binding,
		)
		if err != nil {
			return nil, err
		}
		if template.ChannelPoint() != backing.ChannelPoint {
			return nil, fmt.Errorf("funding backing does not " +
				"match channel FSM")
		}
	}
	signature, err := s.cfg.Funding.SignBacking(
		ctx, id, terms, binding, packet,
	)
	if err != nil {
		return nil, err
	}

	return &arkchannelrpc.SignBackingResponse{
		Signature: signature.Serialize(),
	}, nil
}

// InstallBacking rejects a conflicting replay for the authenticated channel.
func (s *FundingPeerRPCServer) InstallBacking(ctx context.Context,
	request *arkchannelrpc.InstallBackingRequest) (
	*arkchannelrpc.InstallBackingResponse, error) {

	id, terms, binding, record, err := s.request(
		ctx, request.GetChannelId(), request.GetTerms(),
		request.GetBinding(),
	)
	if err != nil {
		return nil, err
	}
	backing, err := channelBackingFromRPC(request.GetBacking())
	if err != nil {
		return nil, err
	}
	if record.Snapshot.Backing != nil && !sameFundingBacking(
		*record.Snapshot.Backing, backing,
	) {
		return nil, fmt.Errorf("funding backing does not match " +
			"channel FSM")
	}
	// Later phases have already consumed this exact installation. Do not
	// recreate native registration on a delayed acknowledgement retry.
	if record.Snapshot.Backing != nil &&
		record.Snapshot.Phase != arkchannel.PhaseNegotiating {

		if record.Snapshot.Phase == arkchannel.PhaseCancelling ||
			record.Snapshot.Phase == arkchannel.PhaseFailed {
			return nil, fmt.Errorf("cannot install abandoned " +
				"backing")
		}

		return &arkchannelrpc.InstallBackingResponse{}, nil
	}
	if err := s.cfg.Funding.InstallBacking(
		ctx, id, terms, binding, backing,
	); err != nil {
		return nil, err
	}

	return &arkchannelrpc.InstallBackingResponse{}, nil
}

// FundingFinalized queries native durability for the exact recorded backing.
func (s *FundingPeerRPCServer) FundingFinalized(ctx context.Context,
	request *arkchannelrpc.FundingStatusRequest) (
	*arkchannelrpc.FundingStatusResponse, error) {

	terms, backing, err := s.status(ctx, request)
	if err != nil {
		return nil, err
	}
	ready, err := s.cfg.Funding.FundingFinalized(ctx, terms, backing)
	if err != nil {
		return nil, err
	}

	return &arkchannelrpc.FundingStatusResponse{Ready: ready}, nil
}

// ChannelActive queries the native link for the exact recorded backing.
func (s *FundingPeerRPCServer) ChannelActive(ctx context.Context,
	request *arkchannelrpc.FundingStatusRequest) (
	*arkchannelrpc.FundingStatusResponse, error) {

	terms, backing, err := s.status(ctx, request)
	if err != nil {
		return nil, err
	}
	ready, err := s.cfg.Funding.ChannelActive(ctx, terms, backing)
	if err != nil {
		return nil, err
	}

	return &arkchannelrpc.FundingStatusResponse{Ready: ready}, nil
}

// ApplyChannelEvent applies a fact from the authenticated client.
func (s *FundingPeerRPCServer) ApplyChannelEvent(ctx context.Context,
	request *arkchannelrpc.ApplyChannelEventRequest) (
	*arkchannelrpc.ApplyChannelEventResponse, error) {

	id, _, err := s.channel(ctx, request.GetChannelId())
	if err != nil {
		return nil, err
	}
	event, err := channelEventFromRPC(request)
	if err != nil {
		return nil, err
	}
	record, err := s.cfg.Funding.ApplyChannelEvent(ctx, id, event)
	if err != nil {
		return nil, err
	}

	return &arkchannelrpc.ApplyChannelEventResponse{
		Channel: ArkChannelRecordToRPC(record),
	}, nil
}

// request binds all untrusted funding inputs to one authenticated record.
func (s *FundingPeerRPCServer) request(ctx context.Context, rawID []byte,
	termsRPC *arkchannelrpc.ChannelTerms,
	bindingRPC *arkchannelrpc.ChannelVTXOBinding) (arkchannel.ID,
	arkchannel.Terms, arkchannel.VTXOBinding, arkchannel.Record, error) {

	id, record, err := s.channel(ctx, rawID)
	if err != nil {
		return arkchannel.ID{}, arkchannel.Terms{},
			arkchannel.VTXOBinding{}, arkchannel.Record{}, err
	}
	terms, err := channelTermsFromRPC(termsRPC)
	if err != nil {
		return arkchannel.ID{}, arkchannel.Terms{},
			arkchannel.VTXOBinding{}, arkchannel.Record{}, err
	}
	binding, err := channelBindingFromRPC(bindingRPC)
	if err != nil {
		return arkchannel.ID{}, arkchannel.Terms{},
			arkchannel.VTXOBinding{}, arkchannel.Record{}, err
	}
	if err := validateFundingRequestRecord(
		record, id, terms, binding,
	); err != nil {
		return arkchannel.ID{}, arkchannel.Terms{},
			arkchannel.VTXOBinding{}, arkchannel.Record{}, err
	}

	return id, record.Snapshot.Terms.Clone(),
		record.Snapshot.Source.Clone(), record, nil
}

// status prevents a peer from querying readiness for a different artifact.
func (s *FundingPeerRPCServer) status(ctx context.Context,
	request *arkchannelrpc.FundingStatusRequest) (arkchannel.Terms,
	arkchannel.Backing, error) {

	terms, err := channelTermsFromRPC(request.GetTerms())
	if err != nil {
		return arkchannel.Terms{}, arkchannel.Backing{}, err
	}
	backing, err := channelBackingFromRPC(request.GetBacking())
	if err != nil {
		return arkchannel.Terms{}, arkchannel.Backing{}, err
	}
	_, record, err := s.channel(ctx, terms.ID[:])
	if err != nil {
		return arkchannel.Terms{}, arkchannel.Backing{}, err
	}
	if err := validateFundingStatusRecord(
		record, terms, backing,
	); err != nil {
		return arkchannel.Terms{}, arkchannel.Backing{}, err
	}

	return record.Snapshot.Terms.Clone(),
		record.Snapshot.Backing.Clone(), nil
}

// validateFundingRequestRecord binds signing and installation to the stored
// intent.
func validateFundingRequestRecord(record arkchannel.Record, id arkchannel.ID,
	terms arkchannel.Terms, binding arkchannel.VTXOBinding) error {

	if id != terms.ID || record.Snapshot.Terms != terms ||
		record.Snapshot.Source == nil ||
		!sameFundingBinding(*record.Snapshot.Source, binding) {
		return fmt.Errorf("funding request does not match channel FSM")
	}

	return nil
}

// validateFundingStatusRecord binds native status queries to the stored
// backing.
func validateFundingStatusRecord(record arkchannel.Record,
	terms arkchannel.Terms, backing arkchannel.Backing) error {

	if record.Snapshot.Terms != terms || record.Snapshot.Backing == nil ||
		!sameFundingBacking(*record.Snapshot.Backing, backing) {
		return fmt.Errorf("funding status does not match channel FSM")
	}

	return nil
}

// sameFundingBinding compares every field of the prepared OOR output.
func sameFundingBinding(a, b arkchannel.VTXOBinding) bool {
	return a.OORSessionID == b.OORSessionID && a.OutPoint == b.OutPoint &&
		a.Amount == b.Amount && bytes.Equal(
		a.ArkTransaction, b.ArkTransaction,
	) && bytes.Equal(a.PolicyTemplate,
		b.PolicyTemplate) && bytes.Equal(a.PkScript, b.PkScript)
}

// sameFundingBacking compares the outpoint and fully signed transaction.
func sameFundingBacking(a, b arkchannel.Backing) bool {
	return a.ChannelPoint == b.ChannelPoint && bytes.Equal(
		a.Transaction, b.Transaction,
	)
}
