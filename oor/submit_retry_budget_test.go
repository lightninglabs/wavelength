package oor

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// newTestSubmitState builds a deterministic AwaitingSubmitAccepted state with a
// single reserved input, so the transient submit-reject retry-budget tests can
// assert both the reschedule-within-budget and the terminal-past-budget paths
// (the latter must release the reserved pre-point-of-no-return input).
func newTestSubmitState(t *testing.T) (*AwaitingSubmitAccepted, wire.OutPoint) {
	t.Helper()

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
			t, clientKey, policy.OperatorKey, wire.OutPoint{
				Hash:  [32]byte{0x01},
				Index: 0,
			}, inputValue,
		),
	}

	recipients := []oortx.RecipientOutput{{
		PkScript: newTestTaprootPkScript(t, clientKey.PubKey()),
		Value:    inputValue,
	}}

	ark, checkpoints, err := buildSubmitPackage(policy, inputs, recipients)
	require.NoError(t, err)

	return &AwaitingSubmitAccepted{
		ArkPSBT:         ark,
		CheckpointPSBTs: checkpoints,
		TransferInputs:  inputs,
	}, inputs[0].VTXO.Outpoint
}

// TestHandleSubmitOutboxErrorReschedulesWithinBudget asserts that a retryable
// submit-reject error inside the retry budget keeps the FSM in
// AwaitingSubmitAccepted, emits a ScheduleRetryRequest, and records/advances
// the retry-window start using the injected clock rather than time.Now().
func TestHandleSubmitOutboxErrorReschedulesWithinBudget(t *testing.T) {
	t.Parallel()

	current, _ := newTestSubmitState(t)

	start := time.Unix(1_700_000_000, 0)
	clk := clock.NewTestClock(start)
	env := &Environment{
		Clock:                   clk,
		MaxTransientSubmitRetry: time.Hour,
	}

	// First retryable reject: opens the window at the clock's current time
	// and reschedules with the requested delay.
	transition, err := handleSubmitOutboxError(
		env, current, &OutboxErrorEvent{
			OutboxType:  "submit",
			Retryable:   true,
			RetryAfter:  15 * time.Second,
			ErrorReason: "input not spendable",
		},
	)
	require.NoError(t, err)

	next, ok := transition.NextState.(*AwaitingSubmitAccepted)
	require.True(t, ok)
	require.Equal(t, start.UnixNano(), next.FirstRejectUnixNanos)

	// The source state must not be mutated in place.
	require.Zero(t, current.FirstRejectUnixNanos)

	emitted := transition.NewEvents.UnwrapOr(EmittedEvent{})
	require.Len(t, emitted.Outbox, 1)
	schedule, ok := emitted.Outbox[0].(*ScheduleRetryRequest)
	require.True(t, ok)
	require.Equal(t, 15*time.Second, schedule.After)

	// Advance the clock but stay within the budget: a subsequent reject
	// carries the ORIGINAL window start forward (it does not reset it) and
	// keeps rescheduling.
	clk.SetTime(start.Add(30 * time.Minute))
	transition, err = handleSubmitOutboxError(
		env, next, &OutboxErrorEvent{
			OutboxType:  "submit",
			Retryable:   true,
			RetryAfter:  15 * time.Second,
			ErrorReason: "input not spendable",
		},
	)
	require.NoError(t, err)

	next2, ok := transition.NextState.(*AwaitingSubmitAccepted)
	require.True(t, ok)
	require.Equal(t, start.UnixNano(), next2.FirstRejectUnixNanos)
	require.Len(t, transition.NewEvents.UnwrapOr(EmittedEvent{}).Outbox, 1)
}

