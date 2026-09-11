package unroll

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/db/actordelivery"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/recovery"
	"github.com/lightninglabs/wavelength/txconfirm"
	"github.com/lightninglabs/wavelength/unrollplan"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/stretchr/testify/require"
)

// signedSweepWallet signs the actual timeout leaf for script-engine validation.
type signedSweepWallet struct {
	fakeSweepWallet
	key *btcec.PrivateKey
}

// SignOutputRaw signs the complete transaction digest, including the new fee.
func (w *signedSweepWallet) SignOutputRaw(tx *wire.MsgTx,
	desc *input.SignDescriptor) (input.Signature, error) {

	digest, err := txscript.CalcTapscriptSignaturehash(
		desc.SigHashes, desc.HashType, tx, desc.InputIndex,
		desc.PrevOutputFetcher,
		txscript.NewBaseTapLeaf(desc.WitnessScript),
	)
	if err != nil {
		return nil, err
	}

	return schnorr.Sign(w.key, digest)
}

// feeFloorChain applies a deterministic relay floor after validating
// signatures. The real txconfirm actor handles its broadcast errors and
// notifications.
type feeFloorChain struct {
	*sweptSourceChain
	target    *wire.TxOut
	submitted chan *wire.MsgTx
	verified  chan error
}

// Ask checks every signed candidate before simulating low-fee node rejection.
func (c *feeFloorChain) Ask(ctx context.Context,
	msg chainsource.ChainSourceMsg,
) actor.Future[chainsource.ChainSourceResp] {

	req, ok := msg.(*chainsource.BroadcastTxRequest)
	if !ok {
		return c.sweptSourceChain.Ask(ctx, msg)
	}
	p := actor.NewPromise[chainsource.ChainSourceResp]()
	tx := req.Tx.Copy()
	prev := txscript.NewCannedPrevOutputFetcher(
		c.target.PkScript, c.target.Value,
	)
	engine, err := txscript.NewEngine(
		c.target.PkScript, tx, 0, txscript.StandardVerifyFlags, nil,
		txscript.NewTxSigHashes(tx, prev), c.target.Value, prev,
	)
	if err == nil {
		err = engine.Execute()
	}
	c.verified <- err
	if err == nil && c.target.Value-tx.TxOut[0].Value < 1000 {
		err = errors.New("min relay fee not met")
	}
	c.submitted <- tx
	if err != nil {
		p.Complete(fn.Err[chainsource.ChainSourceResp](err))
	} else {
		p.Complete(
			fn.Ok[chainsource.ChainSourceResp](
				&chainsource.BroadcastTxResponse{
					Txid: tx.TxHash(),
				},
			),
		)
	}

	return p.Future()
}

// signedSweepFixture creates a proof with the actual taproot timeout output.
func signedSweepFixture(t *testing.T) (*recovery.Proof, *vtxo.Descriptor,
	*signedSweepWallet) {

	t.Helper()
	original := buildLinearProof(t)
	desc := testDescriptor(
		t, original.TargetOutpoint(), original.CSVDelay(),
	)
	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	desc.ClientKey.PubKey = key.PubKey()
	desc.TapScript, err = arkscript.VTXOTapScript(
		key.PubKey(), desc.OperatorKey, desc.RelativeExpiry,
	)
	require.NoError(t, err)
	outputKey := txscript.ComputeTaprootOutputKey(
		desc.TapScript.ControlBlock.InternalKey,
		desc.TapScript.RootHash,
	)
	desc.PkScript, err = txscript.PayToTaprootScript(outputKey)
	require.NoError(t, err)
	root, _ := original.Node(original.RootTxids()[0])
	target, _ := original.Node(original.TargetOutpoint().Hash)
	tx := target.Tx.Copy()
	tx.TxOut[0].PkScript = desc.PkScript
	desc.Outpoint = wire.OutPoint{Hash: tx.TxHash()}
	proof, err := recovery.NewProof(
		desc.Outpoint, desc.RelativeExpiry, root, &recovery.Node{
			Kind: recovery.NodeKindTree,
			Tx:   tx,
		},
	)
	require.NoError(t, err)

	return proof, desc, &signedSweepWallet{key: key}
}

