package oor

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	oortx "github.com/lightninglabs/darepo-client/lib/tx/oor"
	"github.com/stretchr/testify/require"
)

// testOutboxHandler is a minimal in-process outbox handler for client actor
// tests. It simulates a server and wallet by returning follow-up events that
// drive the FSM forward.
type testOutboxHandler struct {
	t *testing.T
}

// Handle processes the outbox request and returns follow-up events.
func (h *testOutboxHandler) Handle(_ context.Context, sessionID SessionID,
	outbox OutboxEvent) ([]Event, error) {

	h.t.Helper()

	switch msg := outbox.(type) {
	case *SendSubmitPackageRequest:
		txid := msg.ArkPSBT.UnsignedTx.TxHash()
		require.Equal(h.t, SessionID(txid), sessionID)

		return []Event{&SubmitAcceptedEvent{
			SessionID:               sessionID,
			ArkPSBT:                 msg.ArkPSBT,
			CoSignedCheckpointPSBTs: msg.CheckpointPSBTs,
		}}, nil

	case *RequestCheckpointSignatures:
		finalCheckpoints := msg.CoSignedCheckpointPSBTs
		finalCheckpoints[0].Inputs[0].TaprootKeySpendSig = []byte{0x01}

		return []Event{&CheckpointsSignedEvent{
			FinalCheckpointPSBTs: finalCheckpoints,
		}}, nil

	case *SendFinalizePackageRequest:
		_ = msg
		return []Event{&FinalizeAcceptedEvent{}}, nil

	default:
		return nil, nil
	}
}

var _ OutboxHandler = (*testOutboxHandler)(nil)

// TestOORClientActorHappyPath exercises the outgoing transfer flow end-to-end
// using the client actor wrapper and a stub outbox handler.
func TestOORClientActorHappyPath(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := scripts.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	inputValue := btcutil.Amount(10000)

	inputs := []oortx.CheckpointInput{{
		Outpoint: wire.OutPoint{
			Hash:  [32]byte{0x01},
			Index: 0,
		},
		WitnessUtxo: &wire.TxOut{
			Value:    int64(inputValue),
			PkScript: []byte{0x51},
		},
		OwnerLeafScript: []byte{0x51},
	}}

	recipients := []oortx.RecipientOutput{{
		PkScript: []byte{0x51},
		Value:    inputValue,
	}}

	actor := NewOORClientActor(ClientActorCfg{
		OutboxHandler: &testOutboxHandler{t: t},
	})

	startResp := actor.Receive(ctx, &StartTransferRequest{
		Policy:     policy,
		Inputs:     inputs,
		Recipients: recipients,
	})
	require.True(t, startResp.IsOk())

	startMsg, ok := startResp.UnwrapOr(nil).(*StartTransferResponse)
	require.True(t, ok)
	require.NotEqual(t, SessionID{}, startMsg.SessionID)

	stateResp := actor.Receive(ctx, &GetStateRequest{
		SessionID: startMsg.SessionID,
	})
	require.True(t, stateResp.IsOk())

	stateMsg, ok := stateResp.UnwrapOr(nil).(*GetStateResponse)
	require.True(t, ok)
	require.IsType(t, &AwaitingLocalVTXOUpdate{}, stateMsg.State)
}
