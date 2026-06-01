package unroll

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

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

// TestRegistryRecoversCleanFailureEndToEnd is the in-process integration test
// for the darepo-client#602 recovery path. Unlike the unit tests above (which
// call handleTerminated directly with a synthetic UnrollTerminatedMsg), this
// drives a REAL child unroll actor through admission, proof submission, and a
// txconfirm broadcast rejection to terminal Failed, then asserts the registry
// computes HadOnChainFootprint=false from the real FSM/planner state and
// forwards ExitOutcomeRecoverable to the VTXO manager observer.
//
// This is the seam the unit tests cannot cover: the bug was an emergent
// lifecycle gap across the child actor → registry → observer chain, and the
// footprint determination is the change's highest-risk assumption. Here it
// runs against the real machinery rather than a hand-built message.
func TestRegistryRecoversCleanFailureEndToEnd(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())

	observer := &captureExitObserver{}
	var observerRef actor.TellOnlyRef[vtxo.ManagerMsg] = observer

	txconfirmRef := &fakeTxConfirmRef{}
	cfg := RegistryConfig{
		Store:         newMemRegistryStore(),
		DeliveryStore: newMemCheckpointStore(),
		ProofAssembler: &mockProofAssembler{
			proof: proof,
		},
		VTXOStore: &mockVTXOStore{
			desc: desc,
		},
		TxConfirmRef: txconfirmRef,
		ChainSource: &fakeRegistryChainSourceRef{
			height: 200,
		},
		Wallet:           &fakeSweepWallet{},
		VTXOExitObserver: fn.Some(observerRef),
	}

	registry := newRegistryHarnessWithSpawn(t, cfg)
	t.Cleanup(registry.Stop)

	// Reject the first proof tx the child submits (the #602 trigger: e.g. a
	// sub-dust proof tx that can't meet min relay fee). The child drives a
	// terminal TxFailedEvent with nothing confirmed and nothing left
	// in-flight, so the job is a clean failure with no on-chain footprint.
	txconfirmRef.setImmediateFailed(proof.RootTxids()[0], "min relay fee")

	_, err := registry.Ref().Ask(t.Context(), &EnsureUnrollRequest{
		Outpoint: proof.TargetOutpoint(),
		Trigger:  TriggerManual,
	}).Await(t.Context()).Unpack()
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return len(observer.notifications()) == 1
	}, testTimeout, 10*time.Millisecond,
		"registry should forward a recovery notification")

	notes := observer.notifications()
	require.Equal(t, proof.TargetOutpoint(), notes[0].Outpoint)
	require.Equal(
		t, vtxo.ExitOutcomeRecoverable, notes[0].Outcome,
		"a clean no-broadcast failure must be reported as recoverable",
	)
}

// TestRegistryFailedAdmissionNotifiesRecoverable verifies that a child whose
// start fails before any broadcast (failAdmittedChild) both persists a
// recoverable terminal record and notifies the VTXO manager to roll the VTXO
// back to live. These pre-broadcast failures move ownership to unilateral
// exit but leave no on-chain footprint, so they must be recoverable
// (darepo-client#602).
func TestRegistryFailedAdmissionNotifiesRecoverable(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	store := newMemRegistryStore()

	observer := &captureExitObserver{}
	var observerRef actor.TellOnlyRef[vtxo.ManagerMsg] = observer

	registry := newRegistryHarnessWithSpawn(t, RegistryConfig{
		Store:            store,
		DeliveryStore:    newMemCheckpointStore(),
		ProofAssembler:   &mockProofAssembler{proof: proof},
		VTXOStore:        &mockVTXOStore{desc: desc},
		TxConfirmRef:     &fakeTxConfirmRef{},
		ChainSource:      &fakeRegistryChainSourceRef{height: 200},
		Wallet:           &fakeSweepWallet{},
		VTXOExitObserver: fn.Some(observerRef),
	})
	t.Cleanup(registry.Stop)

	registry.behavior.spawnFunc = func(_ context.Context,
		target wire.OutPoint) (*VTXOUnrollActor, error) {

		behavior := actor.NewFunctionBehavior(
			func(_ context.Context, msg Msg) fn.Result[Resp] {
				switch msg.(type) {
				case *StartUnrollRequest:
					return fn.Err[Resp](
						errors.New("start boom"),
					)

				case *GetStateRequest:
					return fn.Ok[Resp](&GetStateResp{
						Started: false,
						Phase:   PhasePending,
					})

				default:
					return fn.Err[Resp](
						fmt.Errorf("unexpected msg %T",
							msg),
					)
				}
			},
		)

		// Test children are owned by t.Cleanup after creation.
		//nolint:contextcheck
		return newTestUnrollChild(t, target, behavior), nil
	}

	_, err := registry.Ref().Ask(t.Context(), &EnsureUnrollRequest{
		Outpoint: proof.TargetOutpoint(),
		Trigger:  TriggerManual,
	}).Await(t.Context()).Unpack()
	require.Error(t, err)

	// The pre-broadcast failure is reported to the manager as recoverable.
	require.Eventually(t, func() bool {
		return len(observer.notifications()) == 1
	}, testTimeout, 10*time.Millisecond,
		"failed admission should notify the manager")

	notes := observer.notifications()
	require.Equal(t, proof.TargetOutpoint(), notes[0].Outpoint)
	require.Equal(t, vtxo.ExitOutcomeRecoverable, notes[0].Outcome)

	// And the durable record is the recoverable terminal variant, so boot
	// reconciliation can recover it even if the notification is lost.
	record, err := store.GetRecord(t.Context(), proof.TargetOutpoint())
	require.NoError(t, err)
	require.NotNil(t, record)
	require.Equal(t, PhaseFailed, record.Phase)
	require.True(t, record.RecoverableFailure)
}
