package arkchannel

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
)

// VirtualFundingActivator exposes lnd's synthetic funding confirmation.
type VirtualFundingActivator interface {
	ConfirmBacking(chainhash.Hash) error
}

// FundingNegotiator runs lnd's native external-funding exchange over the
// application peer transport and records completion through Service.Apply.
type FundingNegotiator interface {
	NegotiateChannel(context.Context, ID, Terms, VTXOBinding) error

	CancelChannel(context.Context, ID, Terms) error
}

// ChannelMaterializer publishes the Ark ancestry before the signed backing.
type ChannelMaterializer interface {
	MaterializeChannel(context.Context, ID, VTXOBinding, Backing) error
}

// NativeExecutor routes durable Ark actions into native lnd and the unroller.
type NativeExecutor struct {
	funding      VirtualFundingActivator
	negotiator   FundingNegotiator
	materializer ChannelMaterializer
}

// NewNativeExecutor constructs the thin native subsystem adapter.
func NewNativeExecutor(funding VirtualFundingActivator,
	negotiator FundingNegotiator,
	materializer ChannelMaterializer) (*NativeExecutor, error) {

	if funding == nil {
		return nil, fmt.Errorf("virtual funding activator is required")
	}
	if negotiator == nil {
		return nil, fmt.Errorf("funding negotiator is required")
	}
	if materializer == nil {
		return nil, fmt.Errorf("channel materializer is required")
	}

	return &NativeExecutor{
		funding:      funding,
		negotiator:   negotiator,
		materializer: materializer,
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

	case *ActivateChannel:
		return e.funding.ConfirmBacking(
			action.Backing.ChannelPoint.Hash,
		)

	case *CancelFunding:
		return e.negotiator.CancelChannel(ctx, id, action.Terms)

	case *PublishChannel:
		return e.materializer.MaterializeChannel(
			ctx, id, action.Source, action.Backing,
		)

	default:
		return fmt.Errorf("unknown Ark channel action %T", action)
	}
}

var _ ActionExecutor = (*NativeExecutor)(nil)
