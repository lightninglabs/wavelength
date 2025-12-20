package oor

import (
	"context"
	"fmt"
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

	clientSigner   input.Signer
	operatorSigner input.Signer
}

// Handle processes the outbox request and returns follow-up events.
func (h *testOutboxHandler) Handle(_ context.Context, sessionID SessionID,
	outbox OutboxEvent) ([]Event, error) {

	h.t.Helper()

	switch msg := outbox.(type) {
	case *RequestArkSignatures:
		// Ark signing is modeled as an outbox boundary.
		// Unit tests treat this as a deterministic pass-through.
		return []Event{
			&ArkSignedEvent{
				ArkPSBT: msg.ArkPSBT,
			},
		}, nil

	case *SendSubmitPackageRequest:
		// The session ID is defined as the Ark txid, which means the
		// client can reconstruct it deterministically from PSBT bytes.
		txid := msg.ArkPSBT.UnsignedTx.TxHash()
		require.Equal(h.t, SessionID(txid), sessionID)

		err := coSignCheckpointPSBTsForTest(
			h.operatorSigner,
			msg.TransferInputs,
			msg.CheckpointPSBTs,
		)
		require.NoError(h.t, err)
		return []Event{
			&SubmitAcceptedEvent{
				SessionID:               sessionID,
				ArkPSBT:                 msg.ArkPSBT,
				CoSignedCheckpointPSBTs: msg.CheckpointPSBTs,
			},
		}, nil

	case *RequestCheckpointSignatures:
		// This simulates wallet-side signing.
		//
		// The FSM is expected to request that the application/wallet
		// layer attaches client signatures to the (server co-signed)
		// checkpoint PSBTs.
		err := SignCheckpointPSBTs(
			h.clientSigner, msg.TransferInputs,
			msg.CoSignedCheckpointPSBTs,
		)
		require.NoError(h.t, err)

		// After signing, we emit the event that drives the FSM into the
		// finalize step.
		finalCheckpoints := msg.CoSignedCheckpointPSBTs

		return []Event{
			&CheckpointsSignedEvent{
				FinalCheckpointPSBTs: finalCheckpoints,
			},
		}, nil

	case *SendFinalizePackageRequest:
		// Finalize is the last transport step: after this point, the
		// server is expected to persist the transfer's VTXO set update.
		//
		// In unit tests we model this as unconditional acceptance.
		_ = msg
		return []Event{
			&FinalizeAcceptedEvent{},
		}, nil

	case *MarkInputsSpentRequest:
		// Outgoing OOR transfers are off-chain.
		// Once finalize is accepted, the local wallet must record
		// that its inputs are spent.
		_ = msg
		return []Event{
			&InputsMarkedSpentEvent{},
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

	// This is a pure unit test: we use mock keys and a mock signer so the
	// test is deterministic and does not require an external wallet.
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
	operatorSigner := input.NewMockSigner(
		[]*btcec.PrivateKey{operatorKey}, nil,
	)

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

	// The actor wrapper is responsible for:
	// - creating a per-session FSM instance
	// - delivering outbox work to an application-provided handler
	// - driving follow-up events back into the FSM
	actor := NewOORClientActor(ClientActorCfg{
		OutboxHandler: &testOutboxHandler{
			t:              t,
			clientSigner:   clientSigner,
			operatorSigner: operatorSigner,
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

	// Verify the session reached a terminal state without requiring any
	// explicit "drive" calls by the test: outbox ↔ event feedback should
	// be sufficient for the happy path.
	stateResp := actor.Receive(ctx, &GetStateRequest{
		SessionID: startMsg.SessionID,
	})
	require.True(t, stateResp.IsOk())

	stateMsg, ok := stateResp.UnwrapOr(nil).(*GetStateResponse)
	require.True(t, ok)
	require.IsType(t, &Completed{}, stateMsg.State)
}

// retrySubmitOutboxHandler simulates a retryable transport error on the first
// submit attempt and verifies the FSM can back off and retry.
type retrySubmitOutboxHandler struct {
	t *testing.T

	clientSigner input.Signer

	submitAttempts int
}

// Handle processes the outbox request and returns follow-up events.
func (h *retrySubmitOutboxHandler) Handle(
	_ context.Context,
	sessionID SessionID,
	outbox OutboxEvent,
) ([]Event, error) {

	h.t.Helper()

	switch msg := outbox.(type) {
	case *RequestArkSignatures:
		return []Event{
			&ArkSignedEvent{
				ArkPSBT: msg.ArkPSBT,
			},
		}, nil

	case *SendSubmitPackageRequest:
		h.submitAttempts++

		// First attempt fails with a retryable error.
		if h.submitAttempts == 1 {
			return nil, NewRetryableOutboxError(
				fmt.Errorf("temporary transport error"),
				0,
			)
		}

		txid := msg.ArkPSBT.UnsignedTx.TxHash()
		require.Equal(h.t, SessionID(txid), sessionID)

		return []Event{
			&SubmitAcceptedEvent{
				SessionID:               sessionID,
				ArkPSBT:                 msg.ArkPSBT,
				CoSignedCheckpointPSBTs: msg.CheckpointPSBTs,
			},
		}, nil

	case *ScheduleRetryRequest:
		_ = msg

		// For unit tests, trigger the retry immediately.
		return []Event{
			&RetryDueEvent{},
		}, nil

	case *RequestCheckpointSignatures:
		err := SignCheckpointPSBTs(
			h.clientSigner, msg.TransferInputs,
			msg.CoSignedCheckpointPSBTs,
		)
		require.NoError(h.t, err)

		return []Event{
			&CheckpointsSignedEvent{
				FinalCheckpointPSBTs: msg.
					CoSignedCheckpointPSBTs,
			},
		}, nil

	case *SendFinalizePackageRequest:
		_ = msg

		return []Event{
			&FinalizeAcceptedEvent{},
		}, nil

	case *MarkInputsSpentRequest:
		_ = msg
		return []Event{
			&InputsMarkedSpentEvent{},
		}, nil

	default:
		return nil, nil
	}
}

var _ OutboxHandler = (*retrySubmitOutboxHandler)(nil)

// TestOORClientActorRetryBackoff asserts the client actor can handle a
// retryable error emitted by the outbox handler and complete after retry.
func TestOORClientActorRetryBackoff(t *testing.T) {
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
				Hash:  [32]byte{0x02},
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
		OutboxHandler: &retrySubmitOutboxHandler{
			t:            t,
			clientSigner: clientSigner,
		},
		DeliveryStore: newTestDeliveryStore(t),
		ActorID:       "oor-actor-retry-backoff",
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

	stateResp := actor.Receive(ctx, &GetStateRequest{
		SessionID: startMsg.SessionID,
	})
	require.True(t, stateResp.IsOk())

	stateMsg, ok := stateResp.UnwrapOr(nil).(*GetStateResponse)
	require.True(t, ok)
	require.IsType(t, &Completed{}, stateMsg.State)
}
