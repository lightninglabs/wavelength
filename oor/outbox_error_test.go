package oor

import (
	"math"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	oortx "github.com/lightninglabs/darepo-client/lib/tx/oor"
	"github.com/stretchr/testify/require"
)

// TestHandleOutboxError asserts retry and failure transitions are derived
// deterministically from outbox error events.
func TestHandleOutboxError(t *testing.T) {
	t.Parallel()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	// Build a minimal, deterministic transfer package. We validate outbox
	// error handling independently of any transport: the important part is
	// that the current state contains enough information to retry safely
	// (or to fail terminally in a deterministic way).
	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	inputValue := btcutil.Amount(10_000)

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	inputs := []TransferInput{
		newTestTransferInput(
			t,
			clientKey,
			policy.OperatorKey,
			wire.OutPoint{
				Hash:  [32]byte{0x01},
				Index: 0,
			},
			inputValue,
		),
	}

	recipients := []oortx.RecipientOutput{{
		PkScript: newTestTaprootPkScript(t, clientKey.PubKey()),
		Value:    inputValue,
	}}

	ark, checkpoints, err := buildSubmitPackage(policy, inputs, recipients)
	require.NoError(t, err)
	require.NotNil(t, ark)
	require.NotEmpty(t, checkpoints)

	current := &AwaitingSubmitAccepted{
		ArkPSBT:         ark,
		CheckpointPSBTs: checkpoints,
		TransferInputs:  inputs,
	}

	// Nil event rejected: this is a programmer error and should not be
	// treated as retryable.
	_, err = handleOutboxError(nil, current, nil)
	require.Error(t, err)

	// Non-retryable error causes terminal failure. The state machine
	// does not attempt to guess whether retry is safe. Because the
	// failure lands before the point of no return (the server never
	// co-signed), the transition releases the reserved inputs so they
	// return to the spendable set instead of waiting for a restart
	// sweep.
	transition, err := handleOutboxError(nil, current, &OutboxErrorEvent{
		OutboxType:  "x",
		Retryable:   false,
		ErrorReason: "boom",
	})
	require.NoError(t, err)
	require.IsType(t, &Failed{}, transition.NextState)

	require.True(t, transition.NewEvents.IsSome())
	failEmitted := transition.NewEvents.UnwrapOr(EmittedEvent{})
	require.Len(t, failEmitted.Outbox, 1)

	release, ok := failEmitted.Outbox[0].(*ReleaseInputsRequest)
	require.True(t, ok)
	require.Equal(
		t, []wire.OutPoint{inputs[0].VTXO.Outpoint}, release.Outpoints,
	)

	// Retryable error keeps the FSM in the current state and emits retry
	// scheduling. The actor persists retry metadata alongside the real
	// protocol state.
	transition, err = handleOutboxError(nil, current, &OutboxErrorEvent{
		OutboxType:  "x",
		Retryable:   true,
		RetryAfter:  0,
		ErrorReason: "boom",
	})
	require.NoError(t, err)
	require.Same(t, current, transition.NextState)

	require.True(t, transition.NewEvents.IsSome())
	emitted := transition.NewEvents.UnwrapOr(EmittedEvent{})
	require.Len(t, emitted.Outbox, 1)

	schedule, ok := emitted.Outbox[0].(*ScheduleRetryRequest)
	require.True(t, ok)
	require.Equal(t, 1*time.Second, schedule.After)
	require.Equal(t, "boom", schedule.Reason)
}

// TestReceiveNotifiedMetadataRetryBackoff asserts that retryable metadata
// failures in the notified state grow the backoff exponentially, cap it, and
// carry the attempt counter on the returned state so it persists.
func TestReceiveNotifiedMetadataRetryBackoff(t *testing.T) {
	t.Parallel()

	cases := []struct {
		startAttempts uint32
		wantAttempts  uint32
		wantAfter     time.Duration
	}{
		{
			startAttempts: 0,
			wantAttempts:  1,
			wantAfter:     1 * time.Second,
		},
		{
			startAttempts: 1,
			wantAttempts:  2,
			wantAfter:     2 * time.Second,
		},
		{
			startAttempts: 2,
			wantAttempts:  3,
			wantAfter:     4 * time.Second,
		},
		{
			startAttempts: 8,
			wantAttempts:  9,
			wantAfter:     256 * time.Second,
		},

		// 2^9 = 512s exceeds the 5m cap, so it clamps.
		{
			startAttempts: 9,
			wantAttempts:  10,
			wantAfter:     metadataRetryMaxDelay,
		},
	}

	for _, tc := range cases {
		state := &ReceiveNotified{
			SessionID: SessionID{
				0x01,
			},
			MetadataAttempts: tc.startAttempts,
		}

		transition, err := handleReceiveOutboxError(
			state, &OutboxErrorEvent{
				Retryable:   true,
				RetryAfter:  defaultRetryDelay,
				ErrorReason: "metadata missing",
			},
		)
		require.NoError(t, err)

		next, ok := transition.NextState.(*ReceiveNotified)
		require.True(t, ok)
		require.Equal(t, tc.wantAttempts, next.MetadataAttempts)

		// The original state must not be mutated in place.
		require.Equal(t, tc.startAttempts, state.MetadataAttempts)

		emitted := transition.NewEvents.UnwrapOr(EmittedEvent{})
		require.Len(t, emitted.Outbox, 1)
		schedule, ok := emitted.Outbox[0].(*ScheduleRetryRequest)
		require.True(t, ok)
		require.Equal(t, tc.wantAfter, schedule.After)
	}
}

