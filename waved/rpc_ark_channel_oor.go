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
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if terms.Funder != arkchannel.PartyHub ||
		terms.Kind != arkchannel.KindReceiveIntent {
		return nil, status.Error(
			codes.InvalidArgument,
			"daemon channel OOR must fund a receive intent",
		)
	}
	binding, err := r.server.prepareArkChannelOOR(
		ctx, terms, arkchannel.DefaultBackingFee,
	)
	if err != nil {
		if errors.Is(err, arkchannel.ErrOORPreparationAmbiguous) {
			return nil, status.Error(codes.Aborted, err.Error())
		}

		return nil, status.Errorf(codes.FailedPrecondition, "prepare "+
			"channel OOR: %v", err)
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
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if terms.Funder != arkchannel.PartyHub ||
		terms.Kind != arkchannel.KindReceiveIntent {
		return nil, status.Error(
			codes.InvalidArgument,
			"daemon channel OOR must fund a receive intent",
		)
	}
	lookup, err := r.server.lookupArkChannelOOR(
		ctx, terms, arkchannel.DefaultBackingFee,
	)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "lookup "+
			"channel OOR: %v", err)
	}

	response := &waverpc.LookupPreparedArkChannelOORResponse{}
	switch lookup.Status {
	case oorbridge.PreparationAbsent:
	case oorbridge.PreparationPending:
	case oorbridge.PreparationPrepared:
		response.Binding = lnruntime.ChannelBindingToRPC(lookup.Binding)

	case oorbridge.PreparationAccepted:
	default:
		return nil, status.Errorf(codes.Internal, "unknown channel "+
			"OOR preparation status %d", lookup.Status)
	}
	response.Status = channelOORPreparationStatus(lookup.Status)

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
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	controller, err := oorbridge.New(r.server.actorSystem)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "initialize "+
			"channel OOR: %v", err)
	}
	if err := controller.ValidatePreparedOOR(
		ctx, terms, binding,
	); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "validate "+
			"channel OOR: %v", err)
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
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	controller, err := oorbridge.New(r.server.actorSystem)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "initialize "+
			"channel OOR: %v", err)
	}
	result, err := controller.CommitPreparedOORResult(
		ctx, id, terms, binding,
	)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "commit "+
			"channel OOR: %v", err)
	}
	if err := result.Validate(); err != nil {
		return nil, status.Errorf(codes.Internal, "invalid channel "+
			"OOR result: %v", err)
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
		return nil, status.Error(
			codes.InvalidArgument,
			"channel OOR abort reason is required",
		)
	}
	id, terms, binding, err := r.channelOORControlRequest(
		req.GetChannelId(), req.GetTerms(), req.GetBinding(),
	)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	controller, err := oorbridge.New(r.server.actorSystem)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "initialize "+
			"channel OOR: %v", err)
	}
	result, err := controller.AbortPreparedOORResult(
		ctx, id, terms, binding, req.GetReason(),
	)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "abort "+
			"channel OOR: %v", err)
	}
	if err := result.Validate(); err != nil {
		return nil, status.Errorf(codes.Internal, "invalid channel "+
			"OOR result: %v", err)
	}

	return &waverpc.AbortPreparedArkChannelOORResponse{
		Aborted: result.Aborted, Reason: result.Reason,
		Finalized: result.Finalized,
	}, nil
}

// arkChannelOORStatus shortens the generated protocol type.
type arkChannelOORStatus = waverpc.ArkChannelOORPreparationStatus

// channelOORPreparationStatus maps every internal state explicitly so future
// enum insertions cannot silently change the mailbox protocol.
func channelOORPreparationStatus(
	preparation oorbridge.PreparationStatus) arkChannelOORStatus {

	const (
		unspecified waverpc.ArkChannelOORPreparationStatus = 0
		absent      waverpc.ArkChannelOORPreparationStatus = 1
		pending     waverpc.ArkChannelOORPreparationStatus = 2
		prepared    waverpc.ArkChannelOORPreparationStatus = 3
		accepted    waverpc.ArkChannelOORPreparationStatus = 4
	)

	switch preparation {
	case oorbridge.PreparationAbsent:
		return absent

	case oorbridge.PreparationPending:
		return pending

	case oorbridge.PreparationPrepared:
		return prepared

	case oorbridge.PreparationAccepted:
		return accepted

	default:
		return unspecified
	}
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
