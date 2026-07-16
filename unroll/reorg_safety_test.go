package unroll

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/txconfirm"
	"github.com/lightninglabs/wavelength/unrollplan"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestProofRootReorgBlocksDownstreamMaterialization drives a proof root
// through Confirmed -> Reorged -> Confirmed and asserts that the unroll
// actor does NOT submit the target transaction while the root anchor is
// torn down. Only once the root reconfirms does the actor advance the
// proof graph.
//
// This is the load-bearing reorg-safety case: before the rollback work
// the actor treated the first TxConfirmedMsg as a monotonic fact and
// would have submitted the target while the chain was still missing the
// root. After rollback, the target submission is gated on the root
// anchor being live.
func TestProofRootReorgBlocksDownstreamMaterialization(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	unrollActor, _, txconfirmRef, _ := newActorHarness(t, proof, desc)

	mustAsk(t, unrollActor.Ref(), &StartUnrollRequest{
		Height:  100,
		Trigger: TriggerManual,
	})

	rootTxid := proof.RootTxids()[0]
	targetTxid := proof.TargetOutpoint().Hash

	// Wait for the actor to register the root with txconfirm.
	require.Eventually(t, func() bool {
		return txconfirmRef.requestCountForTxid(rootTxid) == 1
	}, testTimeout, 10*time.Millisecond, "root never submitted")

	// 1. Root confirms; the actor should immediately submit the target.
	txconfirmRef.emitConfirmed(t, 0, rootTxid, 101)
	require.Eventually(t, func() bool {
		return txconfirmRef.requestCountForTxid(targetTxid) == 1
	}, testTimeout, 10*time.Millisecond, "target never submitted")

	// 2. Root reorgs out. The actor must drop the root anchor AND the
	// dependent target anchor from in-flight, since State.Validate
	// requires every in-flight node to have confirmed parents in the
	// proof graph.
	targetSubmissionsBeforeReorg := txconfirmRef.requestCountForTxid(
		targetTxid,
	)
	txconfirmRef.emitReorged(t, 0, rootTxid)

	require.Eventually(t, func() bool {
		resp, ok := mustAsk(
			t, unrollActor.Ref(), &GetStateRequest{},
		).(*GetStateResp)
		require.True(t, ok)
		for _, h := range resp.PlannerState.ConfirmedTxids {
			if h == rootTxid {
				return false
			}
		}
		for _, h := range resp.PlannerState.InFlightTxids {
			if h == targetTxid {
				return false
			}
		}

		return true
	}, testTimeout, 10*time.Millisecond,
		"root + target anchors never cleared after reorg")

	// The actor naturally re-submits the root on its ready frontier
	// after the reorg drops it from ConfirmedTxids — txconfirm dedup
	// absorbs that re-submit. What matters for reorg safety is that the
	// TARGET was NOT re-submitted while its parent anchor was missing:
	// driving target broadcast off a non-anchored parent is exactly the
	// unsafe behavior this work fixes.
	time.Sleep(50 * time.Millisecond)
	require.Equal(
		t, targetSubmissionsBeforeReorg,
		txconfirmRef.requestCountForTxid(targetTxid),
		"target was re-submitted while its parent was reorged out",
	)

	// 3. Root reconfirms in a different block. The actor should keep
	// progressing without resubmitting target (txconfirm dedup already
	// holds the in-flight subscription, but the planner state must be
	// coherent).
	txconfirmRef.emitConfirmed(t, 0, rootTxid, 102)
	require.Eventually(t, func() bool {
		resp, ok := mustAsk(
			t, unrollActor.Ref(), &GetStateRequest{},
		).(*GetStateResp)
		require.True(t, ok)
		for _, h := range resp.PlannerState.ConfirmedTxids {
			if h == rootTxid {
				return true
			}
		}

		return false
	}, testTimeout, 10*time.Millisecond,
		"root anchor never re-recorded after reconfirm")
}

