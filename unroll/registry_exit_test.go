package unroll

import (
	"context"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/vtxo"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// captureExitObserver records the ExitOutcomeNotifications a registry forwards
// to the VTXO manager. It implements actor.TellOnlyRef[vtxo.ManagerMsg].
type captureExitObserver struct {
	mu   sync.Mutex
	msgs []*vtxo.ExitOutcomeNotification
}

// ID implements actor.BaseActorRef.
func (c *captureExitObserver) ID() string { return "capture-exit-observer" }

// Tell records the forwarded exit-outcome notification.
func (c *captureExitObserver) Tell(_ context.Context,
	msg vtxo.ManagerMsg) error {

	c.mu.Lock()
	defer c.mu.Unlock()

	if n, ok := msg.(*vtxo.ExitOutcomeNotification); ok {
		c.msgs = append(c.msgs, n)
	}

	return nil
}

// notifications returns a copy of the captured notifications.
func (c *captureExitObserver) notifications() []*vtxo.ExitOutcomeNotification {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := make([]*vtxo.ExitOutcomeNotification, len(c.msgs))
	copy(out, c.msgs)

	return out
}

// newExitObserverRegistry builds a registry behavior wired to capture exit
// notifications, with no active child so handleTerminated runs synchronously.
func newExitObserverRegistry(target wire.OutPoint) (*registryBehavior,
	*captureExitObserver) {

	observer := &captureExitObserver{}
	var observerRef actor.TellOnlyRef[vtxo.ManagerMsg] = observer
	behavior := &registryBehavior{
		cfg: RegistryConfig{
			Store:            newMemRegistryStore(),
			VTXOExitObserver: fn.Some(observerRef),
		},
		selfRef: noopRegistryTellRef{},
		active:  make(map[wire.OutPoint]*VTXOUnrollActor),
		pending: map[wire.OutPoint]RegistryRecord{
			target: {
				TargetOutpoint: target,
				ActorID:        actorIDForTarget(target),
				Trigger:        TriggerManual,
				Phase:          PhasePending,
			},
		},
		persisting: make(map[wire.OutPoint]RegistryRecord),
	}

	return behavior, observer
}

// TestRegistryForwardsCleanFailureAsRecoverable verifies a terminal failure
// with no on-chain footprint is forwarded to the VTXO manager as a
// recoverable exit so the VTXO is rolled back to live (darepo-client#602).
func TestRegistryForwardsCleanFailureAsRecoverable(t *testing.T) {
	target := wire.OutPoint{Hash: chainhash.Hash{1}, Index: 0}
	behavior, observer := newExitObserverRegistry(target)

	_, err := behavior.handleTerminated(t.Context(), &UnrollTerminatedMsg{
		Outpoint:            target,
		ActorID:             actorIDForTarget(target),
		Phase:               PhaseFailed,
		FailReason:          "min relay fee not met",
		HadOnChainFootprint: false,
	}).Unpack()
	require.NoError(t, err)

	notes := observer.notifications()
	require.Len(t, notes, 1)
	require.Equal(t, target, notes[0].Outpoint)
	require.Equal(t, vtxo.ExitOutcomeRecoverable, notes[0].Outcome)
	require.Equal(t, "min relay fee not met", notes[0].Reason)
}

// TestRegistryDoesNotRecoverFailureWithFootprint verifies a terminal failure
// that already broadcast on-chain is NOT forwarded as recoverable: the exit
// has begun on-chain and the VTXO must stay in unilateral-exit.
func TestRegistryDoesNotRecoverFailureWithFootprint(t *testing.T) {
	target := wire.OutPoint{Hash: chainhash.Hash{2}, Index: 0}
	behavior, observer := newExitObserverRegistry(target)

	_, err := behavior.handleTerminated(t.Context(), &UnrollTerminatedMsg{
		Outpoint:            target,
		ActorID:             actorIDForTarget(target),
		Phase:               PhaseFailed,
		FailReason:          "sweep rejected after broadcast",
		HadOnChainFootprint: true,
	}).Unpack()
	require.NoError(t, err)

	require.Empty(
		t, observer.notifications(),
		"a failure with an on-chain footprint must not be recovered",
	)
}

// TestRegistryForwardsCompletionAsConfirmed verifies a completed exit is
// forwarded to the VTXO manager so the VTXO is retired to spent.
func TestRegistryForwardsCompletionAsConfirmed(t *testing.T) {
	target := wire.OutPoint{Hash: chainhash.Hash{3}, Index: 0}
	behavior, observer := newExitObserverRegistry(target)

	_, err := behavior.handleTerminated(t.Context(), &UnrollTerminatedMsg{
		Outpoint: target,
		ActorID:  actorIDForTarget(target),
		Phase:    PhaseCompleted,
	}).Unpack()
	require.NoError(t, err)

	notes := observer.notifications()
	require.Len(t, notes, 1)
	require.Equal(t, target, notes[0].Outpoint)
	require.Equal(t, vtxo.ExitOutcomeConfirmed, notes[0].Outcome)
}
