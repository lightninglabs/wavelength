package unroll

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/lib/recovery"
	"github.com/lightninglabs/wavelength/unrollplan"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// stubChainReconciler is a fully controllable ChainReconciler test
// double. The two maps drive the ConfirmedTx / SpentOutpoint answers
// and a non-nil err field lets a test inject a transport failure on
// either query.
type stubChainReconciler struct {
	confirmed map[chainhash.Hash]ConfirmedAnchor
	spent     map[wire.OutPoint]SpendAnchor
	err       error
}

// ConfirmedTx returns the configured anchor for txid, or fn.None.
func (s *stubChainReconciler) ConfirmedTx(_ context.Context,
	txid chainhash.Hash) (fn.Option[ConfirmedAnchor], error) {

	if s.err != nil {
		return fn.None[ConfirmedAnchor](), s.err
	}

	anchor, ok := s.confirmed[txid]
	if !ok {
		return fn.None[ConfirmedAnchor](), nil
	}

	return fn.Some(anchor), nil
}

// SpentOutpoint returns the configured anchor for outpoint, or fn.None.
func (s *stubChainReconciler) SpentOutpoint(_ context.Context,
	outpoint wire.OutPoint) (fn.Option[SpendAnchor], error) {

	if s.err != nil {
		return fn.None[SpendAnchor](), s.err
	}

	anchor, ok := s.spent[outpoint]
	if !ok {
		return fn.None[SpendAnchor](), nil
	}

	return fn.Some(anchor), nil
}

// restoreHarness boots a fresh unroll behavior from a fabricated
// checkpoint so the reconcile tests can inspect the post-reconciliation
// FSM state without running the rest of the actor's IO surface.
func restoreHarness(t *testing.T, proof *recovery.Proof,
	checkpoint *actorCheckpoint, reconciler ChainReconciler) (*behavior,
	*fakeTxConfirmRef, *fakeChainSourceRef) {

	t.Helper()

	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	store := newMemCheckpointStore()
	txconfirmRef := &fakeTxConfirmRef{}
	chainRef := &fakeChainSourceRef{}

	raw, err := encodeCheckpoint(checkpoint)
	require.NoError(t, err)

	err = store.SaveCheckpoint(t.Context(), actor.CheckpointParams{
		ActorID:   "reconcile-test",
		StateType: checkpointStateType,
		StateData: raw,
		Version:   checkpointVersion,
	})
	require.NoError(t, err)

	cfg := Config{
		TargetOutpoint: proof.TargetOutpoint(),
		ActorID:        "reconcile-test",
		DeliveryStore:  store,
		ProofAssembler: &mockProofAssembler{
			proof: proof,
		},
		VTXOStore: &mockVTXOStore{
			desc: desc,
		},
		TxConfirmRef: txconfirmRef,
		ChainSource:  chainRef,
		Wallet:       &fakeSweepWallet{},
		Log:          fn.Some(btclog.Disabled),
	}
	if reconciler != nil {
		// Wrap the stub in a factory that ignores the proof, so
		// the production code path (which always calls the
		// factory) sees the test stub unchanged.
		stub := reconciler
		cfg.ChainReconcilerFactory = fn.Some(
			ChainReconcilerFactory(
				func(wire.OutPoint,
					*recovery.Proof) ChainReconciler {

					return stub
				},
			),
		)
	}

	beh := &behavior{
		cfg: cfg,
		log: btclog.Disabled,
	}
	require.NoError(t, beh.restoreCheckpoint(t.Context()))

	return beh, txconfirmRef, chainRef
}