// TestTargetReorgClearsCSVMaturity confirms the target tx, waits for the
// sweep to broadcast, then reorgs the target out. The actor must clear
// TargetConfirmHeight and downgrade the sweep so the planner stops
// reporting Done.
func TestTargetReorgClearsCSVMaturity(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	unrollActor, _, txconfirmRef, _ := newActorHarness(t, proof, desc)

	mustAsk(t, unrollActor.Ref(), &StartUnrollRequest{
		Height:  100,
		Trigger: TriggerManual,
	})

	rootTxid := proof.RootTxids()[0]
	targetTxid := proof.TargetOutpoint().Hash

	require.Eventually(t, func() bool {
		return txconfirmRef.requestCountForTxid(rootTxid) == 1
	}, testTimeout, 10*time.Millisecond)

	txconfirmRef.emitConfirmed(t, 0, rootTxid, 101)
	require.Eventually(t, func() bool {
		return txconfirmRef.requestCountForTxid(targetTxid) == 1
	}, testTimeout, 10*time.Millisecond)

	txconfirmRef.emitConfirmed(t, 1, targetTxid, 102)

	// Advance height past CSV maturity so the sweep is built and
	// broadcast.
	mustAsk(t, unrollActor.Ref(), &HeightObservedMsg{
		Height: 102 + int32(proof.CSVDelay()) + 1,
	})

	require.Eventually(t, func() bool {
		return txconfirmRef.requestCount() >= 3
	}, testTimeout, 10*time.Millisecond,
		"sweep was never broadcast")

	sweepReq := txconfirmRef.lastRequest(t)
	sweepTxid := sweepReq.Tx.TxHash()

	// Sweep confirms.
	txconfirmRef.emitConfirmed(t, 2, sweepTxid, 110)
	require.Eventually(t, func() bool {
		resp, ok := mustAsk(
			t, unrollActor.Ref(), &GetStateRequest{},
		).(*GetStateResp)
		require.True(t, ok)

		return resp.Phase == PhaseCompleted
	}, testTimeout, 10*time.Millisecond,
		"sweep never reached Completed")

	// 1. Target reorg invalidates the CSV anchor AND the sweep
	// confirmation (which depended on it).
	txconfirmRef.emitReorged(t, 1, targetTxid)

	require.Eventually(t, func() bool {
		resp, ok := mustAsk(
			t, unrollActor.Ref(), &GetStateRequest{},
		).(*GetStateResp)
		require.True(t, ok)

		csvCleared := resp.PlannerState.TargetConfirmHeight.IsNone()
		targetCleared := true
		for _, h := range resp.PlannerState.ConfirmedTxids {
			if h == targetTxid {
				targetCleared = false
			}
		}

		return csvCleared && targetCleared
	}, testTimeout, 10*time.Millisecond,
		"target anchor never cleared after reorg")
}

// recordingRegistryRef is a stub RegistryRef that captures every
// message delivered by the per-target actor's notifyRegistryIfTerminal
// path so a test can assert "the registry has / has not been told the
// actor is terminal".
type recordingRegistryRef struct {
	id   string
	mu   sync.Mutex
	msgs []RegistryMsg
}

// ID returns the stub registry ref identifier.
func (r *recordingRegistryRef) ID() string {
	return r.id
}

// Tell records the inbound RegistryMsg and acks.
func (r *recordingRegistryRef) Tell(_ context.Context, msg RegistryMsg) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, msg)

	return nil
}

// terminatedCount returns how many UnrollTerminatedMsg messages have
// been delivered so far.
func (r *recordingRegistryRef) terminatedCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	count := 0
	for _, m := range r.msgs {
		if _, ok := m.(*UnrollTerminatedMsg); ok {
			count++
		}
	}

	return count
}

// terminatedMessages returns a copy of every terminal message captured by the
// registry stub.
func (r *recordingRegistryRef) terminatedMessages() []*UnrollTerminatedMsg {
	r.mu.Lock()
	defer r.mu.Unlock()

	var messages []*UnrollTerminatedMsg
	for _, msg := range r.msgs {
		terminated, ok := msg.(*UnrollTerminatedMsg)
		if !ok {
			continue
		}

		messages = append(messages, terminated)
	}

	return messages
}

