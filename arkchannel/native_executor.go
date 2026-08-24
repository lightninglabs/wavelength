package arkchannel

import (
	"context"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
)

// ActionExecutor performs one idempotent side effect after the channel FSM
// has durably committed the action that requested it.
type ActionExecutor interface {
	Execute(context.Context, ID, Action) error
}

// VirtualFundingActivator exposes lnd's synthetic funding confirmation.
type VirtualFundingActivator interface {
	ConfirmBacking(chainhash.Hash) error
}

// FundingNegotiator runs lnd's native external-funding exchange over the
// application peer transport and records completion through Service.Apply.
type FundingNegotiator interface {
	NegotiateChannel(context.Context, ID, Terms, VTXOBinding) error

	PrepareChannelRecovery(context.Context, ID, Terms, VTXOBinding) error

	CancelChannel(context.Context, ID, Terms, VTXOBinding, *Backing,
		string) error
}

// OORTransferController commits or aborts the prepared transfer that creates
// the exact channel-policy VTXO.
type OORTransferController interface {
	ValidatePreparedOOR(context.Context, Terms, VTXOBinding) error

	CommitPreparedOOR(context.Context, ID, Terms, VTXOBinding) error

	AbortPreparedOOR(context.Context, ID, Terms, VTXOBinding, string) error
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

// ChannelEventSink records an external subsystem completion back into the
// durable channel state machine.
type ChannelEventSink interface {
	Apply(context.Context, ID, Event) (Record, error)
}

// ChannelEventSinkBinder wires an executor to its owning service after both
// have been constructed.
type ChannelEventSinkBinder interface {
	BindChannelEventSink(ChannelEventSink) error
}

// ChannelMaterializer publishes the Ark ancestry before the signed backing.
type ChannelMaterializer interface {
	MaterializeChannel(context.Context, ID, Terms, VTXOBinding,
		Backing) error
}

// ChannelOnchainHandoff verifies lnd's standard watcher, arbitrator, and
// resolver lifecycle is armed before backing publication is allowed.
type ChannelOnchainHandoff interface {
	HandoffChannel(wire.OutPoint) error
}

// ChannelForceCloser resumes native lnd's commitment publication from a
// durable Ark channel action.
type ChannelForceCloser interface {
	ResumeForceCloseChannel(wire.OutPoint) error
}

// ChannelCooperativeCloser coordinates clean lnd state, a 3-of-3 OOR close,
// and local lnd archival across paired endpoints.
type ChannelCooperativeCloser interface {
	NegotiateCooperativeClose(context.Context, ID, Terms, VTXOBinding,
		Backing, CooperativeCloseRequest) error

	PublishCooperativeClose(context.Context, ID, Terms, VTXOBinding,
		CooperativeClose) error

	FinalizeCooperativeClose(context.Context, ID, Terms, Backing,
		VTXOBinding, CooperativeCloseRequest, CooperativeClose) error
}

// NativeExecutor routes durable Ark actions into native lnd and the unroller.
type NativeExecutor struct {
	localParty   Party
	funding      VirtualFundingActivator
	negotiator   FundingNegotiator
	oor          OORTransferController
	materializer ChannelMaterializer
	handoff      ChannelOnchainHandoff
	forceCloser  ChannelForceCloser
	closer       ChannelCooperativeCloser
}

// NewNativeExecutor constructs the thin native subsystem adapter.
func NewNativeExecutor(localParty Party, funding VirtualFundingActivator,
	negotiator FundingNegotiator, oor OORTransferController,
	materializer ChannelMaterializer, handoff ChannelOnchainHandoff,
	forceCloser ChannelForceCloser,
	closer ChannelCooperativeCloser) (*NativeExecutor, error) {

	if localParty != PartyClient && localParty != PartyHub {
		return nil, fmt.Errorf("local channel party is required")
	}
	if funding == nil {
		return nil, fmt.Errorf("virtual funding activator is required")
	}
	if negotiator == nil {
		return nil, fmt.Errorf("funding negotiator is required")
	}
	if closer == nil {
		return nil, fmt.Errorf("channel cooperative closer is required")
	}
	if handoff == nil {
		return nil, fmt.Errorf("channel on-chain handoff is required")
	}
	if forceCloser == nil {
		return nil, fmt.Errorf("channel force closer is required")
	}

	return &NativeExecutor{
		localParty:   localParty,
		funding:      funding,
		negotiator:   negotiator,
		oor:          oor,
		materializer: materializer,
		handoff:      handoff,
		forceCloser:  forceCloser,
		closer:       closer,
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

	case *CommitOOR:
		if action.Terms.Funder != e.localParty {
			return nil
		}
		if e.oor == nil {
			return fmt.Errorf("local OOR transfer controller is " +
				"unavailable")
		}

		return e.oor.CommitPreparedOOR(
			ctx, id, action.Terms, action.Source,
		)

	case *PrepareRecovery:
		return e.negotiator.PrepareChannelRecovery(
			ctx, id, action.Terms, action.Source,
		)

	case *AbortOOR:
		if action.Terms.Funder != e.localParty {
			return nil
		}
		if e.oor == nil {
			return fmt.Errorf("local OOR transfer controller is " +
				"unavailable")
		}

		return e.oor.AbortPreparedOOR(
			ctx, id, action.Terms, action.Source, action.Reason,
		)

	case *ActivateChannel:
		return e.funding.ConfirmBacking(
			action.Backing.ChannelPoint.Hash,
		)

	case *CancelFunding:
		return e.negotiator.CancelChannel(
			ctx, id, action.Terms, action.Source, action.Backing,
			action.Reason,
		)

	case *PublishChannel:
		if err := e.handoff.HandoffChannel(
			action.Backing.ChannelPoint,
		); err != nil {
			return err
		}
		if e.materializer == nil {
			return fmt.Errorf("channel materializer is unavailable")
		}

		return e.materializer.MaterializeChannel(
			ctx, id, action.Terms, action.Source, action.Backing,
		)

	case *ForceCloseChannel:
		return e.forceCloser.ResumeForceCloseChannel(
			action.Backing.ChannelPoint,
		)

	case *NegotiateCooperativeClose:
		return e.closer.NegotiateCooperativeClose(
			ctx, id, action.Terms, action.Source, action.Backing,
			action.Request,
		)

	case *PublishCooperativeClose:
		return e.closer.PublishCooperativeClose(
			ctx, id, action.Terms, action.Source, action.Close,
		)

	case *FinalizeCooperativeClose:
		return e.closer.FinalizeCooperativeClose(
			ctx, id, action.Terms, action.Backing, action.Source,
			action.Request, action.Close,
		)

	default:
		return fmt.Errorf("unknown Ark channel action %T", action)
	}
}

// BindChannelEventSink connects native funding and OOR terminal observations
// to the same durable channel service.
func (e *NativeExecutor) BindChannelEventSink(sink ChannelEventSink) error {
	var bindErrors []error
	for _, component := range []any{
		e.negotiator, e.oor, e.materializer, e.closer,
	} {
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

var _ ActionExecutor = (*NativeExecutor)(nil)
var _ ChannelEventSinkBinder = (*NativeExecutor)(nil)
