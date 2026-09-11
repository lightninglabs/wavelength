package unroll

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/txconfirm"
	"github.com/lightninglabs/wavelength/unrollplan"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestRejectedSweepRepricesAfterEstimateRecovery proves that fee rejection
// preserves one obligation and destination across duplicate delivery, estimator
// outage, restart, replacement, and either candidate winning on chain.
func TestRejectedSweepRepricesAfterEstimateRecovery(t *testing.T) {
	for _, restart := range []bool{false, true} {
		for _, oldWins := range []bool{false, true} {
			t.Run(repriceCaseName(restart, oldWins), func(
				t *testing.T) {

				proof := buildLinearProof(t)
				desc := testDescriptor(
					t, proof.TargetOutpoint(),
					proof.CSVDelay(),
				)
				inst, b, confirmer, store := newActorHarness(
					t, proof, desc,
				)
				source := b.cfg.ChainSource
				chain, ok := source.(*fakeChainSourceRef)
				require.True(t, ok)
				wallet, ok := b.cfg.Wallet.(*fakeSweepWallet)
				require.True(t, ok)
				chain.setFeeEstimate(2, nil)
				oldID := driveLinearToSweep(
					t, inst.Ref(), confirmer, store, proof,
				)
				oldTx := confirmer.lastRequest(t).Tx.Copy()
				rejection := &TxFailedMsg{
					Txid:   oldID,
					Reason: "min relay fee not met",
					Class:  txconfirm.BroadcastFailureFee,
				}
				mustAsk(t, inst.Ref(), rejection)
				mustAsk(t, inst.Ref(), rejection)
				cp := mustDecodeCheckpoint(
					t, store, "unroll-test",
				)
				require.Equal(t, 0, cp.SweepAttempts)
				require.Empty(t, cp.Fail)
				require.Equal(
					t, oldID,
					cp.RejectedSweep.UnsafeFromSome(),
				)
				require.Equal(
					t, unrollplan.SweepStatusPending,
					cp.State.Sweep.Status,
				)
				require.Equal(t, 3, confirmer.requestCount())

				if restart {
					inst.Stop()
					inst, b = restartSweepActor(t, b.cfg)
					mustAsk(
						t, inst.Ref(),
						&ResumeUnrollRequest{
							Height: 104,
						},
					)
				}
				chain.setFeeEstimate(
					0, errors.New("estimator unavailable"),
				)
				mustAsk(
					t, inst.Ref(), &HeightObservedMsg{
						Height: 105,
					},
				)
				chain.setFeeEstimate(2, nil)
				mustAsk(
					t, inst.Ref(), &HeightObservedMsg{
						Height: 106,
					},
				)
				require.Equal(t, 3, confirmer.requestCount())
				chain.setFeeEstimate(10, nil)
				mustAsk(
					t, inst.Ref(), &HeightObservedMsg{
						Height: 107,
					},
				)
				newTx := confirmer.lastRequest(t).Tx.Copy()
				newID := newTx.TxHash()
				require.NotEqual(t, oldID, newID)
				require.Equal(
					t, oldTx.TxIn[0].PreviousOutPoint,
					newTx.TxIn[0].PreviousOutPoint,
				)
				require.Equal(
					t, oldTx.TxOut[0].PkScript,
					newTx.TxOut[0].PkScript,
				)
				require.Less(
					t, newTx.TxOut[0].Value,
					oldTx.TxOut[0].Value,
				)
				require.Equal(
					t, int64(1),
					wallet.pkScriptRequestCount(),
				)
				require.Equal(
					t, []chainhash.Hash{oldID},
					chain.removedTxSnapshot(),
				)
				cp = mustDecodeCheckpoint(
					t, store, "unroll-test",
				)
				require.Len(t, cp.ReplacedSweeps, 1)
				require.Equal(
					t, oldID, cp.ReplacedSweeps[0].TxHash(),
				)
				require.Equal(t, newID, cp.SweepTx.TxHash())
				require.True(t, cp.RejectedSweep.IsNone())
				require.Empty(t, cp.Fail)
				mustAsk(t, inst.Ref(), rejection)
				mustAsk(
					t, inst.Ref(), &HeightObservedMsg{
						Height: 107,
					},
				)
				require.Equal(t, 4, confirmer.requestCount())
				state, ok := mustAsk(
					t, inst.Ref(), &GetStateRequest{},
				).(*GetStateResp)
				require.True(t, ok)
				require.Equal(
					t, PhaseSweepConfirmation, state.Phase,
				)

				if oldWins {
					// Conflict can arrive before the old
					// sweep's spend notification. Duplicate
					// errors cannot kill the exit.
					conflict := &TxFailedMsg{
						Txid:   newID,
						Reason: "inputs spent",
					}
					for range maxSweepAttempts + 1 {
						mustAsk(t, inst.Ref(), conflict)
					}

					cp = mustDecodeCheckpoint(
						t, store, "unroll-test",
					)
					require.Empty(t, cp.Fail)
					require.True(t, cp.RetrySame)
				}

				// Restore candidate history before receiving
				// the winning spend, including when it is an
				// older transaction.
				if restart {
					inst.Stop()
					inst, _ = restartSweepActor(t, b.cfg)
				}
				winner := newID
				if oldWins {
					winner = oldID
				}
				mustAsk(t, inst.Ref(), &SpendObservedMsg{
					Outpoint:       proof.TargetOutpoint(),
					SpendingTxid:   winner,
					SpendingHeight: 108,
				})
				cp = mustDecodeCheckpoint(
					t, store, "unroll-test",
				)
				require.Equal(
					t, unrollplan.SweepStatusConfirmed,
					cp.State.Sweep.Status,
				)
				require.Equal(t, winner, cp.SweepTx.TxHash())
				require.Empty(t, cp.Fail)
			})
		}
	}
}