// TestRestartFailureCarriesDurableReliveGuard verifies that restart marks the
// job unsafe to relive before it reissues an in-flight transaction, and that a
// synchronous rejection carries the guard through the real actor terminal
// handoff. This closes the process-boundary gap where a conflicting offline
// spend could otherwise make the reissue fail "cleanly" and revive a spent
// VTXO.
func TestRestartFailureCarriesDurableReliveGuard(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	store := newMemCheckpointStore()
	txconfirmRef := &fakeTxConfirmRef{}
	registryRef := &recordingRegistryRef{id: "restart-reg-stub"}
	rootTxid := proof.RootTxids()[0]

	raw, err := encodeCheckpoint(&actorCheckpoint{
		Version: checkpointVersion,
		Height:  100,
		Started: true,
		Trigger: TriggerManual,
		State: unrollplan.State{
			InFlightTxids: []chainhash.Hash{rootTxid},
		},
	})
	require.NoError(t, err)
	require.NoError(
		t,
		store.SaveCheckpoint(
			t.Context(), actor.CheckpointParams{
				ActorID:   "restart-relive-guard-test",
				StateType: checkpointStateType,
				StateData: raw,
				Version:   checkpointVersion,
			},
		),
	)

	persistedBeforeReissue := make(chan bool, 1)
	txconfirmRef.onAsk = func(_ *txconfirm.EnsureConfirmedReq) {
		checkpoint, loadErr := store.LoadCheckpoint(
			context.Background(),
			"restart-relive-guard-test",
		)
		if loadErr != nil || checkpoint == nil {
			persistedBeforeReissue <- false

			return
		}

		decoded, decodeErr := decodeCheckpoint(checkpoint.StateData)
		persisted := decodeErr == nil && decoded.ReliveUnsafe
		persistedBeforeReissue <- persisted
	}
	txconfirmRef.setImmediateFailed(rootTxid, "conflicting spend")

	cfg := Config{
		TargetOutpoint: proof.TargetOutpoint(),
		ActorID:        "restart-relive-guard-test",
		DeliveryStore:  store,
		ProofAssembler: &mockProofAssembler{
			proof: proof,
		},
		VTXOStore: &mockVTXOStore{
			desc: desc,
		},
		TxConfirmRef: txconfirmRef,
		ChainSource:  &fakeChainSourceRef{},
		Wallet:       &fakeSweepWallet{},
		Log:          fn.Some(btclog.Disabled),
		RegistryRef:  registryRef,
	}
	resumeBehavior := &behavior{cfg: cfg, log: btclog.Disabled}
	require.NoError(t, resumeBehavior.restoreCheckpoint(t.Context()))

	resumedActor := actor.NewActor(actor.ActorConfig[Msg, Resp]{
		ID:          "restart-relive-guard-test",
		Behavior:    adaptTx(resumeBehavior),
		MailboxSize: 64,
	})
	resumeBehavior.selfRef = resumedActor.TellRef()
	resumedActor.Start()
	t.Cleanup(resumedActor.Stop)

	mustAsk(t, resumedActor.Ref(), &ResumeUnrollRequest{Height: 101})
	require.True(
		t, <-persistedBeforeReissue,
		"relive guard must be durable before txconfirm reissue",
	)
	require.Eventually(t, func() bool {
		return registryRef.terminatedCount() == 1
	}, testTimeout, 10*time.Millisecond)

	messages := registryRef.terminatedMessages()
	require.Len(t, messages, 1)
	require.Equal(t, PhaseFailed, messages[0].Phase)
	require.False(t, messages[0].HadOnChainFootprint)
	require.True(t, messages[0].ReliveUnsafe)
}

