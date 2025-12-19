package oor

import (
	"context"
	"crypto/rand"
	"testing"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	oorlib "github.com/lightninglabs/darepo-client/lib/tx/oor"
	"github.com/stretchr/testify/require"
)

// happyOutboxHandler is a test stub that drives the OOR session FSM through the
// successful path by turning outbox requests into success events.
type happyOutboxHandler struct {
	ark *psbt.Packet
}

// Handle executes the given outbox request and returns follow-up success
// events.
func (h *happyOutboxHandler) Handle(ctx context.Context, sessionID SessionID,
	outbox OutboxEvent) ([]Event, error) {

	_ = ctx

	switch req := outbox.(type) {
	case *LockInputsReq:
		return []Event{&InputsLockSucceededEvent{}}, nil

	case *ValidateSubmitReq:
		// Validate via the shared client library.
		validated, err := oorlib.ValidateSubmitPackage(
			req.ArkPSBT, req.CheckpointPSBTs,
		)
		if err != nil {
			return []Event{
				&SubmitFailedEvent{
					Reason: err.Error(),
				},
			}, nil
		}

		return []Event{
			&SubmitValidatedEvent{
				ArkTxid: validated.ArkTxid,
			},
		}, nil

	case *CoSignReq:
		return []Event{&OperatorSignedEvent{}}, nil

	case *ValidateFinalizeReq:
		req, ok := outbox.(*ValidateFinalizeReq)
		if !ok {
			return nil, nil
		}

		// Validate finalize structurally: checkpoint set must match Ark
		// inputs and include signature material.
		err := oorlib.ValidateFinalizePackage(
			h.ark, req.FinalCheckpointPSBTs,
		)
		if err != nil {
			return []Event{
				&FinalizeFailedEvent{
					Reason: err.Error(),
				},
			}, nil
		}

		return []Event{&FinalizeValidatedEvent{}}, nil

	case *FinalizeReq:
		return []Event{&FinalizeSucceededEvent{}}, nil

	default:
		return nil, nil
	}
}

// randomP2TRScript returns a P2TR pkScript with a random key.
func randomP2TRScript(t *testing.T) []byte {
	t.Helper()

	var key [32]byte
	_, err := rand.Read(key[:])
	require.NoError(t, err)

	return append([]byte{txscript.OP_1, 0x20}, key[:]...)
}

// TestActorHappyPath exercises a submit and finalize flow through the actor.
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

	actor := NewActor(ActorCfg{
		OutboxHandler: &happyOutboxHandler{ark: arkPsbt},
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
