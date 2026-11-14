package unroll

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/ledger"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestAssetUnrollWithholdsBitcoinSweep still materializes the presigned path
// while refusing both fresh and checkpoint-restored Bitcoin sweeps.
func TestAssetUnrollWithholdsBitcoinSweep(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	root := chainhash.Hash{1}
	desc.TaprootAssetRoot = &root
	unrollActor, behavior, txconfirmRef, _, exec := newActorHarnessExec(
		t, proof, desc,
	)
	mustAsk(t, unrollActor.Ref(), &StartUnrollRequest{
		Height: 100, Trigger: TriggerManual,
	})
	require.Equal(t, 1, txconfirmRef.requestCount())
	require.Equal(
		t, proof.RootTxids()[0],
		txconfirmRef.lastRequest(t).Tx.TxHash(),
	)
	_, err := behavior.resolveExitSpendPolicy(t.Context())
	require.ErrorIs(t, err, vtxo.ErrAssetVTXORequiresTransition)
	require.ErrorIs(
		t,
		behavior.startSweep(
			t.Context(), exec,
		),
		vtxo.ErrAssetVTXORequiresTransition,
	)
	behavior.sweepTx = wire.NewMsgTx(2)
	require.ErrorIs(
		t,
		behavior.startSweep(
			t.Context(), exec,
		),
		vtxo.ErrAssetVTXORequiresTransition,
	)
	require.Equal(t, 1, txconfirmRef.requestCount())
}

// fakeAssetClaimer returns a fixed claim once err is cleared.
type fakeAssetClaimer struct {
	mu    sync.Mutex
	tx    *wire.MsgTx
	err   error
	calls int
}

func (f *fakeAssetClaimer) ClaimAssetVTXO(context.Context, *vtxo.Descriptor,
	uint32) (*wire.MsgTx, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls++
	if f.err != nil {
		return nil, f.err
	}

	return f.tx, nil
}

func (f *fakeAssetClaimer) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.err = err
}

// TestAssetUnrollClaimsAsset checks that a matured asset VTXO is spent by the
// asset claimer's claim in place of a Bitcoin sweep, that a claim the claimer
// cannot produce yet is retried at the next block, and that the exit
// completes on the claim's confirmation without booking an exit cost.
func TestAssetUnrollClaimsAsset(t *testing.T) {
	proof := buildLinearProof(t)
	desc := testDescriptor(t, proof.TargetOutpoint(), proof.CSVDelay())
	root := chainhash.Hash{1}
	desc.TaprootAssetRoot = &root
	unrollActor, beh, txconfirmRef, _ := newActorHarness(t, proof, desc)

	claimTx := wire.NewMsgTx(3)
	claimTx.AddTxIn(&wire.TxIn{PreviousOutPoint: proof.TargetOutpoint()})
	claimTx.AddTxOut(wire.NewTxOut(1_000, []byte{0x51}))
	claimer := &fakeAssetClaimer{
		tx:  claimTx,
		err: errors.New("tapd unavailable"),
	}
	beh.cfg.AssetClaimer = claimer
	ledgerSink := actor.NewChannelTellOnlyRef[ledger.LedgerMsg](
		"unroll-ledger", 2,
	)
	beh.cfg.LedgerSink = fn.Some[ledger.Sink](ledgerSink)

	mustAsk(t, unrollActor.Ref(), &StartUnrollRequest{
		Height:  100,
		Trigger: TriggerManual,
	})
	txconfirmRef.emitConfirmed(t, 0, proof.RootTxids()[0], 101)
	require.Eventually(t, func() bool {
		return txconfirmRef.requestCountForTxid(
			proof.TargetOutpoint().Hash,
		) == 1
	}, testTimeout, 10*time.Millisecond)
	txconfirmRef.emitConfirmed(t, 1, proof.TargetOutpoint().Hash, 102)

	// The exit leaf has matured, but the claimer cannot claim yet.
	mustAsk(t, unrollActor.Ref(), &HeightObservedMsg{Height: 104})
	require.Eventually(t, func() bool {
		claimer.mu.Lock()
		defer claimer.mu.Unlock()

		return claimer.calls > 0
	}, testTimeout, 10*time.Millisecond)
	require.Equal(t, 2, txconfirmRef.requestCount())

	claimer.setErr(nil)
	mustAsk(t, unrollActor.Ref(), &HeightObservedMsg{Height: 105})
	require.Eventually(t, func() bool {
		return txconfirmRef.requestCount() == 3
	}, testTimeout, 10*time.Millisecond)
	submitted := txconfirmRef.lastRequest(t)
	require.Equal(t, claimTx.TxHash(), submitted.Tx.TxHash())
	require.True(t, submitted.RetryUntilAccepted)
	require.Equal(
		t, claimTx.TxOut[0].PkScript, submitted.ConfirmationPkScript,
	)

	// tapd leaves the claim's relay to the job, so the job tracks the
	// claim until it is final rather than until its first confirmation.
	require.Equal(
		t, chainsource.DefaultFinalityDepth, submitted.TargetConfs,
	)

	// The spend watch on the VTXO reports the claim at its first
	// confirmation. That is not final, so the job keeps waiting for
	// txconfirm.
	mustAsk(t, unrollActor.Ref(), &SpendObservedMsg{
		Outpoint:       proof.TargetOutpoint(),
		SpendingTxid:   claimTx.TxHash(),
		SpendingHeight: 106,
	})
	stateResp, ok := mustAsk(
		t, unrollActor.Ref(), &GetStateRequest{},
	).(*GetStateResp)
	require.True(t, ok)
	require.Equal(t, PhaseSweepConfirmation, stateResp.Phase)

	txconfirmRef.emitConfirmed(t, 2, claimTx.TxHash(), 111)
	require.Eventually(t, func() bool {
		stateResp, ok := mustAsk(
			t, unrollActor.Ref(), &GetStateRequest{},
		).(*GetStateResp)
		require.True(t, ok)

		return stateResp.Phase == PhaseCompleted
	}, testTimeout, 10*time.Millisecond)

	_, ok = ledgerSink.AwaitMessage(50 * time.Millisecond)
	require.False(t, ok, "an asset claim books no exit cost")
}