// TestSweepCompletionStaysProvisionalUntilFinalized proves the Phase 7
// invariant: a per-target actor that has reached PhaseCompleted does
// NOT notify the registry as terminal until a TxFinalizedMsg for the
// sweep txid arrives. Until then the actor stays alive so a reorg of
// the sweep confirmation has a live actor to deliver the rollback to.
func TestSweepCompletionStaysProvisionalUntilFinalized(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())

	store := newMemCheckpointStore()
	txconfirmRef := &fakeTxConfirmRef{}
	registryRef := &recordingRegistryRef{id: "reg-stub"}

	cfg := Config{
		TargetOutpoint: proof.TargetOutpoint(),
		ActorID:        "unroll-finality-test",
		DeliveryStore:  store,
		ProofAssembler: &mockProofAssembler{
			proof: proof,
		},
		VTXOStore: &mockVTXOStore{
			desc: desc,
		},
		TxConfirmRef: txconfirmRef,
		ChainSource:  &fakeChainSourceRef{},
		Wallet:       &fakeSweepWallet{},
		Log:          fn.Some(btclog.Disabled),
		RegistryRef:  registryRef,
	}
	beh := &behavior{cfg: cfg, log: btclog.Disabled}
	require.NoError(t, beh.restoreCheckpoint(t.Context()))

	actorInstance := actor.NewActor(actor.ActorConfig[Msg, Resp]{
		ID:          "unroll-finality-test",
		Behavior:    &txExecAdapter{b: beh, ax: newMemExecFor(beh)},
		MailboxSize: 32,
	})
	beh.selfRef = actorInstance.TellRef()
	actorInstance.Start()
	t.Cleanup(actorInstance.Stop)

	mustAsk(t, actorInstance.Ref(), &StartUnrollRequest{
		Height:  100,
		Trigger: TriggerManual,
	})

	rootTxid := proof.RootTxids()[0]
	targetTxid := proof.TargetOutpoint().Hash

	require.Eventually(t, func() bool {
		return txconfirmRef.requestCountForTxid(rootTxid) == 1
	}, testTimeout, 10*time.Millisecond)

	txconfirmRef.emitConfirmed(t, 0, rootTxid, 101)
	require.Eventually(t, func() bool {
		return txconfirmRef.requestCountForTxid(targetTxid) == 1
	}, testTimeout, 10*time.Millisecond)

	txconfirmRef.emitConfirmed(t, 1, targetTxid, 102)
	mustAsk(t, actorInstance.Ref(), &HeightObservedMsg{
		Height: 102 + int32(proof.CSVDelay()) + 1,
	})

	require.Eventually(t, func() bool {
		return txconfirmRef.requestCount() >= 3
	}, testTimeout, 10*time.Millisecond)

	sweepTxid := txconfirmRef.lastRequest(t).Tx.TxHash()
	txconfirmRef.emitConfirmed(t, 2, sweepTxid, 110)

	// Wait for the actor to settle on PhaseCompleted.
	require.Eventually(t, func() bool {
		resp, ok := mustAsk(
			t, actorInstance.Ref(), &GetStateRequest{},
		).(*GetStateResp)
		require.True(t, ok)

		return resp.Phase == PhaseCompleted
	}, testTimeout, 10*time.Millisecond)

	// 1. PROVISIONAL: registry must NOT have been told the actor is
	// terminal. The sweep can still reorg out.
	time.Sleep(50 * time.Millisecond)
	require.Equal(
		t, 0, registryRef.terminatedCount(),
		"registry was told terminal before TxFinalized arrived",
	)

	// 2. Sweep reorgs out; the live actor rolls back to
	// AwaitingSweepConfirmation. Registry still has not been
	// terminated.
	txconfirmRef.emitReorged(t, 2, sweepTxid)
	require.Eventually(t, func() bool {
		resp, ok := mustAsk(
			t, actorInstance.Ref(), &GetStateRequest{},
		).(*GetStateResp)
		require.True(t, ok)

		return resp.Phase == PhaseSweepConfirmation
	}, testTimeout, 10*time.Millisecond,
		"actor never rolled back to AwaitingSweepConfirmation")
	require.Equal(
		t, 0, registryRef.terminatedCount(),
		"registry was told terminal during sweep reorg recovery",
	)

	// 3. Sweep re-confirms; actor returns to PhaseCompleted.
	txconfirmRef.emitConfirmed(t, 2, sweepTxid, 111)
	require.Eventually(t, func() bool {
		resp, ok := mustAsk(
			t, actorInstance.Ref(), &GetStateRequest{},
		).(*GetStateResp)
		require.True(t, ok)

		return resp.Phase == PhaseCompleted
	}, testTimeout, 10*time.Millisecond)
	require.Equal(
		t, 0, registryRef.terminatedCount(),
		"registry was told terminal after sweep re-confirm but "+
			"before TxFinalized",
	)

	// 4. Finality. TxFinalizedMsg matches the recorded sweep txid,
	// the actor latches sweepFinalized, and the next driveEvent
	// fires UnrollTerminatedMsg.
	txconfirmRef.emitFinalized(t, 2, sweepTxid)
	require.Eventually(t, func() bool {
		return registryRef.terminatedCount() == 1
	}, testTimeout, 10*time.Millisecond,
		"registry was never told terminal after TxFinalized")
}