// TestReceiveNotifiedMetadataRetryGivesUp asserts that once the retry bound is
// exceeded the notified state fails terminally instead of scheduling another
// metadata query.
func TestReceiveNotifiedMetadataRetryGivesUp(t *testing.T) {
	t.Parallel()

	state := &ReceiveNotified{
		SessionID: SessionID{
			0x02,
		},
		MetadataAttempts: maxMetadataRetries,
	}

	transition, err := handleReceiveOutboxError(state, &OutboxErrorEvent{
		Retryable:   true,
		RetryAfter:  defaultRetryDelay,
		ErrorReason: "metadata missing",
	})
	require.NoError(t, err)

	failed, ok := transition.NextState.(*Failed)
	require.True(t, ok)
	require.Contains(t, failed.Reason, "metadata missing")
	require.True(t, transition.NewEvents.IsNone())
}

// TestReceiveResolvingRetryReschedules asserts a resolving session below the
// give-up bound re-emits the phase-1 query and re-arms the give-up timer,
// advancing the persisted attempt counter.
func TestReceiveResolvingRetryReschedules(t *testing.T) {
	t.Parallel()

	state := &ReceiveResolving{
		SessionID: SessionID{
			0x04,
		},
		RecipientPkScript: []byte{
			0x51,
			0x20,
			0xaa,
		},
		RecipientEventID: 9,
		ResolveAttempts:  3,
	}

	transition := handleResolveRetry(state)

	next, ok := transition.NextState.(*ReceiveResolving)
	require.True(t, ok)
	require.Equal(t, uint32(4), next.ResolveAttempts)
	require.Equal(t, state.RecipientEventID, next.RecipientEventID)

	// The source state must not be mutated in place.
	require.Equal(t, uint32(3), state.ResolveAttempts)

	require.True(t, transition.NewEvents.IsSome())
	emitted := transition.NewEvents.UnwrapOr(EmittedEvent{})
	require.Len(t, emitted.Outbox, 2)
	require.IsType(t, &QueryIncomingTransferRequest{}, emitted.Outbox[0])
	require.IsType(t, &ScheduleRetryRequest{}, emitted.Outbox[1])
}

// TestReceiveResolvingRetryGivesUp asserts that once the resolve bound is
// reached the resolving state fails terminally so it becomes reap-eligible and
// frees its concurrency slot, instead of re-querying an unanswered resolve
// forever.
func TestReceiveResolvingRetryGivesUp(t *testing.T) {
	t.Parallel()

	state := &ReceiveResolving{
		SessionID: SessionID{
			0x05,
		},
		RecipientPkScript: []byte{
			0x51,
			0x20,
			0xbb,
		},
		ResolveAttempts: maxResolveRetries,
	}

	transition := handleResolveRetry(state)

	failed, ok := transition.NextState.(*Failed)
	require.True(t, ok)
	require.Contains(t, failed.Reason, "unresolved")
	require.True(t, transition.NewEvents.IsNone())
}

// TestIncomingSnapshotResolveAttemptsRoundTrip asserts the persisted resolve
// retry counter survives a snapshot encode/decode cycle so the give-up bound
// holds across restarts.
func TestIncomingSnapshotResolveAttemptsRoundTrip(t *testing.T) {
	t.Parallel()

	snap := &IncomingSnapshot{
		Version: 1,
		SessionID: SessionID{
			0x06,
		},
		Phase: IncomingPhaseResolvePending,
		RecipientPkScript: []byte{
			0x51,
			0x20,
			0xcc,
		},
		RecipientEventID: 11,
		ResolveAttempts:  5,
	}

	raw, err := encodeIncomingSnapshot(snap)
	require.NoError(t, err)

	decoded, err := decodeIncomingSnapshotWithLimits(
		raw, DefaultReceiveLimits(),
	)
	require.NoError(t, err)
	require.Equal(t, uint32(5), decoded.ResolveAttempts)
}

// TestMetadataRetryBackoff covers the backoff helper bounds directly.
func TestMetadataRetryBackoff(t *testing.T) {
	t.Parallel()

	require.Equal(t, metadataRetryBaseDelay, metadataRetryBackoff(0))
	require.Equal(t, metadataRetryBaseDelay, metadataRetryBackoff(1))
	require.Equal(t, 2*time.Second, metadataRetryBackoff(2))
	require.Equal(t, metadataRetryMaxDelay, metadataRetryBackoff(100))
}

