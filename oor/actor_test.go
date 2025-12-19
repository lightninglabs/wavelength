package oor

import (
	"crypto/rand"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	oorlib "github.com/lightninglabs/darepo-client/lib/tx/oor"
	"github.com/stretchr/testify/require"
)

// randomP2TRScript returns a P2TR pkScript with a random key.
func randomP2TRScript(t *testing.T) []byte {
	t.Helper()

	var key [32]byte
	_, err := rand.Read(key[:])
	require.NoError(t, err)

	return append([]byte{txscript.OP_1, 0x20}, key[:]...)
}

// TestActorHappyPath exercises a submit and finalize flow through the actor
// using the in-process outbox driver.
func TestActorHappyPath(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	checkpointTx := wire.NewMsgTx(2)
	checkpointTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{1},
			Index: 7,
		},
	})
	checkpointTx.AddTxOut(&wire.TxOut{
		Value:    1234,
		PkScript: randomP2TRScript(t),
	})

	checkpointPsbt, err := psbt.NewFromUnsignedTx(checkpointTx)
	require.NoError(t, err)

	checkpointOutpoint := wire.OutPoint{
		Hash:  checkpointTx.TxHash(),
		Index: 0,
	}

	arkTx := wire.NewMsgTx(2)
	arkTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: checkpointOutpoint,
	})
	arkTx.AddTxOut(&wire.TxOut{
		Value:    1234,
		PkScript: randomP2TRScript(t),
	})
	arkTx.AddTxOut(scripts.AnchorOutput())

	arkPsbt, err := psbt.NewFromUnsignedTx(arkTx)
	require.NoError(t, err)

	arkPsbt.Inputs[0].WitnessUtxo = checkpointTx.TxOut[0]

	encodedTapTree, err := oorlib.EncodeTapTree([][]byte{{0x51}})
	require.NoError(t, err)

	err = oorlib.PutTapTreePSBTInput(arkPsbt, 0, encodedTapTree)
	require.NoError(t, err)

	checkpointPsbt.Inputs[0].FinalScriptWitness = []byte{0x01}

	driver := NewInProcessOutboxDriver()
	actor := NewActor(ActorCfg{
		OutboxHandler: driver,
	})

	submitResp := actor.Receive(ctx, &SubmitOORRequest{
		ArkPSBT:         arkPsbt,
		CheckpointPSBTs: []*psbt.Packet{checkpointPsbt},
	})
	require.True(t, submitResp.IsOk())

	submitMsg, ok := submitResp.UnwrapOr(nil).(*SubmitOORResponse)
	require.True(t, ok)

	finalizeResp := actor.Receive(ctx, &FinalizeOORRequest{
		SessionID:            submitMsg.SessionID,
		FinalCheckpointPSBTs: []*psbt.Packet{checkpointPsbt},
	})
	require.True(t, finalizeResp.IsOk())

	state, err := actor.CurrentState(ctx, submitMsg.SessionID)
	require.NoError(t, err)
	require.IsType(t, &FinalizedState{}, state)
}

// TestActorSubmitMissingWitnessAssertsUnlock exercises a submit that fails
// validation because the Ark PSBT input does not include a witness UTXO.
func TestActorSubmitMissingWitnessAssertsUnlock(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	checkpointTx := wire.NewMsgTx(2)
	checkpointTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{1},
			Index: 7,
		},
	})
	checkpointTx.AddTxOut(&wire.TxOut{
		Value:    1234,
		PkScript: randomP2TRScript(t),
	})

	checkpointPsbt, err := psbt.NewFromUnsignedTx(checkpointTx)
	require.NoError(t, err)

	checkpointOutpoint := wire.OutPoint{
		Hash:  checkpointTx.TxHash(),
		Index: 0,
	}

	arkTx := wire.NewMsgTx(2)
	arkTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: checkpointOutpoint,
	})
	arkTx.AddTxOut(&wire.TxOut{
		Value:    1234,
		PkScript: randomP2TRScript(t),
	})
	arkTx.AddTxOut(scripts.AnchorOutput())

	arkPsbt, err := psbt.NewFromUnsignedTx(arkTx)
	require.NoError(t, err)

	encodedTapTree, err := oorlib.EncodeTapTree([][]byte{{0x51}})
	require.NoError(t, err)

	err = oorlib.PutTapTreePSBTInput(arkPsbt, 0, encodedTapTree)
	require.NoError(t, err)

	driver := NewInProcessOutboxDriver()
	actor := NewActor(ActorCfg{OutboxHandler: driver})

	submitResp := actor.Receive(ctx, &SubmitOORRequest{
		ArkPSBT:         arkPsbt,
		CheckpointPSBTs: []*psbt.Packet{checkpointPsbt},
	})
	require.True(t, submitResp.IsOk())

	sessionID := SessionID(arkTx.TxHash())
	state, err := actor.CurrentState(ctx, sessionID)
	require.NoError(t, err)
	require.IsType(t, &FailedState{}, state)

	seen := strings.Join(driver.SeenOutboxTypes(), ",")
	require.Contains(t, seen, "UnlockInputsReq")
}