// TestReconcileTargetReorgedOfflineResumesMaterialization restores a
// checkpoint whose target tx is recorded as confirmed but is no longer
// on the canonical chain. After reconciliation the actor must restart
// without the stale TargetConfirmHeight, in PhaseMaterializing, so it
// does not broadcast a sweep on a target that has been reorged out.
func TestReconcileTargetReorgedOfflineResumesMaterialization(t *testing.T) {
	proof := buildLinearProof(t)
	rootTxid := proof.RootTxids()[0]
	targetTxid := proof.TargetOutpoint().Hash

	checkpoint := &actorCheckpoint{
		Version: checkpointVersion,
		Height:  103,
		Started: true,
		Trigger: TriggerRestart,
		State: unrollplan.State{
			ConfirmedTxids: []chainhash.Hash{
				rootTxid, targetTxid,
			},
			TargetConfirmHeight: fn.Some[int32](102),
		},
	}

	// Reconciler reports the root is still on chain at H=101 but the
	// target tx is absent — its confirming block was reorged out
	// while the daemon was offline.
	reconciler := &stubChainReconciler{
		confirmed: map[chainhash.Hash]ConfirmedAnchor{
			rootTxid: {
				Txid:   rootTxid,
				Height: 101,
			},
		},
	}

	beh, _, _ := restoreHarness(t, proof, checkpoint, reconciler)

	actorInstance := actor.NewActor(actor.ActorConfig[Msg, Resp]{
		ID:          "reconcile-test",
		Behavior:    &txExecAdapter{b: beh, ax: newMemExecFor(beh)},
		MailboxSize: 16,
	})
	beh.selfRef = actorInstance.TellRef()
	actorInstance.Start()
	t.Cleanup(actorInstance.Stop)

	mustAsk(t, actorInstance.Ref(), &ResumeUnrollRequest{Height: 103})

	require.Eventually(t, func() bool {
		resp, ok := mustAsk(
			t, actorInstance.Ref(), &GetStateRequest{},
		).(*GetStateResp)
		require.True(t, ok)

		// Target anchor must be gone, planner must be back in
		// materialization, and no FailReason should have leaked
		// through.
		for _, h := range resp.PlannerState.ConfirmedTxids {
			if h == targetTxid {
				return false
			}
		}

		return resp.Phase == PhaseMaterializing &&
			resp.PlannerState.TargetConfirmHeight.IsNone() &&
			resp.FailReason == ""
	}, testTimeout, 10*time.Millisecond,
		"reconciliation never cleared the stale target anchor")
}

// TestReconcileSweepReorgedOfflineDowngradesSweep restores a
// checkpoint with a Confirmed sweep that is no longer on chain. After
// reconciliation the sweep state must reset so the next planner pass
// re-emits NeedSweep and the cached sweep tx is re-broadcast.
func TestReconcileSweepReorgedOfflineDowngradesSweep(t *testing.T) {
	proof := buildLinearProof(t)
	rootTxid := proof.RootTxids()[0]
	targetTxid := proof.TargetOutpoint().Hash
	sweepTxid := chainhash.Hash{0xaa}

	checkpoint := &actorCheckpoint{
		Version: checkpointVersion,
		Height:  110,
		Started: true,
		Trigger: TriggerRestart,
		State: unrollplan.State{
			ConfirmedTxids: []chainhash.Hash{
				rootTxid, targetTxid,
			},
			TargetConfirmHeight: fn.Some[int32](102),
			Sweep: unrollplan.SweepState{
				Status:        unrollplan.SweepStatusConfirmed,
				Txid:          fn.Some(sweepTxid),
				ConfirmHeight: fn.Some[int32](108),
			},
		},
	}

	// Reconciler: every proof anchor still confirmed, but the sweep
	// is no longer on chain.
	reconciler := &stubChainReconciler{
		confirmed: map[chainhash.Hash]ConfirmedAnchor{
			rootTxid: {
				Txid:   rootTxid,
				Height: 101,
			},
			targetTxid: {
				Txid:   targetTxid,
				Height: 102,
			},
		},
	}

	beh, _, _ := restoreHarness(t, proof, checkpoint, reconciler)

	actorInstance := actor.NewActor(actor.ActorConfig[Msg, Resp]{
		ID:          "reconcile-test",
		Behavior:    &txExecAdapter{b: beh, ax: newMemExecFor(beh)},
		MailboxSize: 16,
	})
	beh.selfRef = actorInstance.TellRef()
	actorInstance.Start()
	t.Cleanup(actorInstance.Stop)

	mustAsk(t, actorInstance.Ref(), &ResumeUnrollRequest{Height: 110})

	require.Eventually(t, func() bool {
		resp, ok := mustAsk(
			t, actorInstance.Ref(), &GetStateRequest{},
		).(*GetStateResp)
		require.True(t, ok)

		// The target anchor must survive a sweep-only reorg —
		// CSV maturity is still valid even though the sweep
		// itself needs to be re-broadcast. The planner should
		// move the actor back into AwaitingSweepBroadcast (or
		// have already requested a fresh sweep build).
		sweep := resp.PlannerState.Sweep

		return resp.PlannerState.TargetConfirmHeight.IsSome() &&
			(sweep.Status == unrollplan.SweepStatusPending ||
				sweep.Status ==
					unrollplan.SweepStatusBroadcasted) &&
			sweep.ConfirmHeight.IsNone()
	}, testTimeout, 10*time.Millisecond,
		"reconciliation never downgraded the stale sweep anchor")
}

