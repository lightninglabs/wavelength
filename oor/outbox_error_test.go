package oor

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
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
		InputOutpoints:  []wire.OutPoint{inputs[0].VTXO.Outpoint},
		ArkPSBT:         ark,
		CheckpointPSBTs: checkpoints,
		TransferInputs:  inputs,
	}

	sessionID, err := sessionIDFromArk(ark)
	require.NoError(t, err)

	env := &Environment{SessionID: sessionID}

	// Nil event rejected: this is a programmer error and should not be
	// treated as retryable.
	_, err = handleOutboxError(env, current, nil)
	require.Error(t, err)

	// Retryable error requires a valid environment session id. The session
	// id is used to reconstruct a safe resume snapshot.
	_, err = handleOutboxError(nil, current, &OutboxErrorEvent{
		OutboxType:  "x",
		Retryable:   true,
		RetryAfter:  0,
		ErrorReason: "boom",
	})
	require.Error(t, err)

	// Non-retryable error causes terminal failure. The state machine does
	// not attempt to guess whether retry is safe.
	transition, err := handleOutboxError(env, current, &OutboxErrorEvent{
		OutboxType:  "x",
		Retryable:   false,
		ErrorReason: "boom",
	})
	require.NoError(t, err)
	require.IsType(t, &Failed{}, transition.NextState)
	require.True(t, transition.NewEvents.IsNone())

	// Retryable error schedules a retry and snapshots the resume state so
	// the caller can survive a crash while waiting for the backoff timer.
	transition, err = handleOutboxError(env, current, &OutboxErrorEvent{
		OutboxType:  "x",
		Retryable:   true,
		RetryAfter:  0,
		ErrorReason: "boom",
	})
	require.NoError(t, err)

	backoff, ok := transition.NextState.(*RetryBackoff)
	require.True(t, ok)
	require.NotNil(t, backoff.ResumeSnapshot)
	require.Equal(t, 1*time.Second, backoff.RetryAfter)

	require.True(t, transition.NewEvents.IsSome())
	emitted := transition.NewEvents.UnwrapOr(EmittedEvent{})
	require.Len(t, emitted.Outbox, 1)

	schedule, ok := emitted.Outbox[0].(*ScheduleRetryRequest)
	require.True(t, ok)
	require.Equal(t, 1*time.Second, schedule.After)
	require.Equal(t, "boom", schedule.Reason)
}

// TestAwaitingFinalizeAcceptedOutboxError verifies that retryable outbox
// failures in the finalize-ack wait state transition into RetryBackoff.
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

	sessionID, err := sessionIDFromArk(ark)
	require.NoError(t, err)

	state := &AwaitingFinalizeAccepted{
		SessionID:            sessionID,
		ArkPSBT:              ark,
		InputOutpoints:       []wire.OutPoint{inputs[0].VTXO.Outpoint},
		FinalCheckpointPSBTs: checkpoints,
	}

	env := &Environment{SessionID: sessionID}
	transition, err := state.ProcessEvent(t.Context(), &OutboxErrorEvent{
		OutboxType:  (&SendFinalizePackageRequest{}).outboxType(),
		Retryable:   true,
		RetryAfter:  0,
		ErrorReason: "finalize transport unavailable",
	}, env)
	require.NoError(t, err)
	require.IsType(t, &RetryBackoff{}, transition.NextState)

	emitted := transition.NewEvents.UnwrapOr(EmittedEvent{})
	require.Len(t, emitted.Outbox, 1)

	schedule, ok := emitted.Outbox[0].(*ScheduleRetryRequest)
	require.True(t, ok)
	require.Equal(t, defaultRetryDelay, schedule.After)
}