// TestActorSubmitMissingTapTreeAssertsUnlock exercises a submit that fails
// validation because the Ark PSBT input does not include tap tree metadata.
func TestActorSubmitMissingTapTreeAssertsUnlock(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	checkpointTx := wire.NewMsgTx(2)
	checkpointTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{1},
			Index: 7,
		},
	})
	checkpointTx.AddTxOut(&wire.TxOut{
		Value:    1234,
		PkScript: randomP2TRScript(t),
	})

	checkpointPsbt, err := psbt.NewFromUnsignedTx(checkpointTx)
	require.NoError(t, err)

	checkpointOutpoint := wire.OutPoint{
		Hash:  checkpointTx.TxHash(),
		Index: 0,
	}

	arkTx := wire.NewMsgTx(2)
	arkTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: checkpointOutpoint,
	})
	arkTx.AddTxOut(&wire.TxOut{
		Value:    1234,
		PkScript: randomP2TRScript(t),
	})
	arkTx.AddTxOut(scripts.AnchorOutput())

	arkPsbt, err := psbt.NewFromUnsignedTx(arkTx)
	require.NoError(t, err)

	// Submit validation requires witness utxo and taptree metadata. We
	// only set witness utxo here to prove missing taptree triggers failure.
	arkPsbt.Inputs[0].WitnessUtxo = checkpointTx.TxOut[0]

	driver := NewInProcessOutboxDriver()
	actor := NewActor(ActorCfg{OutboxHandler: driver})

	submitResp := actor.Receive(ctx, &SubmitOORRequest{
		ArkPSBT:         arkPsbt,
		CheckpointPSBTs: []*psbt.Packet{checkpointPsbt},
	})
	require.True(t, submitResp.IsOk())

	sessionID := SessionID(arkTx.TxHash())
	state, err := actor.CurrentState(ctx, sessionID)
	require.NoError(t, err)
	require.IsType(t, &FailedState{}, state)

	seen := strings.Join(driver.SeenOutboxTypes(), ",")
	require.Contains(t, seen, "UnlockInputsReq")
}

// TestActorFinalizeMissingSigDoesNotUnlock asserts that finalize failures after
// the point-of-no-return do not emit an unlock request.
func TestActorFinalizeMissingSigDoesNotUnlock(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	checkpointTx := wire.NewMsgTx(2)
	checkpointTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{1},
			Index: 7,
		},
	})
	checkpointTx.AddTxOut(&wire.TxOut{
		Value:    1234,
		PkScript: randomP2TRScript(t),
	})

	checkpointPsbt, err := psbt.NewFromUnsignedTx(checkpointTx)
	require.NoError(t, err)

	checkpointOutpoint := wire.OutPoint{
		Hash:  checkpointTx.TxHash(),
		Index: 0,
	}

	arkTx := wire.NewMsgTx(2)
	arkTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: checkpointOutpoint,
	})
	arkTx.AddTxOut(&wire.TxOut{
		Value:    1234,
		PkScript: randomP2TRScript(t),
	})
	arkTx.AddTxOut(scripts.AnchorOutput())

	arkPsbt, err := psbt.NewFromUnsignedTx(arkTx)
	require.NoError(t, err)

	arkPsbt.Inputs[0].WitnessUtxo = checkpointTx.TxOut[0]

	encodedTapTree, err := oorlib.EncodeTapTree([][]byte{{0x51}})
	require.NoError(t, err)

	err = oorlib.PutTapTreePSBTInput(arkPsbt, 0, encodedTapTree)
	require.NoError(t, err)

	driver := NewInProcessOutboxDriver()
	actor := NewActor(ActorCfg{OutboxHandler: driver})

	submitResp := actor.Receive(ctx, &SubmitOORRequest{
		ArkPSBT:         arkPsbt,
		CheckpointPSBTs: []*psbt.Packet{checkpointPsbt},
	})
	require.True(t, submitResp.IsOk())

	sessionID := SessionID(arkTx.TxHash())
	state, err := actor.CurrentState(ctx, sessionID)
	require.NoError(t, err)
	require.IsType(t, &CoSignedState{}, state)

	finalizeResp := actor.Receive(ctx, &FinalizeOORRequest{
		SessionID:            sessionID,
		FinalCheckpointPSBTs: []*psbt.Packet{checkpointPsbt},
	})
	require.True(t, finalizeResp.IsOk())

	state, err = actor.CurrentState(ctx, sessionID)
	require.NoError(t, err)
	require.IsType(t, &FailedState{}, state)

	seen := strings.Join(driver.SeenOutboxTypes(), ",")
	require.NotContains(t, seen, "UnlockInputsReq")
}
