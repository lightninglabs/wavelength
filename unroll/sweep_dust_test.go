package unroll

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btcwallet/wallet/txrules"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/txconfirm"
	"github.com/lightninglabs/wavelength/unrollplan"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/stretchr/testify/require"
)

// dustSweepSigner counts signing so dust is rejected before wallet access.
type dustSweepSigner struct {
	fakeSweepWallet
	signs int
}

// SignOutputRaw records the policy's signing call.
func (w *dustSweepSigner) SignOutputRaw(tx *wire.MsgTx,
	desc *input.SignDescriptor) (input.Signature, error) {

	w.signs++

	return w.fakeSweepWallet.SignOutputRaw(tx, desc)
}

// TestStandardSweepDustBoundary proves script-specific dust boundaries,
// including equality, are enforced before signing a positive or empty payout.
func TestStandardSweepDustBoundary(t *testing.T) {
	for _, script := range []struct {
		name      string
		pkScript  []byte
		threshold int64
	}{
		{
			"taproot",
			append([]byte{txscript.OP_1, 32}, make([]byte, 32)...),
			330,
		},
		{
			"witness-key-hash",
			append([]byte{txscript.OP_0, 20}, make([]byte, 20)...),
			294,
		},
		{
			"key-hash",
			append(
				append(
					[]byte{
						txscript.OP_DUP,
						txscript.OP_HASH160, 20,
					},
					make([]byte, 20)...,
				),
				txscript.OP_EQUALVERIFY,
				txscript.OP_CHECKSIG,
			),
			546,
		},
	} {
		for _, value := range []int64{
			-1,
			0,
			1,
			script.threshold - 1,
			script.threshold,
		} {
			t.Run(fmt.Sprintf("%s/%d",
				script.name, value), func(t *testing.T) {
				proof := buildLinearProof(t)
				desc := testDescriptor(
					t, proof.TargetOutpoint(),
					proof.CSVDelay(),
				)
				wallet := &dustSweepSigner{}
				policy := NewStandardVTXOExitSpendPolicy(desc)
				request := ExitSpendRequest{
					TargetOutpoint: desc.Outpoint,
					TargetOutput: &wire.TxOut{
						Value: estimatedSweepVBytes +
							value,
						PkScript: desc.PkScript,
					},
					DestinationPkScript: script.pkScript,
					FeeRateSatPerVByte:  1,
					Signer:              wallet,
				}
				tx, err := policy.BuildSpendTx(
					t.Context(), request,
				)
				if value < script.threshold {
					require.ErrorIs(
						t, err, ErrExitSpendDust,
					)
					require.Nil(t, tx)
					require.Zero(t, wallet.signs)

					return
				}
				require.NoError(t, err)
				require.Equal(t, 1, wallet.signs)
				require.Equal(t, value, tx.TxOut[0].Value)
				require.False(
					t, txrules.IsDustOutput(
						tx.TxOut[0],
						txrules.DefaultRelayFeePerKb,
					),
				)
			})
		}
	}
}

// dustSweepConfirmRef observes durable cancellation and can reject a repair
// admission after staging, exercising replay across both IO boundaries.
type dustSweepConfirmRef struct {
	*fakeTxConfirmRef
	store      *memCheckpointStore
	oldID      chainhash.Hash
	cancels    atomic.Int64
	ordered    atomic.Bool
	failRepair atomic.Bool
	failNext   atomic.Bool
}

// Ask requires durable rejection before cancellation and can fail either the
// cancellation or repair admission without recording successful submission.
func (f *dustSweepConfirmRef) Ask(ctx context.Context,
	msg txconfirm.Msg) actor.Future[txconfirm.Resp] {

	fail := false
	if req, ok := msg.(*txconfirm.CancelInterestReq); ok {
		cp, err := f.store.LoadCheckpoint(ctx, "unroll-test")
		if err == nil && cp != nil {
			job, err := decodeCheckpoint(cp.StateData)
			if err == nil && job.RejectedSweep.IsSome() &&
				job.RejectedSweep.UnsafeFromSome() == f.oldID &&
				req.Txid == f.oldID {

				f.ordered.Store(true)
			}
		}
		fail = f.failNext.Swap(false)
		if !fail {
			f.cancels.Add(1)
		}
	}
	if req, ok := msg.(*txconfirm.EnsureConfirmedReq); ok &&
		req.Tx.TxHash() != f.oldID {

		fail = f.failRepair.Swap(false)
	}
	if fail {
		p := actor.NewPromise[txconfirm.Resp]()
		p.Complete(
			fn.Err[txconfirm.Resp](
				errors.New("confirmation service unavailable"),
			),
		)

		return p.Future()
	}

	return f.fakeTxConfirmRef.Ask(ctx, msg)
}