// TestFeeRejectedSweepDurableRestart covers both live typed rejection and a
// version-1 terminal checkpoint through SQLite, registry admission, the durable
// child and the production broadcaster. Completion requires chain evidence.
func TestFeeRejectedSweepDurableRestart(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%v", legacy), func(t *testing.T) {
			testFeeRejectedSweepDurableRestart(t, legacy)
		})
	}
}

// testFeeRejectedSweepDurableRestart runs one durable recovery scenario.
func testFeeRejectedSweepDurableRestart(t *testing.T, legacy bool) {
	t.Helper()
	proof, desc, wallet := signedSweepFixture(t)
	target := proof.TargetOutpoint()
	output, err := proof.TargetOutput()
	require.NoError(t, err)
	chain := &feeFloorChain{
		sweptSourceChain: &sweptSourceChain{
			fakeChainSourceRef: &fakeChainSourceRef{
				bestHeight: 104,
				feeRate:    2,
			},
			blocks: nil,
		},
		verified: make(chan error, 16),
		target:   output, submitted: make(
			chan *wire.MsgTx, 16,
		),
	}
	chain.blocks = make(
		map[string]actor.TellOnlyRef[chainsource.BlockEpoch],
	)
	sqlDB := db.NewTestDB(t)
	clk := clock.NewDefaultClock()
	stores := db.NewStore(
		sqlDB.DB, sqlDB.Queries, sqlDB.Backend(), btclog.Disabled,
	)
	delivery, err := actordelivery.NewTxAwareDeliveryStoreFromDB(
		sqlDB.DB, sqlDB.Backend(), clk, btclog.Disabled,
	)
	require.NoError(t, err)
	store := &DBRegistryStore{
		UEStore: stores.NewUnilateralExitStore(clk),
	}
	id := actorIDForTarget(target)
	cp := &actorCheckpoint{
		Version: 1, Height: 104, Started: true, Trigger: TriggerRestart,
		State: unrollplan.State{
			ConfirmedTxids: []chainhash.Hash{
				proof.RootTxids()[0],
				target.Hash,
			},
			TargetConfirmHeight: fn.Some[int32](102),
		},
	}
	row := RegistryRecord{TargetOutpoint: target, ActorID: id,
		Phase: PhaseSweepBroadcast, Trigger: TriggerRestart}
	var old *wire.MsgTx
	if legacy {
		old, err = buildSweepTx(
			t.Context(), wallet, chain, proof, desc, 0, 0, 104,
			NewStandardVTXOExitSpendPolicy(desc),
		)
		require.NoError(t, err)
		oldID := old.TxHash()
		cp.SweepTx = old
		cp.State.Sweep = unrollplan.SweepState{
			Status: unrollplan.SweepStatusBroadcasted,
			Txid: fn.Some(
				oldID,
			),
		}
		cp.Fail = "sweep tx " + oldID.String() +
			" failed: broadcast: min relay fee not met"
		cp.SweepAttempts = maxSweepAttempts
		row.Phase = PhaseFailed
		row.FailReason, row.SweepTxid = cp.Fail, &oldID
	}
	raw, err := encodeCheckpoint(cp)
	require.NoError(t, err)
	require.NoError(
		t,
		delivery.SaveCheckpoint(
			t.Context(), actor.CheckpointParams{
				ActorID:   id,
				StateType: checkpointStateType,
				StateData: raw,
				Version:   1,
			},
		),
	)
	require.NoError(t, store.UpsertRecord(t.Context(), row))
	txBehavior := txconfirm.NewTxBroadcasterActor(
		txconfirm.Config{
			ChainSource:           chain,
			FeeBumpIntervalBlocks: 1,
		},
	)
	txActor := actor.NewActor(actor.ActorConfig[
		txconfirm.Msg,
		txconfirm.Resp,
	]{
		ID:       "fee-restart-txconfirm",
		Behavior: txBehavior, MailboxSize: 64,
	})
	txBehavior.SetSelfRef(txActor.TellRef())
	txActor.Start()
	t.Cleanup(txActor.Stop)
	cfg := RegistryConfig{
		Store: store, DeliveryStore: delivery,
		ProofAssembler: &mockProofAssembler{
			proof: proof,
		},
		VTXOStore: &mockVTXOStore{
			desc: desc,
		},
		TxConfirmRef: txActor.Ref(), ChainSource: chain, Wallet: wallet,
	}
	registry := NewUnrollRegistryActor(cfg)
	t.Cleanup(func() { registry.Stop() })
	ensure := func() {
		t.Helper()
		resp, err := registry.Ref().Ask(
			t.Context(), &EnsureUnrollRequest{
				Outpoint: target, Trigger: TriggerRestart,
			},
		).Await(t.Context()).Unpack()
		require.NoError(t, err)
		admitted, ok := resp.(*EnsureUnrollResp)
		require.True(t, ok)
		require.Equal(t, id, admitted.ActorID)
		require.False(t, admitted.Created)
	}
	ensure()
	if !legacy {
		select {
		case old = <-chain.submitted:
			require.NoError(t, <-chain.verified)

		case <-time.After(testTimeout):
			t.Fatal("initial sweep not submitted")
		}
	}
	require.Eventually(t, func() bool {
		stored, err := delivery.LoadCheckpoint(
			t.Context(), id,
		)
		if err != nil || stored == nil {
			return false
		}
		decoded, err := decodeCheckpoint(
			stored.StateData,
		)

		return err == nil && decoded.Fail == "" &&
			decoded.RejectedSweep.UnwrapOr(
				chainhash.Hash{},
			) == old.TxHash()
	}, testTimeout, 10*time.Millisecond)
	registry.Stop()
	chain.setFeeEstimate(
		0, errors.New("estimator unavailable"),
	)
	registry = NewUnrollRegistryActor(cfg)
	require.NoError(
		t,
		registry.RestoreNonTerminal(
			t.Context(),
		),
	)
	ensure()
	ensure()
	require.Empty(t, chain.submitted)
	chain.setFeeEstimate(10, nil)
	chain.emitBlock(t, 105)
	var replacement *wire.MsgTx
	select {
	case replacement = <-chain.submitted:
		require.NoError(t, <-chain.verified)

	case <-time.After(testTimeout):
		t.Fatal("replacement not submitted")
	}
	require.NotEqual(t, old.TxHash(), replacement.TxHash())
	require.Equal(
		t, old.TxIn[0].PreviousOutPoint,
		replacement.TxIn[0].PreviousOutPoint,
	)
	require.Equal(
		t, old.TxOut[0].PkScript, replacement.TxOut[0].PkScript,
	)
	require.Less(
		t, replacement.TxOut[0].Value,
		old.TxOut[0].Value,
	)
	require.Equal(
		t, int64(1), wallet.pkScriptRequestCount(),
	)
	require.Eventually(t, func() bool {
		stored, err := delivery.LoadCheckpoint(
			t.Context(), id,
		)
		if err != nil || stored == nil {
			return false
		}
		decoded, err := decodeCheckpoint(
			stored.StateData,
		)

		if err != nil {
			return false
		}

		return decoded.State.Sweep.Status ==
			unrollplan.SweepStatusBroadcasted &&
			decoded.SweepTx.TxHash() == replacement.TxHash() &&
			decoded.Fail == ""
	}, testTimeout, 10*time.Millisecond)
	ensure()
	require.Empty(t, chain.submitted)
	rowBefore, err := store.GetRecord(t.Context(), target)
	require.NoError(t, err)
	require.False(t, rowBefore.IsTerminal())
	chain.emitSpendForOutpoint(
		t, target, replacement.TxHash(), 106,
	)
	require.Eventually(t, func() bool {
		record, err := store.GetRecord(
			t.Context(), target,
		)

		if err != nil || record == nil {
			return false
		}

		return record.Phase == PhaseCompleted &&
			record.SweepTxid != nil &&
			*record.SweepTxid == replacement.TxHash()
	}, testTimeout, 10*time.Millisecond)
	ensure()
	require.Empty(t, chain.submitted)
}
