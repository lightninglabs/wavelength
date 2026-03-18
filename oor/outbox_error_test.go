package oor

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/scripts"
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
	policy := scripts.CheckpointPolicy{
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
	_, err = handleOutboxError(current, nil)
	require.Error(t, err)

	// Non-retryable error causes terminal failure. The state machine does
	// not attempt to guess whether retry is safe.
	transition, err := handleOutboxError(current, &OutboxErrorEvent{
		OutboxType:  "x",
		Retryable:   false,
		ErrorReason: "boom",
	})
	require.NoError(t, err)
	require.IsType(t, &Failed{}, transition.NextState)
	require.True(t, transition.NewEvents.IsNone())

	// Retryable error keeps the FSM in the current state and emits retry
	// scheduling. The actor persists retry metadata alongside the real
	// protocol state.
	transition, err = handleOutboxError(current, &OutboxErrorEvent{
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

// TestAwaitingFinalizeAcceptedOutboxError verifies that retryable outbox
// failures in the finalize-ack wait state stay in the current state while
// scheduling retry.
func TestAwaitingFinalizeAcceptedOutboxError(t *testing.T) {
	t.Parallel()

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := scripts.CheckpointPolicy{
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
