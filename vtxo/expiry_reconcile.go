package vtxo

import (
	"context"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
)

// expiryReconcilePlan owns a snapshot of actor references, not the manager's
// mutable registry. Child FSMs still serialize and persist all transitions.
// The startup caller executes the plan once while the manager drains relays
// and termination notifications. No worker reads or mutates manager state.
type expiryReconcilePlan struct {
	actors  map[wire.OutPoint]VTXOActorRef
	store   VTXOStore
	log     btclog.Logger
	timeout time.Duration
	epoch   BlockEpochEvent
}

// VTXOManagerResp marks the private startup preparation response.
func (*expiryReconcilePlan) VTXOManagerResp() {}

// ReconcileExpiry applies the current tip after round registration, outside
// the manager's receive loop so child-to-manager sends cannot block the pass.
// It must be called once by daemon startup, not from an actor that the manager
// or its children need to contact. The caller waits for completion and owns
// cancellation; each child ask retains the configured forfeit timeout.
func ReconcileExpiry(ctx context.Context,
	manager actor.ActorRef[ManagerMsg, ManagerResp]) (
	*ReconcileExpiryResponse, error) {

	response, err := manager.Ask(ctx, &ReconcileExpiryRequest{}).
		Await(ctx).Unpack()
	if err != nil {
		return nil, fmt.Errorf("prepare VTXO expiry reconcile: %w", err)
	}
	plan, ok := response.(*expiryReconcilePlan)
	if !ok {
		return nil, fmt.Errorf("unexpected VTXO expiry plan: %T",
			response)
	}

	return plan.run(ctx).Unpack()
}
