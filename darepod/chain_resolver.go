package darepod

import (
	"context"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/unroller"
	"github.com/lightninglabs/darepo-client/vtxo"
)

// chainResolverAdapter bridges vtxo.ExpiringNotification messages to
// unroller.UnrollRequest messages. The VTXO actor sends an
// ExpiringNotification when a VTXO is critically close to expiry. This
// adapter converts it into an UnrollRequest targeting the VTXO's
// outpoint and forwards it to the unroller actor.
//
// It implements actor.TellOnlyRef[vtxo.ExpiringNotification] so it can
// be passed as the ChainResolver in vtxo.ManagerConfig.
type chainResolverAdapter struct {
	unrollerRef actor.TellOnlyRef[unroller.UnrollerMsg]
}

// newChainResolverAdapter creates a new adapter that forwards expiring
// notifications to the given unroller actor ref.
func newChainResolverAdapter(
	unrollerRef actor.TellOnlyRef[unroller.UnrollerMsg],
) *chainResolverAdapter {

	return &chainResolverAdapter{
		unrollerRef: unrollerRef,
	}
}

// ID returns the adapter's identifier for the actor framework.
func (a *chainResolverAdapter) ID() string {
	return "chain-resolver-adapter"
}

// Tell converts the ExpiringNotification into an UnrollRequest and
// forwards it to the unroller. The VTXO descriptor's Outpoint is
// mapped to the UnrollRequest's TargetVTXOs slice.
func (a *chainResolverAdapter) Tell(ctx context.Context,
	msg vtxo.ExpiringNotification) error {

	unrollReq := &unroller.UnrollRequest{
		TargetVTXOs: []wire.OutPoint{msg.VTXO.Outpoint},
	}

	return a.unrollerRef.Tell(ctx, unrollReq)
}

// Compile-time check that chainResolverAdapter implements
// actor.TellOnlyRef[vtxo.ExpiringNotification].
//
//nolint:ll
var _ actor.TellOnlyRef[vtxo.ExpiringNotification] = (*chainResolverAdapter)(nil)
