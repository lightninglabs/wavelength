package oor

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
)

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

func TestDriveEventPayloadRequiresEvent(t *testing.T) {
	t.Parallel()

	_, err := encodeDriveEventRequestPayload(
		SessionID(chainhash.Hash{1, 2, 3}), nil,
	)
	require.ErrorContains(t, err, "event must be provided")
}