// TestIncomingSnapshotMetadataAttemptsRoundTrip asserts the persisted retry
// counter survives a snapshot encode/decode cycle so the give-up bound holds
// across restarts.
func TestIncomingSnapshotMetadataAttemptsRoundTrip(t *testing.T) {
	t.Parallel()

	snap := &IncomingSnapshot{
		Version: 1,
		SessionID: SessionID{
			0x03,
		},
		Phase: IncomingPhaseMaterializePending,
		ArkPSBT: []byte{
			0xaa,
			0xbb,
		},
		MetadataAttempts: 7,
	}

	raw, err := encodeIncomingSnapshot(snap)
	require.NoError(t, err)

	decoded, err := decodeIncomingSnapshotWithLimits(
		raw, DefaultReceiveLimits(),
	)
	require.NoError(t, err)
	require.Equal(t, uint32(7), decoded.MetadataAttempts)
}

// TestIncomingSnapshotRecipientPolicyTemplateRoundTrip asserts that a
// recipient's VTXO policy template survives a snapshot encode/decode cycle, so
// a custom recipient policy is preserved across a restart between notify and
// materialization rather than being lost and silently downgraded to the
// standard template on resume.
func TestIncomingSnapshotRecipientPolicyTemplateRoundTrip(t *testing.T) {
	t.Parallel()

	template := []byte{0x01, 0x02, 0x03, 0x04}
	snap := &IncomingSnapshot{
		Version: 1,
		SessionID: SessionID{
			0x09,
		},
		Phase: IncomingPhaseMaterializePending,
		ArkPSBT: []byte{
			0xaa,
			0xbb,
		},
		Recipients: []ArkRecipientOutput{
			{
				OutputIndex: 2,
				Value:       12345,
				PkScript: []byte{
					0x51,
					0x20,
					0xcc,
				},
				VTXOPolicyTemplate: template,
			},
		},
	}

	raw, err := encodeIncomingSnapshot(snap)
	require.NoError(t, err)

	decoded, err := decodeIncomingSnapshotWithLimits(
		raw, DefaultReceiveLimits(),
	)
	require.NoError(t, err)
	require.Len(t, decoded.Recipients, 1)
	require.Equal(
		t, template, decoded.Recipients[0].VTXOPolicyTemplate,
	)
	require.Equal(t, uint32(2), decoded.Recipients[0].OutputIndex)
}

// TestAwaitingFinalizeAcceptedOutboxError verifies that retryable outbox
// failures in the finalize-ack wait state stay in the current state while
// scheduling retry.
func TestAwaitingFinalizeAcceptedOutboxError(t *testing.T) {
	t.Parallel()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	inputValue := btcutil.Amount(10_000)
	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	inputs := []TransferInput{
		newTestTransferInput(
			t,
			clientKey,
			policy.OperatorKey,
			wire.OutPoint{
				Hash:  [32]byte{0x11},
				Index: 1,
			},
			inputValue,
		),
	}

	recipients := []oortx.RecipientOutput{{
		PkScript: newTestTaprootPkScript(t, clientKey.PubKey()),
		Value:    inputValue,
	}}

	ark, checkpoints, err := buildSubmitPackage(policy, inputs, recipients)
	require.NoError(t, err)

	state := &AwaitingFinalizeAccepted{
		SessionID:            SessionID(ark.UnsignedTx.TxHash()),
		ArkPSBT:              ark,
		FinalCheckpointPSBTs: checkpoints,
		TransferInputs:       inputs,
	}

	transition, err := state.ProcessEvent(t.Context(), &OutboxErrorEvent{
		OutboxType:  (&SendFinalizePackageRequest{}).outboxType(),
		Retryable:   true,
		RetryAfter:  0,
		ErrorReason: "finalize transport unavailable",
	}, nil)
	require.NoError(t, err)
	require.Same(t, state, transition.NextState)

	emitted := transition.NewEvents.UnwrapOr(EmittedEvent{})
	require.Len(t, emitted.Outbox, 1)

	schedule, ok := emitted.Outbox[0].(*ScheduleRetryRequest)
	require.True(t, ok)
	require.Equal(t, defaultRetryDelay, schedule.After)
}

// TestReceiveNotifiedMetadataRetryOverflowGivesUp ensures a corrupted snapshot
// whose attempt counter sits at the uint32 maximum still terminates terminally
// rather than wrapping past the bound when incremented. Checking the persisted
// counter before the increment keeps the give-up reachable.
func TestReceiveNotifiedMetadataRetryOverflowGivesUp(t *testing.T) {
	t.Parallel()

	state := &ReceiveNotified{
		SessionID: SessionID{
			0x03,
		},
		MetadataAttempts: math.MaxUint32,
	}

	transition, err := handleReceiveOutboxError(state, &OutboxErrorEvent{
		Retryable:   true,
		RetryAfter:  defaultRetryDelay,
		ErrorReason: "metadata missing",
	})
	require.NoError(t, err)

	failed, ok := transition.NextState.(*Failed)
	require.True(t, ok)
	require.Contains(t, failed.Reason, "metadata missing")
	require.True(t, transition.NewEvents.IsNone())
}
