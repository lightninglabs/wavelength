package waved

import (
	"context"
	"fmt"

	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/arkchannel/oorbridge"
	"github.com/lightninglabs/wavelength/lnruntime"
	"github.com/lightninglabs/wavelength/rpc/arkchannelrpc"
	"github.com/lightninglabs/wavelength/waverpc"
)

// channelOORResultSink captures the terminal event from a detached daemon OOR
// controller. The swapserver adapter records the same fact in its channel FSM.
type channelOORResultSink struct {
	id        arkchannel.ID
	finalized bool
	aborted   string
}

// Apply captures only the terminal events emitted by prepared OOR control.
func (s *channelOORResultSink) Apply(_ context.Context, id arkchannel.ID,
	event arkchannel.Event) (arkchannel.Record, error) {

	if id != s.id {
		return arkchannel.Record{}, fmt.Errorf("OOR callback channel " +
			"changed")
	}
	switch event := event.(type) {
	case *arkchannel.OORFinalized:
		s.finalized = true

	case *arkchannel.OORAborted:
		s.aborted = event.Reason

	default:
		return arkchannel.Record{}, fmt.Errorf("unexpected OOR "+
			"callback %T", event)
	}

	return arkchannel.Record{}, nil
}

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
		ctx, terms, defaultArkChannelBackingFee,
	)
	if err != nil {
		return nil, err
	}

	return &waverpc.PrepareArkChannelOORResponse{
		Binding: lnruntime.ChannelBindingToRPC(binding),
	}, nil
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
	controller, sink, err := r.detachedChannelOORController(id)
	if err != nil {
		return nil, err
	}
	if err := controller.CommitPreparedOOR(
		ctx, id, terms, binding,
	); err != nil {
		return nil, err
	}
	if !sink.finalized {
		return nil, fmt.Errorf("prepared channel OOR did not finalize")
	}

	return &waverpc.CommitPreparedArkChannelOORResponse{
		Finalized: true,
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
	controller, sink, err := r.detachedChannelOORController(id)
	if err != nil {
		return nil, err
	}
	if err := controller.AbortPreparedOOR(
		ctx, id, terms, binding, req.GetReason(),
	); err != nil {
		return nil, err
	}
	if sink.aborted == "" {
		return nil, fmt.Errorf("prepared channel OOR did not abort")
	}

	return &waverpc.AbortPreparedArkChannelOORResponse{
		Aborted: true, Reason: sink.aborted,
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

// detachedChannelOORController constructs a controller whose callback is
// returned to this RPC instead of being applied to an unrelated local channel.
func (r *RPCServer) detachedChannelOORController(id arkchannel.ID) (
	*oorbridge.Controller, *channelOORResultSink, error) {

	controller, err := oorbridge.New(r.server.actorSystem)
	if err != nil {
		return nil, nil, err
	}
	sink := &channelOORResultSink{id: id}
	if err := controller.BindChannelEventSink(sink); err != nil {
		return nil, nil, err
	}

	return controller, sink, nil
}