// TestExternalSpendReorgResumesActor observes an external spend of the
// target outpoint while materialization is in flight, then drives a
// reorg of the spending block. The actor must park in
// AwaitingExternalSpendFinality while the spend is provisional, then
// resume normal planning once the spend reorgs out. Without this
// behavior, a transient reorg-out replacement of the target would
// permanently terminate the recovery job.
func TestExternalSpendReorgResumesActor(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	unrollActor, beh, txconfirmRef, _ := newActorHarness(t, proof, desc)

	chainSource, ok := beh.cfg.ChainSource.(*fakeChainSourceRef)
	require.True(t, ok)

	mustAsk(t, unrollActor.Ref(), &StartUnrollRequest{
		Height:  100,
		Trigger: TriggerManual,
	})

	rootTxid := proof.RootTxids()[0]
	require.Eventually(t, func() bool {
		return txconfirmRef.requestCountForTxid(rootTxid) == 1
	}, testTimeout, 10*time.Millisecond,
		"root never submitted")

	// Ensure spend watch on the target is registered with the
	// reorg / done refs wired before driving any events.
	require.Eventually(t, func() bool {
		var targetRegistered bool
		for _, op := range chainSource.spendRegistrations() {
			if op == proof.TargetOutpoint() {
				targetRegistered = true
				break
			}
		}
		if !targetRegistered {
			return false
		}

		chainSource.mu.Lock()
		defer chainSource.mu.Unlock()

		return chainSource.spendReorgedRef != nil &&
			chainSource.spendFinalizedRef != nil
	}, testTimeout, 10*time.Millisecond)

	// 1. An external party broadcasts a spend of the target outpoint.
	externalTxid := chainhash.Hash{0xee}
	chainSource.emitSpendForOutpoint(
		t, proof.TargetOutpoint(), externalTxid, 110,
	)

	// The actor must park in AwaitingExternalSpendFinality rather
	// than transitioning to Failed; the spend is provisional.
	require.Eventually(t, func() bool {
		resp, ok := mustAsk(
			t, unrollActor.Ref(), &GetStateRequest{},
		).(*GetStateResp)
		require.True(t, ok)

		return resp.Phase == PhaseExternalSpendObserved &&
			resp.FailReason == ""
	}, testTimeout, 10*time.Millisecond,
		"actor never parked in PhaseExternalSpendObserved")

	// 2. The spending block is reorged out. The actor must clear the
	// provisional anchor and resume normal planning. Because the
	// proof root is still in-flight (no confirmation has arrived
	// yet), the actor goes back to PhaseMaterializing.
	chainSource.emitSpendReorged(t)

	require.Eventually(t, func() bool {
		resp, ok := mustAsk(
			t, unrollActor.Ref(), &GetStateRequest{},
		).(*GetStateResp)
		require.True(t, ok)

		return resp.Phase == PhaseMaterializing &&
			resp.FailReason == ""
	}, testTimeout, 10*time.Millisecond,
		"actor did not resume materialization after spend reorg")
}

