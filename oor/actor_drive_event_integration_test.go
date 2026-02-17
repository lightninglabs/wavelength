package oor

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
)

const (
	retryDueTestRetryAfter        = 2 * time.Second
	retryDueTestEventuallyTimeout = 2 * time.Second
	retryDueTestEventuallyPoll    = 20 * time.Millisecond
)

type retryDueIntegrationHandler struct {
	sawFinalize atomic.Bool
}

func (h *retryDueIntegrationHandler) Handle(_ context.Context,
	_ SessionID, outbox OutboxEvent) ([]Event, error) {

	switch outbox.(type) {
	case *SendFinalizePackageRequest:
		h.sawFinalize.Store(true)
		return nil, nil

	default:
		return nil, fmt.Errorf("unexpected outbox event: %T", outbox)
	}
}

var _ OutboxHandler = (*retryDueIntegrationHandler)(nil)

// TestOORClientActorDriveRetryDueEventDurablePath verifies the retry-due signal
// can cross the durable actor boundary and re-drive the resumed outbox work.
func TestOORClientActorDriveRetryDueEventDurablePath(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	ark, checkpoints := testOutboxPSBTPair(t)
	sessionID, err := sessionIDFromArk(ark)
	require.NoError(t, err)

	resumeSnapshot, err := NewOutgoingSnapshot(
		sessionID,
		&AwaitingFinalizeAccepted{
			SessionID:            sessionID,
			InputOutpoints:       []wire.OutPoint{{}},
			ArkPSBT:              ark,
			FinalCheckpointPSBTs: checkpoints,
		},
	)
	require.NoError(t, err)

	retrySnapshot := &OutgoingSnapshot{
		Version:        2,
		SessionID:      sessionID,
		Phase:          OutgoingPhaseRetryBackoff,
		RetryAfter:     retryDueTestRetryAfter,
		ResumeSnapshot: resumeSnapshot,
		FailReason:     "retry later",
	}

	handler := &retryDueIntegrationHandler{}
	actor := NewOORClientActor(ClientActorCfg{
		OutboxHandler: handler,
		DeliveryStore: newTestDeliveryStore(t),
		ActorID:       "oor-drive-retry-due-durable-path",
	})
	defer actor.Stop()

	restoreResp := actor.Receive(ctx, &RestoreSessionRequest{
		Snapshot: retrySnapshot,
	})
	require.True(t, restoreResp.IsOk())

	driveResp := actor.Receive(ctx, &DriveEventRequest{
		SessionID: sessionID,
		Event:     &RetryDueEvent{},
	})
	require.True(t, driveResp.IsOk())

	require.Eventually(
		t, func() bool {
			return handler.sawFinalize.Load()
		}, retryDueTestEventuallyTimeout, retryDueTestEventuallyPoll,
	)

	stateResp := actor.Receive(ctx, &GetStateRequest{
		SessionID: sessionID,
	})
	require.True(t, stateResp.IsOk())

	stateMsg, ok := stateResp.UnwrapOr(nil).(*GetStateResponse)
	require.True(t, ok)
	require.IsType(t, &AwaitingFinalizeAccepted{}, stateMsg.State)
}

// TestOutboxForStateRetryBackoff asserts retry-backoff state emits a
// schedule-retry request with the expected delay and reason.
func TestOutboxForStateRetryBackoff(t *testing.T) {
	t.Parallel()

	outbox, err := OutboxForState(&RetryBackoff{
		RetryAfter: 3 * time.Second,
		Reason:     "transport timeout",
	})
	require.NoError(t, err)
	require.Len(t, outbox, 1)

	scheduleMsg, ok := outbox[0].(*ScheduleRetryRequest)
	require.True(t, ok)
	require.Equal(t, 3*time.Second, scheduleMsg.After)
	require.Equal(t, "transport timeout", scheduleMsg.Reason)
}

// TestRetryBackoffProcessEventRetryDue asserts RetryDueEvent resumes from the
// stored snapshot and emits finalize outbox work.
func TestRetryBackoffProcessEventRetryDue(t *testing.T) {
	t.Parallel()

	ark, checkpoints := testOutboxPSBTPair(t)
	sessionID, err := sessionIDFromArk(ark)
	require.NoError(t, err)

	resumeSnapshot, err := NewOutgoingSnapshot(
		sessionID,
		&AwaitingFinalizeAccepted{
			SessionID:            sessionID,
			InputOutpoints:       []wire.OutPoint{{}},
			ArkPSBT:              ark,
			FinalCheckpointPSBTs: checkpoints,
		},
	)
	require.NoError(t, err)

	retryState := &RetryBackoff{
		ResumeSnapshot: resumeSnapshot,
		RetryAfter:     2 * time.Second,
		Reason:         "rpc timeout",
	}

	transition, err := retryState.ProcessEvent(
		t.Context(), &RetryDueEvent{}, nil,
	)
	require.NoError(t, err)
	require.IsType(t, &AwaitingFinalizeAccepted{}, transition.NextState)
	require.True(t, transition.NewEvents.IsSome())

	emitted := transition.NewEvents.UnsafeFromSome()
	require.Len(t, emitted.Outbox, 1)
	require.IsType(t, &SendFinalizePackageRequest{}, emitted.Outbox[0])
}

// TestRetryBackoffProcessEventRetryDueRequiresSnapshot asserts retry-due
// processing fails when no resume snapshot is present.
func TestRetryBackoffProcessEventRetryDueRequiresSnapshot(t *testing.T) {
	t.Parallel()

	retryState := &RetryBackoff{
		RetryAfter: 1 * time.Second,
	}

	transition, err := retryState.ProcessEvent(
		t.Context(), &RetryDueEvent{}, nil,
	)
	require.Nil(t, transition)
	require.ErrorContains(t, err, "resume snapshot must be provided")
}