// repriceCaseName makes the two independent recovery choices visible in output.
func repriceCaseName(restart, oldWins bool) string {
	name := "live"
	if restart {
		name = "restart"
	}
	if oldWins {
		return name + "/original-confirms"
	}

	return name + "/replacement-confirms"
}

// restartSweepActor reconstructs a new actor solely from its saved checkpoint.
// The caller stops the previous actor before invoking this helper.
func restartSweepActor(t *testing.T, cfg Config) (*actor.Actor[Msg, Resp],
	*behavior) {

	t.Helper()
	b := &behavior{cfg: cfg, log: btclog.Disabled}
	require.NoError(t, b.restoreCheckpoint(t.Context()))
	inst := actor.NewActor(actor.ActorConfig[Msg, Resp]{
		ID: cfg.ActorID, Behavior: adaptTx(b), MailboxSize: 64,
	})
	b.selfRef = inst.TellRef()
	inst.Start()
	t.Cleanup(inst.Stop)

	return inst, b
}

// cleanupFailureRef injects a single old-tracker cleanup failure after the
// signed replacement has been staged but before it can be submitted.
type cleanupFailureRef struct {
	*fakeTxConfirmRef
	fail atomic.Bool
}

// Ask rejects one cancellation while preserving ordinary txconfirm behavior.
func (f *cleanupFailureRef) Ask(ctx context.Context,
	msg txconfirm.Msg) actor.Future[txconfirm.Resp] {

	if _, ok := msg.(*txconfirm.CancelInterestReq); ok &&
		f.fail.Swap(false) {

		p := actor.NewPromise[txconfirm.Resp]()
		p.Complete(
			fn.Err[txconfirm.Resp](
				errors.New("cleanup unavailable"),
			),
		)

		return p.Future()
	}

	return f.fakeTxConfirmRef.Ask(ctx, msg)
}

// TestReplacementStageSurvivesCleanupFailure proves that a prepared replacement
// survives a restart before admission and is reused without another estimate.
func TestReplacementStageSurvivesCleanupFailure(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	inst, b, confirmer, store := newActorHarness(t, proof, desc)
	chain, ok := b.cfg.ChainSource.(*fakeChainSourceRef)
	require.True(t, ok)
	chain.setFeeEstimate(2, nil)
	oldID := driveLinearToSweep(t, inst.Ref(), confirmer, store, proof)
	mustAsk(t, inst.Ref(), &TxFailedMsg{
		Txid: oldID, Reason: "mempool min fee not met",
		Class: txconfirm.BroadcastFailureFee,
	})
	inst.Stop()
	failer := &cleanupFailureRef{fakeTxConfirmRef: confirmer}
	failer.fail.Store(true)
	cfg := b.cfg
	cfg.TxConfirmRef = failer
	inst, _ = restartSweepActor(t, cfg)
	chain.setFeeEstimate(10, nil)
	_, err := inst.
		Ref().
		Ask(t.Context(), &ResumeUnrollRequest{Height: 105}).
		Await(t.Context()).
		Unpack()
	require.ErrorContains(t, err, "cleanup unavailable")
	cp := mustDecodeCheckpoint(t, store, cfg.ActorID)
	newID := cp.SweepTx.TxHash()
	require.NotEqual(t, oldID, newID)
	require.Equal(t, oldID, cp.RejectedSweep.UnsafeFromSome())
	require.Equal(t, 3, confirmer.requestCount())
	inst.Stop()
	chain.setFeeEstimate(0, errors.New("estimator unavailable"))
	calls := chain.feeRequestCount()
	inst, _ = restartSweepActor(t, cfg)
	mustAsk(t, inst.Ref(), &ResumeUnrollRequest{Height: 106})
	require.Equal(t, newID, confirmer.lastRequest(t).Tx.TxHash())
	require.Equal(t, calls, chain.feeRequestCount())
	require.Equal(t, 4, confirmer.requestCount())
}

