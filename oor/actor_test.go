package oor

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	oortx "github.com/lightninglabs/darepo-client/lib/tx/oor"
	"github.com/lightningnetwork/lnd/input"
	"github.com/stretchr/testify/require"
)

// testOutboxHandler is a minimal in-process outbox handler for client actor
// tests. It simulates a server and wallet by returning follow-up events that
// drive the FSM forward.
type testOutboxHandler struct {
	t *testing.T

	clientSigner input.Signer
}

// Handle processes the outbox request and returns follow-up events.
func (h *testOutboxHandler) Handle(_ context.Context, sessionID SessionID,
	outbox OutboxEvent) ([]Event, error) {

	h.t.Helper()

	switch msg := outbox.(type) {
	case *SendSubmitPackageRequest:
		txid := msg.ArkPSBT.UnsignedTx.TxHash()
		require.Equal(h.t, SessionID(txid), sessionID)

		return []Event{
			&SubmitAcceptedEvent{
				SessionID:               sessionID,
				ArkPSBT:                 msg.ArkPSBT,
				CoSignedCheckpointPSBTs: msg.CheckpointPSBTs,
			},
		}, nil

	case *RequestCheckpointSignatures:
		err := SignCheckpointPSBTs(
			h.clientSigner, msg.TransferInputs,
			msg.CoSignedCheckpointPSBTs,
		)
		require.NoError(h.t, err)

		finalCheckpoints := msg.CoSignedCheckpointPSBTs

		return []Event{
			&CheckpointsSignedEvent{
				FinalCheckpointPSBTs: finalCheckpoints,
			},
		}, nil

	case *SendFinalizePackageRequest:
		_ = msg
		return []Event{
			&FinalizeAcceptedEvent{},
		}, nil

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

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	clientSigner := input.NewMockSigner([]*btcec.PrivateKey{clientKey}, nil)

	inputs := []TransferInput{
		newTestTransferInput(
			t, clientKey, policy.OperatorKey,
			wire.OutPoint{
				Hash:  [32]byte{0x01},
				Index: 0,
			},
			inputValue,
		),
	}

	recipients := []oortx.RecipientOutput{
		{
			PkScript: newTestTaprootPkScript(t, clientKey.PubKey()),
			Value:    inputValue,
		},
	}

	actor := NewOORClientActor(ClientActorCfg{
		OutboxHandler: &testOutboxHandler{
			t:            t,
			clientSigner: clientSigner,
		},
		DeliveryStore: newTestDeliveryStore(t),
		ActorID:       "oor-actor-test-happy",
	})
	defer actor.Stop()

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