// TestSweepConfirmationReorgReversesCompleted drives the full happy path
// to Completed and then reorgs the sweep confirmation out. The actor
// must drop SweepStatusConfirmed back to Broadcasted and clear the
// stored ConfirmHeight so the planner stops reporting Done.
func TestSweepConfirmationReorgReversesCompleted(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	unrollActor, _, txconfirmRef, _ := newActorHarness(t, proof, desc)

	mustAsk(t, unrollActor.Ref(), &StartUnrollRequest{
		Height:  100,
		Trigger: TriggerManual,
	})

	rootTxid := proof.RootTxids()[0]
	targetTxid := proof.TargetOutpoint().Hash

	require.Eventually(t, func() bool {
		return txconfirmRef.requestCountForTxid(rootTxid) == 1
	}, testTimeout, 10*time.Millisecond)

	txconfirmRef.emitConfirmed(t, 0, rootTxid, 101)
	require.Eventually(t, func() bool {
		return txconfirmRef.requestCountForTxid(targetTxid) == 1
	}, testTimeout, 10*time.Millisecond)

	txconfirmRef.emitConfirmed(t, 1, targetTxid, 102)
	mustAsk(t, unrollActor.Ref(), &HeightObservedMsg{
		Height: 102 + int32(proof.CSVDelay()) + 1,
	})

	require.Eventually(t, func() bool {
		return txconfirmRef.requestCount() >= 3
	}, testTimeout, 10*time.Millisecond)

	sweepReq := txconfirmRef.lastRequest(t)
	sweepTxid := sweepReq.Tx.TxHash()

	txconfirmRef.emitConfirmed(t, 2, sweepTxid, 110)
	require.Eventually(t, func() bool {
		resp, ok := mustAsk(
			t, unrollActor.Ref(), &GetStateRequest{},
		).(*GetStateResp)
		require.True(t, ok)

		return resp.Phase == PhaseCompleted
	}, testTimeout, 10*time.Millisecond)

	// 1. Sweep confirmation reorgs out. The actor must roll back to
	// AwaitingSweepConfirmation: the signed sweep is still durable, the
	// txconfirm subscription is still live, but the planner stops
	// reporting Done.
	txconfirmRef.emitReorged(t, 2, sweepTxid)

	require.Eventually(t, func() bool {
		resp, ok := mustAsk(
			t, unrollActor.Ref(), &GetStateRequest{},
		).(*GetStateResp)
		require.True(t, ok)

		return resp.Phase == PhaseSweepConfirmation &&
			resp.PlannerState.Sweep.Status ==
				unrollplan.SweepStatusBroadcasted &&
			resp.PlannerState.Sweep.ConfirmHeight.IsNone()
	}, testTimeout, 10*time.Millisecond,
		"sweep reorg did not roll the actor back to "+
			"AwaitingSweepConfirmation")

	// 2. Sweep reconfirms in a different block. The actor must move
	// back to Completed.
	txconfirmRef.emitConfirmed(t, 2, sweepTxid, 111)
	require.Eventually(t, func() bool {
		resp, ok := mustAsk(
			t, unrollActor.Ref(), &GetStateRequest{},
		).(*GetStateResp)
		require.True(t, ok)

		sweep := resp.PlannerState.Sweep

		return resp.Phase == PhaseCompleted &&
			sweep.ConfirmHeight.IsSome() &&
			sweep.ConfirmHeight.UnsafeFromSome() == 111
	}, testTimeout, 10*time.Millisecond,
		"sweep did not re-Complete after reconfirm")
}