// TestReconcileRefreshesTargetHeightAfterOfflineReMine restores a
// checkpoint that recorded the target tx confirmed at one height, but
// the reconciler reports the target as still confirmed at a DIFFERENT
// (higher) height — i.e. the target tx was re-mined into a different
// block while the daemon was offline. The reconciler must refresh
// TargetConfirmHeight to the live value so CSV maturity is computed
// off the correct on-chain anchor, not the stale checkpoint height.
func TestReconcileRefreshesTargetHeightAfterOfflineReMine(t *testing.T) {
	proof := buildLinearProof(t)
	rootTxid := proof.RootTxids()[0]
	targetTxid := proof.TargetOutpoint().Hash

	const (
		staleHeight = int32(102)
		liveHeight  = int32(105)
	)

	checkpoint := &actorCheckpoint{
		Version: checkpointVersion,
		Height:  staleHeight,
		Started: true,
		Trigger: TriggerRestart,
		State: unrollplan.State{
			ConfirmedTxids: []chainhash.Hash{
				rootTxid, targetTxid,
			},
			TargetConfirmHeight: fn.Some(staleHeight),
		},
	}

	reconciler := &stubChainReconciler{
		confirmed: map[chainhash.Hash]ConfirmedAnchor{
			rootTxid: {
				Txid:   rootTxid,
				Height: 101,
			},
			targetTxid: {
				Txid:   targetTxid,
				Height: liveHeight,
			},
		},
	}

	err := reconcileCheckpoint(t.Context(), reconciler, proof, checkpoint)
	require.NoError(t, err)

	require.True(t, checkpoint.State.TargetConfirmHeight.IsSome())
	require.Equal(
		t, liveHeight,
		checkpoint.State.TargetConfirmHeight.UnsafeFromSome(),
		"TargetConfirmHeight must refresh to the live anchor height",
	)
	require.GreaterOrEqual(
		t, checkpoint.Height, liveHeight,
		"checkpoint best-height should not lag the live anchor",
	)
}

// TestReconcileTransportErrorSurfacesUpward asserts that a transport
// failure from the reconciler propagates out of ensureLoaded rather
// than silently letting the actor proceed off an unverified
// checkpoint.
func TestReconcileTransportErrorSurfacesUpward(t *testing.T) {
	proof := buildLinearProof(t)
	rootTxid := proof.RootTxids()[0]

	checkpoint := &actorCheckpoint{
		Version: checkpointVersion,
		Height:  101,
		Started: true,
		Trigger: TriggerRestart,
		State: unrollplan.State{
			ConfirmedTxids: []chainhash.Hash{
				rootTxid,
			},
		},
	}

	reconciler := &stubChainReconciler{
		err: errors.New("backend unreachable"),
	}

	beh, _, _ := restoreHarness(t, proof, checkpoint, reconciler)

	err := beh.ensureLoaded(t.Context())
	require.Error(t, err)
	require.Contains(t, err.Error(), "backend unreachable")
}

// TestReconcileExternalSpendStillLiveRestoresParkedActor restores a
// checkpoint that already carries a ProvisionalExternalSpend anchor
// and verifies that the reconciler keeps it set when the chain still
// reports the target as spent — the actor must enter
// AwaitingExternalSpendFinality on the next Resume rather than
// resuming planner-driven materialization.
func TestReconcileExternalSpendStillLiveRestoresParkedActor(t *testing.T) {
	proof := buildLinearProof(t)
	rootTxid := proof.RootTxids()[0]
	externalTxid := chainhash.Hash{0xee}

	checkpoint := &actorCheckpoint{
		Version: checkpointVersion,
		Height:  155,
		Started: true,
		Trigger: TriggerRestart,
		State: unrollplan.State{
			ConfirmedTxids: []chainhash.Hash{
				rootTxid,
			},
		},
		ProvisionalExternalSpend: fn.Some(ExternalSpendAnchor{
			SpendingTxid:   externalTxid,
			SpendingHeight: 155,
		}),
	}

	reconciler := &stubChainReconciler{
		confirmed: map[chainhash.Hash]ConfirmedAnchor{
			rootTxid: {
				Txid:   rootTxid,
				Height: 101,
			},
		},
		spent: map[wire.OutPoint]SpendAnchor{
			proof.TargetOutpoint(): {
				Outpoint:       proof.TargetOutpoint(),
				SpendingTxid:   externalTxid,
				SpendingHeight: 155,
			},
		},
	}

	beh, _, _ := restoreHarness(t, proof, checkpoint, reconciler)

	actorInstance := actor.NewActor(actor.ActorConfig[Msg, Resp]{
		ID:          "reconcile-test",
		Behavior:    &txExecAdapter{b: beh, ax: newMemExecFor(beh)},
		MailboxSize: 16,
	})
	beh.selfRef = actorInstance.TellRef()
	actorInstance.Start()
	t.Cleanup(actorInstance.Stop)

	mustAsk(t, actorInstance.Ref(), &ResumeUnrollRequest{Height: 155})

	require.Eventually(t, func() bool {
		resp, ok := mustAsk(
			t, actorInstance.Ref(), &GetStateRequest{},
		).(*GetStateResp)
		require.True(t, ok)

		return resp.Phase == PhaseExternalSpendObserved &&
			resp.FailReason == ""
	}, testTimeout, 10*time.Millisecond,
		"actor did not restore into PhaseExternalSpendObserved")
}

