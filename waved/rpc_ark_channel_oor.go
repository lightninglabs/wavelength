package waved

import (
	"context"
	"errors"
	"fmt"

	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/arkchannel/oorbridge"
	"github.com/lightninglabs/wavelength/lnruntime"
	"github.com/lightninglabs/wavelength/rpc/arkchannelrpc"
	"github.com/lightninglabs/wavelength/waverpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// PrepareArkChannelOOR reserves daemon-owned VTXOs and creates the exact
// channel-policy output without releasing any OOR signatures.
func (r *RPCServer) PrepareArkChannelOOR(ctx context.Context,
	req *waverpc.PrepareArkChannelOORRequest) (
	*waverpc.PrepareArkChannelOORResponse, error) {

	terms, err := lnruntime.ChannelTermsFromRPC(req.GetTerms())
	if err != nil {
		return nil, err
	}
	if terms.Funder != arkchannel.PartyHub ||
		terms.Kind != arkchannel.KindReceiveIntent {
		return nil, fmt.Errorf("daemon channel OOR must fund a " +
			"receive intent")
	}
	binding, err := r.server.prepareArkChannelOOR(
		ctx, terms, arkchannel.DefaultBackingFee,
	)
	if err != nil {
		if errors.Is(err, arkchannel.ErrOORPreparationAmbiguous) {
			return nil, status.Error(codes.Aborted, err.Error())
		}

		return nil, err
	}

	return &waverpc.PrepareArkChannelOORResponse{
		Binding: lnruntime.ChannelBindingToRPC(binding),
	}, nil
}

// LookupPreparedArkChannelOOR reconciles one deterministic channel OOR key
// without selecting or locking new wallet inputs.
func (r *RPCServer) LookupPreparedArkChannelOOR(ctx context.Context,
	req *waverpc.LookupPreparedArkChannelOORRequest) (
	*waverpc.LookupPreparedArkChannelOORResponse, error) {

	terms, err := lnruntime.ChannelTermsFromRPC(req.GetTerms())
	if err != nil {
		return nil, err
	}
	if terms.Funder != arkchannel.PartyHub ||
		terms.Kind != arkchannel.KindReceiveIntent {
		return nil, fmt.Errorf("daemon channel OOR must fund a " +
			"receive intent")
	}
	lookup, err := r.server.lookupArkChannelOOR(
		ctx, terms, arkchannel.DefaultBackingFee,
	)
	if err != nil {
		return nil, err
	}

	response := &waverpc.LookupPreparedArkChannelOORResponse{}
	switch lookup.Status {
	case oorbridge.PreparationAbsent:
	case oorbridge.PreparationPending:
	case oorbridge.PreparationPrepared:
		response.Binding = lnruntime.ChannelBindingToRPC(lookup.Binding)

	case oorbridge.PreparationAccepted:
	default:
		return nil, fmt.Errorf("unknown channel OOR preparation "+
			"status %d", lookup.Status)
	}
	// The RPC enum reserves zero for unspecified while the internal
	// statuses are the same contiguous sequence starting at zero.
	response.Status = waverpc.ArkChannelOORPreparationStatus(
		int32(lookup.Status) + 1,
	)

	return response, nil
}

// ValidatePreparedArkChannelOOR verifies a daemon-owned prepared session.
func (r *RPCServer) ValidatePreparedArkChannelOOR(ctx context.Context,
	req *waverpc.ValidatePreparedArkChannelOORRequest) (
	*waverpc.ValidatePreparedArkChannelOORResponse, error) {

	terms, binding, err := r.channelOORRequest(
		req.GetTerms(), req.GetBinding(),
	)
	if err != nil {
		return nil, err
	}
	controller, err := oorbridge.New(r.server.actorSystem)
	if err != nil {
		return nil, err
	}
	if err := controller.ValidatePreparedOOR(
		ctx, terms, binding,
	); err != nil {
		return nil, err
	}

	return &waverpc.ValidatePreparedArkChannelOORResponse{Valid: true}, nil
}

// CommitPreparedArkChannelOOR releases a prepared daemon OOR and waits for
// its durable finalization.
func (r *RPCServer) CommitPreparedArkChannelOOR(ctx context.Context,
	req *waverpc.CommitPreparedArkChannelOORRequest) (
	*waverpc.CommitPreparedArkChannelOORResponse, error) {

	id, terms, binding, err := r.channelOORControlRequest(
		req.GetChannelId(), req.GetTerms(), req.GetBinding(),
	)
	if err != nil {
		return nil, err
	}
	controller, err := oorbridge.New(r.server.actorSystem)
	if err != nil {
		return nil, err
	}
	result, err := controller.CommitPreparedOORResult(
		ctx, id, terms, binding,
	)
	if err != nil {
		return nil, err
	}
	if err := result.Validate(); err != nil {
		return nil, err
	}

	return &waverpc.CommitPreparedArkChannelOORResponse{
		Finalized: result.Finalized,
		Aborted:   result.Aborted,
		Reason:    result.Reason,
	}, nil
}

// AbortPreparedArkChannelOOR releases a prepared daemon OOR before PONR.
func (r *RPCServer) AbortPreparedArkChannelOOR(ctx context.Context,
	req *waverpc.AbortPreparedArkChannelOORRequest) (
	*waverpc.AbortPreparedArkChannelOORResponse, error) {

	if req.GetReason() == "" {
		return nil, fmt.Errorf("channel OOR abort reason is required")
	}
	id, terms, binding, err := r.channelOORControlRequest(
		req.GetChannelId(), req.GetTerms(), req.GetBinding(),
	)
	if err != nil {
		return nil, err
	}
	controller, err := oorbridge.New(r.server.actorSystem)
	if err != nil {
		return nil, err
	}
	result, err := controller.AbortPreparedOORResult(
		ctx, id, terms, binding, req.GetReason(),
	)
	if err != nil {
		return nil, err
	}
	if err := result.Validate(); err != nil {
		return nil, err
	}

	return &waverpc.AbortPreparedArkChannelOORResponse{
		Aborted: result.Aborted, Reason: result.Reason,
		Finalized: result.Finalized,
	}, nil
}

// channelOORRequest parses one immutable terms and binding pair.
func (r *RPCServer) channelOORRequest(termsRPC *arkchannelrpc.ChannelTerms,
	bindingRPC *arkchannelrpc.ChannelVTXOBinding) (arkchannel.Terms,
	arkchannel.VTXOBinding, error) {

	terms, err := lnruntime.ChannelTermsFromRPC(termsRPC)
	if err != nil {
		return arkchannel.Terms{}, arkchannel.VTXOBinding{}, err
	}
	binding, err := lnruntime.ChannelBindingFromRPC(bindingRPC)
	if err != nil {
		return arkchannel.Terms{}, arkchannel.VTXOBinding{}, err
	}
	if terms.Funder != arkchannel.PartyHub {
		return arkchannel.Terms{}, arkchannel.VTXOBinding{},
			fmt.Errorf("daemon does not fund this channel")
	}
	if err := binding.Validate(terms); err != nil {
		return arkchannel.Terms{}, arkchannel.VTXOBinding{}, err
	}

	return terms, binding, nil
}

// channelOORControlRequest additionally binds the request channel ID.
func (r *RPCServer) channelOORControlRequest(rawID []byte,
	termsRPC *arkchannelrpc.ChannelTerms,
	bindingRPC *arkchannelrpc.ChannelVTXOBinding) (arkchannel.ID,
	arkchannel.Terms, arkchannel.VTXOBinding, error) {

	terms, binding, err := r.channelOORRequest(termsRPC, bindingRPC)
	if err != nil {
		return arkchannel.ID{}, arkchannel.Terms{},
			arkchannel.VTXOBinding{}, err
	}
	if len(rawID) != len(terms.ID) {
		return arkchannel.ID{}, arkchannel.Terms{},
			arkchannel.VTXOBinding{}, fmt.Errorf("channel ID " +
				"must be 32 bytes")
	}
	var id arkchannel.ID
	copy(id[:], rawID)
	if id != terms.ID {
		return arkchannel.ID{}, arkchannel.Terms{},
			arkchannel.VTXOBinding{}, fmt.Errorf("channel ID " +
				"changed")
	}

	return id, terms, binding, nil
}