// TestOfflineReorgRestartReconcilesBeforeSideEffects bundles the three
// offline-reorg scenarios (target reorged out, sweep reorged out, and
// external-spend reorged out) into one restart and asserts that the
// reconciler runs BEFORE the FSM session is built — i.e. before the
// actor can broadcast a stale sweep or park in
// AwaitingExternalSpendFinality on a vanished spender.
//
// The setup mimics a daemon that committed a checkpoint reflecting:
//
//   - root + target proof nodes confirmed
//   - a sweep tx broadcast AND confirmed at H=108
//   - an external spender provisionally observed at H=110
//
// While the daemon was offline, the chain reorged: the target's
// confirming block was orphaned (so the sweep that spent it can no
// longer be confirmed either), and the external spender's block was
// also dropped. The reconciler the actor consults on restart returns
// "root still confirmed" and "everything else absent".
//
// After Resume the actor must end up in the post-reconcile state
// (sweep downgraded to Pending, TargetConfirmHeight cleared, target
// pruned from ConfirmedTxids, ProvisionalExternalSpend cleared) and
// must NOT have re-submitted the stale sweep tx to txconfirm before
// the planner re-derives broadcast intent from the post-reconcile
// PlannerState. The latter is the load-bearing safety invariant: a
// re-broadcast off a stale checkpoint would race a fresh wallet
// pkScript against any future re-mining of the target.
func TestOfflineReorgRestartReconcilesBeforeSideEffects(t *testing.T) {
	proof := buildLinearProof(t)
	rootTxid := proof.RootTxids()[0]
	targetTxid := proof.TargetOutpoint().Hash
	staleSweepTxid := chainhash.Hash{0xaa}
	staleExternalTxid := chainhash.Hash{0xbb}

	checkpoint := &actorCheckpoint{
		Version: checkpointVersion,
		Height:  112,
		Started: true,
		Trigger: TriggerRestart,
		State: unrollplan.State{
			ConfirmedTxids: []chainhash.Hash{
				rootTxid, targetTxid,
			},
			TargetConfirmHeight: fn.Some[int32](102),
			Sweep: unrollplan.SweepState{
				Status:        unrollplan.SweepStatusConfirmed,
				Txid:          fn.Some(staleSweepTxid),
				ConfirmHeight: fn.Some[int32](108),
			},
		},
		ProvisionalExternalSpend: fn.Some(ExternalSpendAnchor{
			SpendingTxid:   staleExternalTxid,
			SpendingHeight: 110,
		}),
	}

	// Reconciler reports:
	//   - root still confirmed (so the planner keeps the partial
	//     proof-graph progress)
	//   - target absent (offline reorg)
	//   - stale sweep absent (offline reorg)
	//   - target outpoint unspent (external spender reorged out)
	reconciler := &stubChainReconciler{
		confirmed: map[chainhash.Hash]ConfirmedAnchor{
			rootTxid: {
				Txid:   rootTxid,
				Height: 101,
			},
		},
	}

	beh, txconfirmRef, _ := restoreHarness(
		t, proof, checkpoint, reconciler,
	)

	actorInstance := actor.NewActor(actor.ActorConfig[Msg, Resp]{
		ID:          "reconcile-test",
		Behavior:    &txExecAdapter{b: beh, ax: newMemExecFor(beh)},
		MailboxSize: 16,
	})
	beh.selfRef = actorInstance.TellRef()
	actorInstance.Start()
	t.Cleanup(actorInstance.Stop)

	mustAsk(t, actorInstance.Ref(), &ResumeUnrollRequest{Height: 112})

	// The actor must end up in the post-reconcile state: sweep
	// downgraded, target-derived height cleared, target pruned from
	// ConfirmedTxids, external spend cleared, root preserved.
	require.Eventually(t, func() bool {
		resp, ok := mustAsk(
			t, actorInstance.Ref(), &GetStateRequest{},
		).(*GetStateResp)
		require.True(t, ok)

		for _, h := range resp.PlannerState.ConfirmedTxids {
			if h == targetTxid {
				return false
			}
		}
		sweep := resp.PlannerState.Sweep
		rootPresent := false
		for _, h := range resp.PlannerState.ConfirmedTxids {
			if h == rootTxid {
				rootPresent = true
				break
			}
		}

		return rootPresent &&
			sweep.Status == unrollplan.SweepStatusPending &&
			sweep.Txid.IsNone() &&
			sweep.ConfirmHeight.IsNone() &&
			resp.PlannerState.TargetConfirmHeight.IsNone() &&
			resp.Phase == PhaseMaterializing &&
			resp.FailReason == ""
	}, testTimeout, 10*time.Millisecond,
		"actor did not reach post-reconcile state on restart")

	// Load-bearing safety invariant: every txconfirm request issued
	// after restart must target a tx the post-reconcile PlannerState
	// authorizes. In particular, the orphaned sweep txid must never
	// be re-submitted — that would replay a sweep against a target
	// the chain says no longer exists.
	require.Zero(
		t, txconfirmRef.requestCountForTxid(staleSweepTxid),
		"stale sweep tx was re-submitted to txconfirm before "+
			"reconciliation downgraded it",
	)
}