// TestReconcileExternalSpendReorgedOfflineResumesActor restores a
// checkpoint carrying a ProvisionalExternalSpend anchor but the
// reconciler reports the target outpoint as currently unspent: the
// spending block was reorged out while the daemon was offline.
// Reconciliation must clear the anchor so the actor resumes
// materialization instead of staying parked.
func TestReconcileExternalSpendReorgedOfflineResumesActor(t *testing.T) {
	proof := buildLinearProof(t)
	rootTxid := proof.RootTxids()[0]
	externalTxid := chainhash.Hash{0xee}

	checkpoint := &actorCheckpoint{
		Version: checkpointVersion,
		Height:  155,
		Started: true,
		Trigger: TriggerRestart,
		State: unrollplan.State{
			InFlightTxids: []chainhash.Hash{
				rootTxid,
			},
		},
		ProvisionalExternalSpend: fn.Some(ExternalSpendAnchor{
			SpendingTxid:   externalTxid,
			SpendingHeight: 155,
		}),
	}

	// Reconciler reports the target outpoint as unspent. The
	// proof root is still in-flight; no confirmed anchors to verify
	// here.
	reconciler := &stubChainReconciler{}

	beh, _, _ := restoreHarness(t, proof, checkpoint, reconciler)

	actorInstance := actor.NewActor(actor.ActorConfig[Msg, Resp]{
		ID:          "reconcile-test",
		Behavior:    &txExecAdapter{b: beh, ax: newMemExecFor(beh)},
		MailboxSize: 16,
	})
	beh.selfRef = actorInstance.TellRef()
	actorInstance.Start()
	t.Cleanup(actorInstance.Stop)

	mustAsk(t, actorInstance.Ref(), &ResumeUnrollRequest{Height: 155})

	require.Eventually(t, func() bool {
		resp, ok := mustAsk(
			t, actorInstance.Ref(), &GetStateRequest{},
		).(*GetStateResp)
		require.True(t, ok)

		return resp.Phase == PhaseMaterializing &&
			resp.FailReason == ""
	}, testTimeout, 10*time.Millisecond,
		"actor did not resume materialization after offline "+
			"external-spend reorg")
}

// TestReconcileExternalSpendObservedOfflineParksActor restores a
// checkpoint that does NOT carry a ProvisionalExternalSpend anchor,
// but the reconciler reports the target outpoint as currently spent
// by a txid that is neither in the proof graph nor our sweep. The
// reconciler must install the anchor so the actor enters
// AwaitingExternalSpendFinality on the next Resume.
func TestReconcileExternalSpendObservedOfflineParksActor(t *testing.T) {
	proof := buildLinearProof(t)
	rootTxid := proof.RootTxids()[0]
	externalTxid := chainhash.Hash{0xee}

	checkpoint := &actorCheckpoint{
		Version: checkpointVersion,
		Height:  155,
		Started: true,
		Trigger: TriggerRestart,
		State: unrollplan.State{
			ConfirmedTxids: []chainhash.Hash{
				rootTxid,
			},
		},
	}

	reconciler := &stubChainReconciler{
		confirmed: map[chainhash.Hash]ConfirmedAnchor{
			rootTxid: {
				Txid:   rootTxid,
				Height: 101,
			},
		},
		spent: map[wire.OutPoint]SpendAnchor{
			proof.TargetOutpoint(): {
				Outpoint:       proof.TargetOutpoint(),
				SpendingTxid:   externalTxid,
				SpendingHeight: 155,
			},
		},
	}

	beh, _, _ := restoreHarness(t, proof, checkpoint, reconciler)

	actorInstance := actor.NewActor(actor.ActorConfig[Msg, Resp]{
		ID:          "reconcile-test",
		Behavior:    &txExecAdapter{b: beh, ax: newMemExecFor(beh)},
		MailboxSize: 16,
	})
	beh.selfRef = actorInstance.TellRef()
	actorInstance.Start()
	t.Cleanup(actorInstance.Stop)

	mustAsk(t, actorInstance.Ref(), &ResumeUnrollRequest{Height: 155})

	require.Eventually(t, func() bool {
		resp, ok := mustAsk(
			t, actorInstance.Ref(), &GetStateRequest{},
		).(*GetStateResp)
		require.True(t, ok)

		return resp.Phase == PhaseExternalSpendObserved &&
			resp.FailReason == ""
	}, testTimeout, 10*time.Millisecond,
		"actor did not park in PhaseExternalSpendObserved after "+
			"offline external spend")
}