// TestPermanentSweepFailureDoesNotReprice keeps structural-invalid failures
// on the existing bounded path even when the estimator would offer a new fee.
func TestPermanentSweepFailureDoesNotReprice(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	inst, b, confirmer, store := newActorHarness(t, proof, desc)
	oldID := driveLinearToSweep(t, inst.Ref(), confirmer, store, proof)
	chain, ok := b.cfg.ChainSource.(*fakeChainSourceRef)
	require.True(t, ok)
	chain.setFeeEstimate(10, nil)
	for range maxSweepAttempts {
		mustAsk(t, inst.Ref(), &TxFailedMsg{
			Txid:   oldID,
			Reason: "mandatory-script-verify-flag-failed",
			Class:  txconfirm.BroadcastFailurePermanent,
		})
	}
	cp := mustDecodeCheckpoint(t, store, "unroll-test")
	require.NotEmpty(t, cp.Fail)
	require.Equal(t, oldID, cp.SweepTx.TxHash())
	require.Empty(t, cp.ReplacedSweeps)
	require.Equal(t, 1, chain.feeRequestCount())
}

// persistentRemoveFailureRef models a wallet RPC that cannot remove broadcasts.
type persistentRemoveFailureRef struct {
	*fakeChainSourceRef
	calls atomic.Int32
}

// Ask always rejects removal while retaining normal fee and chain responses.
func (f *persistentRemoveFailureRef) Ask(ctx context.Context,
	msg chainsource.ChainSourceMsg,
) actor.Future[chainsource.ChainSourceResp] {

	if _, ok := msg.(*chainsource.RemoveTxRequest); ok {
		f.calls.Add(1)
		p := actor.NewPromise[chainsource.ChainSourceResp]()
		p.Complete(
			fn.Err[chainsource.ChainSourceResp](
				errors.New("rpc error: code = Unimplemented " +
					"desc = unknown method " +
					"RemoveTransaction"),
			),
		)

		return p.Future()
	}

	return f.fakeChainSourceRef.Ask(ctx, msg)
}

// TestReplacementSurvivesWalletRemovalFailure proves that failed wallet cleanup
// cannot strand a staged replacement. A later invalid replacement also cannot
// erase the still-valid earlier candidate's eventual confirmed spend.
func TestReplacementSurvivesWalletRemovalFailure(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	inst, b, confirmer, store := newActorHarness(t, proof, desc)
	chain, ok := b.cfg.ChainSource.(*fakeChainSourceRef)
	require.True(t, ok)
	chain.setFeeEstimate(2, nil)
	oldID := driveLinearToSweep(t, inst.Ref(), confirmer, store, proof)
	mustAsk(t, inst.Ref(), &TxFailedMsg{
		Txid: oldID, Reason: "min relay fee not met",
		Class: txconfirm.BroadcastFailureFee,
	})
	inst.Stop()
	cfg := b.cfg
	failer := &persistentRemoveFailureRef{fakeChainSourceRef: chain}
	cfg.ChainSource = failer
	chain.setFeeEstimate(10, nil)
	inst, _ = restartSweepActor(t, cfg)
	mustAsk(t, inst.Ref(), &ResumeUnrollRequest{Height: 105})
	newID := confirmer.lastRequest(t).Tx.TxHash()
	require.NotEqual(t, oldID, newID)
	require.Equal(t, int32(1), failer.calls.Load())
	cp := mustDecodeCheckpoint(t, store, cfg.ActorID)
	require.Equal(t, oldID, cp.ReplacedSweeps[0].TxHash())
	require.Equal(t, 4, confirmer.requestCount())

	// A permanent rejection proves only that this replacement is invalid.
	// It cannot prove the prior candidate was never accepted elsewhere.
	for attempt := range maxSweepAttempts + 1 {
		mustAsk(t, inst.Ref(), &TxFailedMsg{
			Txid:   newID,
			Reason: "mandatory-script-verify-flag-failed",
			Class:  txconfirm.BroadcastFailurePermanent,
		})
		mustAsk(t, inst.Ref(), &HeightObservedMsg{
			Height: 106 + int32(attempt),
		})
	}
	cp = mustDecodeCheckpoint(t, store, cfg.ActorID)
	require.Empty(t, cp.Fail)
	require.Zero(t, cp.SweepAttempts)
	require.Equal(t, newID, cp.SweepTx.TxHash())
	inst.Stop()
	inst, _ = restartSweepActor(t, cfg)
	mustAsk(t, inst.Ref(), &SpendObservedMsg{
		Outpoint: proof.TargetOutpoint(), SpendingTxid: oldID,
		SpendingHeight: 111,
	})
	cp = mustDecodeCheckpoint(t, store, cfg.ActorID)
	require.Equal(t, unrollplan.SweepStatusConfirmed, cp.State.Sweep.Status)
	require.Equal(t, oldID, cp.SweepTx.TxHash())
	require.Empty(t, cp.Fail)
}
