package unroll

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightninglabs/wavelength/vtxo"
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
// recoverable exit so the VTXO is rolled back to live (wavelength#602).
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

// TestRegistryForwardsExitPolicyFromTerminalMsg verifies the exit policy the
// VTXO manager sees on a recoverable failure comes from the child's terminal
// message, not the registry's in-memory pending cache. The pending record is
// legitimately evicted once its async terminal persist completes
// (handlePersistRecordResult), so a recovery-only vHTLC target can reach
// handleTerminated with no cached record at all. If the policy were sourced
// from r.pending it would arrive empty, the manager's Valid() guard would miss,
// and the target would be relived to live: the exact wavelength#602 relive
// bug. Here the pending map starts empty and the terminal message carries the
// vHTLC refund policy, so the forwarded notification must still name it.
func TestRegistryForwardsExitPolicyFromTerminalMsg(t *testing.T) {
	target := wire.OutPoint{Hash: chainhash.Hash{4}, Index: 0}
	behavior, observer := newExitObserverRegistry(target)

	// Model the post-persist eviction window: the child is long gone from
	// both maps by the time its terminal handoff lands.
	delete(behavior.pending, target)

	const vhtlcRefund ExitPolicyKind = "vhtlc_refund_without_receiver"
	_, err := behavior.handleTerminated(t.Context(), &UnrollTerminatedMsg{
		Outpoint:            target,
		ActorID:             actorIDForTarget(target),
		Phase:               PhaseFailed,
		FailReason:          "min relay fee not met",
		HadOnChainFootprint: false,
		ExitPolicyKind:      vhtlcRefund,
	}).Unpack()
	require.NoError(t, err)

	notes := observer.notifications()
	require.Len(t, notes, 1)
	require.Equal(t, vtxo.ExitOutcomeRecoverable, notes[0].Outcome)
	require.Equal(
		t, actormsg.ExitPolicyVHTLCRefundWithoutReceiver,
		notes[0].ExitPolicyKind, "the recovery-only exit policy "+
			"must survive an evicted pending cache",
	)
	require.True(
		t, notes[0].ExitPolicyKind.Valid(),
		"a vHTLC policy must pass the manager's hold-in-exit guard",
	)

	// The message's kind reaches the manager but must NOT be stamped onto
	// the persisted terminal record: the message has no policy ref, so a
	// stamped kind would overwrite the store's (kind, ref) identity with
	// (kind, ""). The no-cache record therefore leaves the policy empty,
	// letting registryExitPolicy preserve the durable admission identity.
	persisted, ok := behavior.pending[target]
	require.True(t, ok)
	require.Empty(
		t, persisted.ExitPolicyKind, "the terminal record must not "+
			"carry a ref-less kind that clobbers the store "+
			"identity",
	)
	require.Empty(t, persisted.ExitPolicyRef)
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
// for the wavelength#602 recovery path. Unlike the unit tests above (which
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
// (wavelength#602).
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

// TestRegistryReadmitsTargetAfterRecoverableFailure verifies that once a VTXO
// has been rolled back to live by a recoverable (no-footprint) unroll failure,
// a fresh exit for the same outpoint is admitted rather than deduped against
// the dead recoverable record. Without this, the wavelength#602 fix would
// trade a guaranteed strand for a deferred one: the VTXO would recover to live
// but could never be unrolled again, because the terminal record blocks every
// subsequent admission. This exercises the in-memory pending-cache arm of the
// dedup gate (the record lingers in r.pending after failAdmittedChild).
func TestRegistryReadmitsTargetAfterRecoverableFailure(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	target := proof.TargetOutpoint()
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

	// The first spawn fails its start (pre-broadcast admission failure,
	// the #602 trigger); the second spawn is a healthy child. A counter
	// distinguishes the two admissions deterministically.
	var spawnCount int
	registry.behavior.spawnFunc = func(_ context.Context,
		spawnTarget wire.OutPoint) (*VTXOUnrollActor, error) {

		spawnCount++
		firstSpawn := spawnCount == 1
		childBehavior := actor.NewFunctionBehavior(
			func(_ context.Context, msg Msg) fn.Result[Resp] {
				switch msg.(type) {
				case *StartUnrollRequest:
					if firstSpawn {
						return fn.Err[Resp](
							errors.New(
								"start boom",
							),
						)
					}

					return fn.Ok[Resp](&AckResp{})

				case *GetStateRequest:
					return fn.Ok[Resp](&GetStateResp{
						Started: !firstSpawn,
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
		return newTestUnrollChild(t, spawnTarget, childBehavior), nil
	}

	// First admission fails pre-broadcast: failAdmittedChild records a
	// recoverable terminal row, leaves it in r.pending, and notifies the
	// manager to roll the VTXO back to live.
	_, err := registry.Ref().Ask(t.Context(), &EnsureUnrollRequest{
		Outpoint: target,
		Trigger:  TriggerManual,
	}).Await(t.Context()).Unpack()
	require.Error(t, err)

	require.Eventually(t, func() bool {
		return len(observer.notifications()) == 1
	}, testTimeout, 10*time.Millisecond,
		"failed admission should roll the VTXO back to live")

	pre, err := store.GetRecord(t.Context(), target)
	require.NoError(t, err)
	require.NotNil(t, pre)
	require.True(
		t, pre.RecoverableFailure,
		"the failed admission must leave a recoverable terminal record",
	)

	// Second admission for the SAME outpoint must re-admit (Created=true)
	// rather than dedup against the recoverable record.
	resp, err := registry.Ref().Ask(t.Context(), &EnsureUnrollRequest{
		Outpoint: target,
		Trigger:  TriggerManual,
	}).Await(t.Context()).Unpack()
	require.NoError(t, err)

	ensureResp, ok := resp.(*EnsureUnrollResp)
	require.True(t, ok)
	require.True(
		t, ensureResp.Created, "a recovered VTXO must be "+
			"re-admittable, not deduped against the dead "+
			"recoverable record",
	)
	require.Equal(
		t, 2, spawnCount, "the second Ensure must spawn a fresh child",
	)

	// The stale recoverable record is overwritten by the fresh admission,
	// so the VTXO can progress through a new exit.
	post, err := store.GetRecord(t.Context(), target)
	require.NoError(t, err)
	require.NotNil(t, post)
	require.False(
		t, post.RecoverableFailure,
		"re-admission must overwrite the stale recoverable record",
	)
	require.Equal(t, PhasePending, post.Phase)
}

// TestRegistryReadmitsTargetAfterRecoverableFailureAcrossRestart verifies the
// durable arm of the re-admission path: after a restart, the in-memory pending
// cache is empty, so the only surviving artifact of a recovered exit is the
// FailedRecoverable row on disk. A fresh Ensure must re-admit through the
// Store.GetRecord branch rather than dedup against that terminal record. This
// is the boot-time companion to TestRegistryReadmitsTargetAfterRecoverable
// Failure (which covers the same-process r.pending arm).
func TestRegistryReadmitsTargetAfterRecoverableFailureAcrossRestart(
	t *testing.T) {

	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	target := proof.TargetOutpoint()
	store := newMemRegistryStore()

	// Seed the post-recovery durable state a restart would leave behind: a
	// FailedRecoverable terminal record on disk, with no in-memory registry
	// state (the process restarted). Boot reconciliation has already rolled
	// the VTXO back to live; this stale record is the only artifact left.
	err := store.MarkTerminal(
		t.Context(), target, PhaseFailed, true, "min relay fee", nil,
	)
	require.NoError(t, err)

	seeded, err := store.GetRecord(t.Context(), target)
	require.NoError(t, err)
	require.NotNil(t, seeded)
	require.True(t, seeded.IsTerminal())
	require.True(t, seeded.RecoverableFailure)

	observer := &captureExitObserver{}
	var observerRef actor.TellOnlyRef[vtxo.ManagerMsg] = observer

	// A fresh registry over the seeded store models the post-restart
	// process: r.active and r.pending start empty.
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

	// The Ensure must re-admit through the durable GetRecord path, spawning
	// a new child rather than returning the historical ActorID with
	// Created=false.
	resp, err := registry.Ref().Ask(t.Context(), &EnsureUnrollRequest{
		Outpoint: target,
		Trigger:  TriggerManual,
	}).Await(t.Context()).Unpack()
	require.NoError(t, err)

	ensureResp, ok := resp.(*EnsureUnrollResp)
	require.True(t, ok)
	require.True(
		t, ensureResp.Created, "after restart, a recovered VTXO "+
			"must re-admit via the durable record path",
	)

	// The terminal recoverable row is overwritten with a fresh pending
	// record so the new exit can make progress.
	post, err := store.GetRecord(t.Context(), target)
	require.NoError(t, err)
	require.NotNil(t, post)
	require.False(
		t, post.RecoverableFailure,
		"re-admission must overwrite the stale recoverable record",
	)
}
