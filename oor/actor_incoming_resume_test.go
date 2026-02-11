package oor

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// pausedIncomingAckHandler simulates an incoming ack transport that drops the
// first ack attempt and succeeds on resume.
type pausedIncomingAckHandler struct {
	t *testing.T

	ackPaused bool

	materializeCalls int
	ackCalls         int
}

// Handle processes incoming outbox requests and returns follow-up events.
func (h *pausedIncomingAckHandler) Handle(_ context.Context, _ SessionID,
	outbox OutboxEvent) ([]Event, error) {

	h.t.Helper()

	switch outbox.(type) {
	case *IncomingTransferNotification:
		return nil, nil

	case *MaterializeIncomingVTXOsRequest:
		h.materializeCalls++

		return []Event{
			&IncomingHandledEvent{},
		}, nil

	case *SendIncomingAckRequest:
		h.ackCalls++

		if !h.ackPaused {
			h.ackPaused = true
			return nil, nil
		}

		return []Event{
			&IncomingAckSentEvent{},
		}, nil

	default:
		return nil, nil
	}
}

var _ OutboxHandler = (*pausedIncomingAckHandler)(nil)

// TestOORClientActorIncomingResumeFromStore verifies incoming sessions can be
// resumed from durable store state without explicit restore logic.
func TestOORClientActorIncomingResumeFromStore(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	arkPSBT, _, _, _, _ := buildTestIncomingMaterialization(t)
	sessionID := SessionID(arkPSBT.UnsignedTx.TxHash())

	handler := &pausedIncomingAckHandler{t: t}
	incomingStore := NewInMemoryIncomingSessionStore()

	actor1 := NewOORClientActor(ClientActorCfg{
		OutboxHandler:        handler,
		IncomingSessionStore: incomingStore,
	})

	receiveResp := actor1.Receive(ctx, &ReceiveTransferRequest{
		SessionID: sessionID,
		ArkPSBT:   arkPSBT,
	})
	require.True(t, receiveResp.IsOk())

	stateResp := actor1.Receive(ctx, &GetIncomingStateRequest{
		SessionID: sessionID,
	})
	require.True(t, stateResp.IsOk())

	stateMsg, ok := stateResp.UnwrapOr(nil).(*GetIncomingStateResponse)
	require.True(t, ok)
	require.IsType(t, &ReceiveAwaitingAck{}, stateMsg.State)

	actor2 := NewOORClientActor(ClientActorCfg{
		OutboxHandler:        handler,
		IncomingSessionStore: incomingStore,
	})

	resumeResp := actor2.Receive(ctx, &ResumeIncomingRequest{
		SessionID: sessionID,
	})
	require.True(t, resumeResp.IsOk())

	finalResp := actor2.Receive(ctx, &GetIncomingStateRequest{
		SessionID: sessionID,
	})
	require.True(t, finalResp.IsOk())

	finalMsg, ok := finalResp.UnwrapOr(nil).(*GetIncomingStateResponse)
	require.True(t, ok)
	require.IsType(t, &ReceiveCompleted{}, finalMsg.State)

	require.Equal(t, 1, handler.materializeCalls)
	require.Equal(t, 2, handler.ackCalls)
}