// TestHandleSubmitOutboxErrorGivesUpPastBudget asserts that once the injected
// clock is past the retry budget, the next retryable submit-reject drives the
// session to terminal Failed and releases the reserved pre-point-of-no-return
// input, rather than scheduling another retry.
func TestHandleSubmitOutboxErrorGivesUpPastBudget(t *testing.T) {
	t.Parallel()

	current, outpoint := newTestSubmitState(t)

	start := time.Unix(1_700_000_000, 0)
	// The window opened at start; the clock is now well past the budget.
	current.FirstRejectUnixNanos = start.UnixNano()

	clk := clock.NewTestClock(start.Add(2 * time.Hour))
	env := &Environment{
		Clock:                   clk,
		MaxTransientSubmitRetry: time.Hour,
	}

	transition, err := handleSubmitOutboxError(
		env, current, &OutboxErrorEvent{
			OutboxType:  "submit",
			Retryable:   true,
			RetryAfter:  15 * time.Second,
			ErrorReason: "user balance exceeded",
		},
	)
	require.NoError(t, err)

	failed, ok := transition.NextState.(*Failed)
	require.True(t, ok)
	require.Contains(t, failed.Reason, "retry budget")
	require.Contains(t, failed.Reason, "user balance exceeded")

	// Pre-point-of-no-return: the reserved input is released back to the
	// spendable set instead of waiting for a restart sweep.
	emitted := transition.NewEvents.UnwrapOr(EmittedEvent{})
	require.Len(t, emitted.Outbox, 1)
	release, ok := emitted.Outbox[0].(*ReleaseInputsRequest)
	require.True(t, ok)
	require.Equal(t, []wire.OutPoint{outpoint}, release.Outpoints)
}

// TestHandleSubmitOutboxErrorUnboundedNeverGivesUp asserts a zero budget
// preserves the legacy unbounded behavior: a retryable reject always
// reschedules and never fails, no matter how far the clock has advanced.
func TestHandleSubmitOutboxErrorUnboundedNeverGivesUp(t *testing.T) {
	t.Parallel()

	current, _ := newTestSubmitState(t)

	start := time.Unix(1_700_000_000, 0)
	current.FirstRejectUnixNanos = start.UnixNano()

	clk := clock.NewTestClock(start.Add(1000 * time.Hour))
	env := &Environment{
		Clock: clk,
		// Zero budget: unbounded.
		MaxTransientSubmitRetry: 0,
	}

	transition, err := handleSubmitOutboxError(
		env, current, &OutboxErrorEvent{
			OutboxType:  "submit",
			Retryable:   true,
			RetryAfter:  15 * time.Second,
			ErrorReason: "input not spendable",
		},
	)
	require.NoError(t, err)

	_, ok := transition.NextState.(*AwaitingSubmitAccepted)
	require.True(t, ok)
	require.Len(t, transition.NewEvents.UnwrapOr(EmittedEvent{}).Outbox, 1)
}

// TestHandleSubmitOutboxErrorNonRetryableTerminal asserts a non-retryable
// submit error still fails terminally and releases pre-PONR inputs, unchanged
// from the shared handleOutboxError path.
func TestHandleSubmitOutboxErrorNonRetryableTerminal(t *testing.T) {
	t.Parallel()

	current, outpoint := newTestSubmitState(t)

	env := &Environment{
		Clock:                   clock.NewTestClock(time.Unix(1, 0)),
		MaxTransientSubmitRetry: time.Hour,
	}

	transition, err := handleSubmitOutboxError(
		env, current, &OutboxErrorEvent{
			OutboxType:  "submit",
			Retryable:   false,
			ErrorReason: "output policy violation",
		},
	)
	require.NoError(t, err)

	failed, ok := transition.NextState.(*Failed)
	require.True(t, ok)
	require.Equal(t, "output policy violation", failed.Reason)

	emitted := transition.NewEvents.UnwrapOr(EmittedEvent{})
	require.Len(t, emitted.Outbox, 1)
	release, ok := emitted.Outbox[0].(*ReleaseInputsRequest)
	require.True(t, ok)
	require.Equal(t, []wire.OutPoint{outpoint}, release.Outpoints)
}
