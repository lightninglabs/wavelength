package oor

import (
	"crypto/rand"
	"strings"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	oorlib "github.com/lightninglabs/darepo-client/lib/tx/oor"
	"github.com/stretchr/testify/require"
)

// TestActorGetOrCreateSessionFSMConcurrent verifies concurrent access to the
// session map safely converges on a single handle instance.
func TestActorGetOrCreateSessionFSMConcurrent(t *testing.T) {
	t.Parallel()

	const workers = 32

	ctx := t.Context()
	sessionID := SessionID(chainhash.Hash{1})
	actor := NewActor(ActorCfg{})

	handles := make(chan *sessionHandle, workers)
	errs := make(chan error, workers)

	var wg sync.WaitGroup
	wg.Add(workers)

	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()

			handle, err := actor.getOrCreateSessionFSM(
				ctx, sessionID,
			)
			if err != nil {
				errs <- err
				return
			}

			handles <- handle
		}()
	}

	wg.Wait()
	close(handles)
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}

	var first *sessionHandle
	for handle := range handles {
		if first == nil {
			first = handle
			continue
		}

		require.Same(t, first, handle)
	}

	actor.sessionsMu.RLock()
	require.Len(t, actor.sessions, 1)
	actor.sessionsMu.RUnlock()
}

// randomP2TRScript returns a P2TR pkScript with a random key.
func randomP2TRScript(t *testing.T) []byte {
	t.Helper()

	var key [32]byte
	_, err := rand.Read(key[:])
	require.NoError(t, err)

	return append([]byte{txscript.OP_1, 0x20}, key[:]...)
}

func randomCheckpointPolicy(t *testing.T) scripts.CheckpointPolicy {
	t.Helper()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	return scripts.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}
}

func makeValidSubmitPackage(t *testing.T,
	policy scripts.CheckpointPolicy) (*psbt.Packet, *psbt.Packet) {

	t.Helper()

	vtxoWitness := &wire.TxOut{
		Value:    5000,
		PkScript: randomP2TRScript(t),
	}

	checkpointResult, err := oorlib.BuildCheckpointPSBT(
		policy, oorlib.CheckpointInput{
			Outpoint: wire.OutPoint{
				Hash:  chainhash.Hash{1},
				Index: 7,
			},
			WitnessUtxo:     vtxoWitness,
			OwnerLeafScript: []byte{txscript.OP_TRUE},
		},
	)
	require.NoError(t, err)

	checkpointTx := checkpointResult.PSBT.UnsignedTx
	require.NotNil(t, checkpointTx)
	require.Len(t, checkpointTx.TxOut, 1)

	arkPsbt, err := oorlib.BuildArkPSBT([]oorlib.CheckpointOutput{
		{
			Txid:           checkpointTx.TxHash(),
			Output:         checkpointTx.TxOut[0],
			TapTreeEncoded: checkpointResult.TapTreeEncoded,
		},
	}, []oorlib.RecipientOutput{
		{
			PkScript: randomP2TRScript(t),
			Value:    btcutil.Amount(vtxoWitness.Value),
		},
	})
	require.NoError(t, err)

	return arkPsbt, checkpointResult.PSBT
}

// TestActorHappyPath exercises a submit and finalize flow through the actor
// using the in-process outbox driver.
func TestActorHappyPath(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	policy := randomCheckpointPolicy(t)
	arkPsbt, checkpointPsbt := makeValidSubmitPackage(t, policy)
	checkpointPsbt.Inputs[0].FinalScriptWitness = []byte{0x01}

	driver := NewInProcessOutboxDriver()
	actor := NewActor(ActorCfg{
		OutboxHandler:    driver,
		CheckpointPolicy: policy,
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

// TestActorSubmitMissingWitnessRejectedBeforeLock asserts submit package
// validation runs before any lock side effects.
func TestActorSubmitMissingWitnessRejectedBeforeLock(t *testing.T) {
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
	actor := NewActor(ActorCfg{
		OutboxHandler:    driver,
		CheckpointPolicy: randomCheckpointPolicy(t),
	})

	submitResp := actor.Receive(ctx, &SubmitOORRequest{
		ArkPSBT:         arkPsbt,
		CheckpointPSBTs: []*psbt.Packet{checkpointPsbt},
	})
	require.True(t, submitResp.IsErr())

	sessionID := SessionID(arkTx.TxHash())
	_, err = actor.CurrentState(ctx, sessionID)
	require.ErrorContains(t, err, "unknown session")

	seen := strings.Join(driver.SeenOutboxTypes(), ",")
	require.NotContains(t, seen, "LockInputsReq")
	require.NotContains(t, seen, "UnlockInputsReq")
}

// TestActorSubmitMissingTapTreeRejectedBeforeLock asserts submit package
// validation runs before any lock side effects.
func TestActorSubmitMissingTapTreeRejectedBeforeLock(t *testing.T) {
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
	actor := NewActor(ActorCfg{
		OutboxHandler:    driver,
		CheckpointPolicy: randomCheckpointPolicy(t),
	})

	submitResp := actor.Receive(ctx, &SubmitOORRequest{
		ArkPSBT:         arkPsbt,
		CheckpointPSBTs: []*psbt.Packet{checkpointPsbt},
	})
	require.True(t, submitResp.IsErr())

	sessionID := SessionID(arkTx.TxHash())
	_, err = actor.CurrentState(ctx, sessionID)
	require.ErrorContains(t, err, "unknown session")

	seen := strings.Join(driver.SeenOutboxTypes(), ",")
	require.NotContains(t, seen, "LockInputsReq")
	require.NotContains(t, seen, "UnlockInputsReq")
}

// TestActorFinalizeMissingSigDoesNotUnlock asserts that finalize failures after
// the point-of-no-return do not emit an unlock request.
func TestActorFinalizeMissingSigDoesNotUnlock(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	policy := randomCheckpointPolicy(t)
	arkPsbt, checkpointPsbt := makeValidSubmitPackage(t, policy)

	driver := NewInProcessOutboxDriver()
	actor := NewActor(ActorCfg{
		OutboxHandler:    driver,
		CheckpointPolicy: policy,
	})

	submitResp := actor.Receive(ctx, &SubmitOORRequest{
		ArkPSBT:         arkPsbt,
		CheckpointPSBTs: []*psbt.Packet{checkpointPsbt},
	})
	require.True(t, submitResp.IsOk())

	sessionID := SessionID(arkPsbt.UnsignedTx.TxHash())
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