// TestPersistedDustSweepRecovery preserves the original claim and destination
// while quarantining a legacy dust sweep, deferring uneconomic quotes, and
// recovering either candidate across restart and staged-repair admission
// failure.
func TestPersistedDustSweepRecovery(t *testing.T) {
	for _, staged := range []bool{false, true} {
		for _, oldWins := range []bool{false, true} {
			t.Run(fmt.Sprintf(
				"staged=%v/old-wins=%v",
				staged, oldWins), func(t *testing.T) {
				proof := buildLinearProof(t)
				desc := testDescriptor(
					t, proof.TargetOutpoint(),
					proof.CSVDelay(),
				)
				inst, b, confirmer, store := newActorHarness(
					t, proof, desc,
				)
				driveLinearToSweep(
					t, inst.Ref(), confirmer, store, proof,
				)
				inst.Stop()
				legacy := seedLegacyDustSweep(
					t, b, store, staged,
				)
				oldID := legacy.TxHash()
				var cp *actorCheckpoint
				tracker := &dustSweepConfirmRef{
					fakeTxConfirmRef: confirmer,
					store:            store,
					oldID:            oldID,
				}
				cfg := b.cfg
				cfg.TxConfirmRef = tracker
				source := cfg.ChainSource
				chain, ok := source.(*fakeChainSourceRef)
				require.True(t, ok)
				wallet, ok := cfg.Wallet.(*fakeSweepWallet)
				require.True(t, ok)
				// Raising the cap makes this quote consume the
				// entire input.
				cfg.MaxSweepFeeRateSatPerVByte = 300
				chain.setFeeEstimate(250, nil)
				inst, _ = restartSweepActor(t, cfg)
				mustAsk(
					t, inst.Ref(), &ResumeUnrollRequest{
						Height: 105,
					},
				)
				require.True(t, tracker.ordered.Load())
				require.Equal(
					t, int64(1), tracker.cancels.Load(),
				)
				require.Equal(t, 3, confirmer.requestCount())
				cp = mustDecodeCheckpoint(t, store, cfg.ActorID)
				require.Equal(
					t, oldID,
					cp.RejectedSweep.UnsafeFromSome(),
				)
				require.Equal(
					t, unrollplan.SweepStatusPending,
					cp.State.Sweep.Status,
				)
				require.Equal(t, 0, cp.SweepAttempts)
				require.Empty(t, cp.Fail)
				require.Empty(t, chain.removedTxSnapshot())

				// A too-expensive quote and an outage both keep
				// the original signed candidate without
				// deriving another address. An acknowledged
				// cancellation no longer depends on the
				// service.
				tracker.failNext.Store(true)
				mustAsk(
					t, inst.Ref(), &HeightObservedMsg{
						Height: 106,
					},
				)
				chain.setFeeEstimate(
					0, errors.New("estimator unavailable"),
				)
				for height := int32(
					107,
				); height < 112; height++ {

					mustAsk(
						t, inst.Ref(),
						&HeightObservedMsg{
							Height: height,
						},
					)
				}
				require.Equal(t, 3, confirmer.requestCount())
				require.Equal(
					t, int64(1),
					wallet.pkScriptRequestCount(),
				)
				require.Equal(
					t, int64(1), tracker.cancels.Load(),
				)
				require.True(t, tracker.failNext.Load())
				tracker.failNext.Store(false)
				cp = mustDecodeCheckpoint(t, store, cfg.ActorID)
				require.Equal(t, oldID, cp.SweepTx.TxHash())
				require.Empty(t, cp.ReplacedSweeps)
				require.Zero(t, cp.SweepAttempts)
				require.Empty(t, cp.Fail)

				// A lower fee repairs the dust payout. Crash
				// after staging it but before the confirmation
				// service admits the new candidate.
				tracker.failRepair.Store(true)
				chain.setFeeEstimate(2, nil)
				_, err := inst.
					Ref().
					Ask(
						t.Context(),
						&HeightObservedMsg{Height: 112},
					).
					Await(t.Context()).
					Unpack()
				require.ErrorContains(
					t, err,
					"confirmation service unavailable",
				)
				cp = mustDecodeCheckpoint(t, store, cfg.ActorID)
				newID := cp.SweepTx.TxHash()
				require.NotEqual(t, oldID, newID)
				require.Len(t, cp.ReplacedSweeps, 1)
				requireSameSweep(
					t, legacy, cp.ReplacedSweeps[0],
				)
				require.Equal(
					t, legacy.TxIn[0].PreviousOutPoint,
					cp.SweepTx.TxIn[0].PreviousOutPoint,
				)
				require.Equal(
					t, legacy.TxOut[0].PkScript,
					cp.SweepTx.TxOut[0].PkScript,
				)
				require.False(t, sweepHasDustOutput(cp.SweepTx))
				require.Equal(
					t, int64(49_600),
					cp.SweepTx.TxOut[0].Value,
				)
				require.Equal(t, 3, confirmer.requestCount())
				inst.Stop()
				chain.setFeeEstimate(
					0, errors.New("estimator unavailable"),
				)
				calls := chain.feeRequestCount()
				inst, _ = restartSweepActor(t, cfg)
				mustAsk(
					t, inst.Ref(), &ResumeUnrollRequest{
						Height: 113,
					},
				)
				require.Equal(t, calls, chain.feeRequestCount())
				require.Equal(
					t, newID,
					confirmer.lastRequest(t).Tx.TxHash(),
				)
				require.Equal(
					t, int64(1),
					wallet.pkScriptRequestCount(),
				)
				require.Equal(t, 4, confirmer.requestCount())
				require.Equal(
					t, []chainhash.Hash{oldID, oldID},
					chain.removedTxSnapshot(),
				)
				mustAsk(
					t, inst.Ref(), &TxFailedMsg{
						Txid:   oldID,
						Reason: "dust",
					},
				)
				require.Equal(t, 4, confirmer.requestCount())
				inst.Stop()
				inst, _ = restartSweepActor(t, cfg)
				winner := newID
				if oldWins {
					winner = oldID
				}
				mustAsk(
					t, inst.Ref(), &SpendObservedMsg{
						Outpoint:       desc.Outpoint,
						SpendingTxid:   winner,
						SpendingHeight: 114,
					},
				)
				cp = mustDecodeCheckpoint(t, store, cfg.ActorID)
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

// saveDustCheckpoint seeds only the isolated actor store with a legacy fixture.
func saveDustCheckpoint(t *testing.T, store *memCheckpointStore,
	cp *actorCheckpoint) {

	t.Helper()
	raw, err := encodeCheckpoint(cp)
	require.NoError(t, err)
	require.NoError(
		t,
		store.SaveCheckpoint(
			t.Context(), actor.CheckpointParams{
				ActorID:   "unroll-test",
				StateType: checkpointStateType,
				StateData: raw,
				Version:   int64(cp.Version),
			},
		),
	)
}

// TestUneconomicSweepDefersWithoutFailing keeps a newly materialized input
// pending when fees leave dust or no payout, then recovers after fees fall.
func TestUneconomicSweepDefersWithoutFailing(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	inst, b, confirmer, store := newActorHarness(t, proof, desc)
	chain, ok := b.cfg.ChainSource.(*fakeChainSourceRef)
	require.True(t, ok)
	wallet := &dustSweepSigner{}
	b.cfg.Wallet = wallet
	b.cfg.MaxSweepFeeRateSatPerVByte = 300
	chain.setFeeEstimate(249, nil)
	mustAsk(
		t, inst.Ref(), &StartUnrollRequest{
			Height:  100,
			Trigger: TriggerManual,
		},
	)
	mustAsk(
		t, inst.Ref(), &TxConfirmedMsg{
			Txid:   proof.RootTxids()[0],
			Height: 101,
		},
	)
	mustAsk(
		t, inst.Ref(), &TxConfirmedMsg{
			Txid:   proof.TargetOutpoint().Hash,
			Height: 102,
		},
	)
	for height := int32(104); height < 110; height++ {
		chain.setFeeEstimate(249+int64(height%3), nil)
		mustAsk(t, inst.Ref(), &HeightObservedMsg{Height: height})
	}
	cp := mustDecodeCheckpoint(t, store, "unroll-test")
	require.Nil(t, cp.SweepTx)
	require.Zero(t, cp.SweepAttempts)
	require.Empty(t, cp.Fail)
	require.Zero(t, wallet.signs)
	require.Equal(t, int64(1), wallet.pkScriptRequestCount())
	require.Equal(t, 2, confirmer.requestCount())
	state, ok := mustAsk(
		t, inst.Ref(), &GetStateRequest{},
	).(*GetStateResp)
	require.True(t, ok)
	require.Equal(t, PhaseSweepBroadcast, state.Phase)
	chain.setFeeEstimate(2, nil)
	mustAsk(t, inst.Ref(), &HeightObservedMsg{Height: 110})
	require.Equal(t, 3, confirmer.requestCount())
	require.Equal(t, 1, wallet.signs)
	require.Equal(t, int64(1), wallet.pkScriptRequestCount())
	require.False(t, sweepHasDustOutput(confirmer.lastRequest(t).Tx))
}

// TestDustSweepCancellationReplay proves that a lost cancellation ack cannot
// resurrect a dust broadcast or erase a claim that confirms while deferred.
func TestDustSweepCancellationReplay(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	inst, b, confirmer, store := newActorHarness(t, proof, desc)
	driveLinearToSweep(t, inst.Ref(), confirmer, store, proof)
	inst.Stop()
	legacy := seedLegacyDustSweep(t, b, store, false)
	cfg := b.cfg
	tracker := &dustSweepConfirmRef{
		fakeTxConfirmRef: confirmer, store: store,
		oldID: legacy.TxHash(),
	}
	tracker.failNext.Store(true)
	cfg.TxConfirmRef = tracker
	chain, ok := cfg.ChainSource.(*fakeChainSourceRef)
	require.True(t, ok)
	chain.setFeeEstimate(0, errors.New("estimator unavailable"))
	inst, _ = restartSweepActor(t, cfg)
	_, err := inst.
		Ref().
		Ask(t.Context(), &ResumeUnrollRequest{Height: 105}).
		Await(t.Context()).
		Unpack()
	require.ErrorContains(t, err, "confirmation service unavailable")
	cp := mustDecodeCheckpoint(t, store, cfg.ActorID)
	require.Equal(t, legacy.TxHash(), cp.RejectedSweep.UnsafeFromSome())
	require.Equal(t, unrollplan.SweepStatusPending, cp.State.Sweep.Status)
	require.Zero(t, cp.SweepAttempts)
	require.True(t, tracker.ordered.Load())
	require.Zero(t, tracker.cancels.Load())
	inst.Stop()
	inst, _ = restartSweepActor(t, cfg)
	mustAsk(t, inst.Ref(), &ResumeUnrollRequest{Height: 105})
	require.Equal(t, int64(1), tracker.cancels.Load())
	tracker.failNext.Store(true)
	mustAsk(t, inst.Ref(), &HeightObservedMsg{Height: 106})
	require.True(t, tracker.failNext.Load())
	require.Equal(t, int64(1), tracker.cancels.Load())
	require.Equal(t, 3, confirmer.requestCount())
	require.Empty(t, chain.removedTxSnapshot())
	// Standardness is relay policy. A miner can still confirm the old
	// transaction, so its source spend watch must complete this claim.
	mustAsk(t, inst.Ref(), &SpendObservedMsg{
		Outpoint: proof.TargetOutpoint(), SpendingTxid: legacy.TxHash(),
		SpendingHeight: 107,
	})
	cp = mustDecodeCheckpoint(t, store, cfg.ActorID)
	require.Equal(t, unrollplan.SweepStatusConfirmed, cp.State.Sweep.Status)
	require.Equal(t, legacy.TxHash(), cp.SweepTx.TxHash())
	require.Empty(t, cp.Fail)
}

// seedLegacyDustSweep retains a signed legacy candidate in a version-one
// checkpoint, with either an acknowledged submission or only a staged build.
func seedLegacyDustSweep(t *testing.T, b *behavior, store *memCheckpointStore,
	staged bool) *wire.MsgTx {

	t.Helper()
	cp := mustDecodeCheckpoint(t, store, "unroll-test")
	legacy := cp.SweepTx.Copy()
	legacy.TxOut[0].Value = 1
	// Re-sign through the old builder's signing boundary. The new
	// construction guard must never create this fixture.
	target, err := b.proof.TargetOutput()
	require.NoError(t, err)
	policy := NewStandardVTXOExitSpendPolicy(b.desc)
	request := ExitSpendRequest{
		TargetOutput: target, Signer: b.cfg.Wallet,
	}
	legacy, err = policy.signSpendTx(request, legacy)
	require.NoError(t, err)
	cp.SweepTx = legacy
	cp.State.Sweep.Txid = fn.Some(legacy.TxHash())
	if staged {
		cp.State.Sweep.Status = unrollplan.SweepStatusPending
		cp.State.Sweep.Txid = fn.None[chainhash.Hash]()
	}
	cp.Version = 1
	saveDustCheckpoint(t, store, cp)

	return legacy
}
