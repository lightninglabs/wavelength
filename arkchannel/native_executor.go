package arkchannel

import (
	"context"
	"errors"
	"fmt"
)

// ActionExecutor performs one idempotent side effect after the channel FSM
// has durably committed the action that requested it.
type ActionExecutor interface {
	Execute(context.Context, ID, Action) error
}

// FundingNegotiator runs lnd's native external-funding exchange over the
// application peer transport and records completion through Service.Apply.
type FundingNegotiator interface {
	NegotiateChannel(context.Context, ID, Terms, VTXOBinding) error
}

// OORTransferController validates the prepared transfer that creates the exact
// channel-policy VTXO before native negotiation starts.
type OORTransferController interface {
	ValidatePreparedOOR(context.Context, Terms, VTXOBinding) error
}

// ValidatePreparedOOR proves that a binding names an output in the exact
// prepared OOR package before it can enter the durable channel FSM.
func (e *NativeExecutor) ValidatePreparedOOR(ctx context.Context, terms Terms,
	source VTXOBinding) error {

	if err := source.Validate(terms); err != nil {
		return err
	}
	if terms.Funder != e.localParty {
		return nil
	}
	if e.oor == nil {
		return fmt.Errorf("local OOR transfer controller is " +
			"unavailable")
	}

	return e.oor.ValidatePreparedOOR(ctx, terms, source)
}

// ChannelEventSink records trusted local subsystem completion back into the
// durable channel state machine.
type ChannelEventSink interface {
	ApplyLocalEvent(context.Context, ID, Event) (Record, error)
}

// ChannelEventSinkBinder wires an executor to its owning service after both
// have been constructed.
type ChannelEventSinkBinder interface {
	BindChannelEventSink(ChannelEventSink) error
}

// NativeExecutor routes durable Ark actions into native subsystems.
type NativeExecutor struct {
	localParty Party
	negotiator FundingNegotiator
	oor        OORTransferController
}

// NewNativeExecutor constructs the thin native subsystem adapter.
func NewNativeExecutor(localParty Party, negotiator FundingNegotiator,
	oor OORTransferController) (*NativeExecutor, error) {

	if localParty != PartyClient && localParty != PartyHub {
		return nil, fmt.Errorf("local channel party is required")
	}
	if negotiator == nil {
		return nil, fmt.Errorf("funding negotiator is required")
	}

	return &NativeExecutor{
		localParty: localParty, negotiator: negotiator, oor: oor,
	}, nil
}

// Execute dispatches one already-durable action without interpreting lnd
// channel or payment state.
func (e *NativeExecutor) Execute(ctx context.Context, id ID,
	action Action) error {

	switch action := action.(type) {
	case *NegotiateFunding:
		return e.negotiator.NegotiateChannel(
			ctx, id, action.Terms, action.Source,
		)

	default:
		return fmt.Errorf("unknown Ark channel action %T", action)
	}
}

// BindChannelEventSink connects native observations to the durable service.
func (e *NativeExecutor) BindChannelEventSink(sink ChannelEventSink) error {
	var bindErrors []error
	for _, component := range []any{e.negotiator, e.oor} {
		if component == nil {
			continue
		}
		binder, ok := component.(ChannelEventSinkBinder)
		if !ok {
			continue
		}
		if err := binder.BindChannelEventSink(sink); err != nil {
			bindErrors = append(bindErrors, err)
		}
	}

	return errors.Join(bindErrors...)
}
