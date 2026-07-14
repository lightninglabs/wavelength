package swaps

import (
	"context"
	"fmt"
	"testing"

	loopfsm "github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestWalletNotReadyFailureIsNonTerminal verifies transient daemon wallet
// readiness errors do not durably fail persisted swaps.
func TestWalletNotReadyFailureIsNonTerminal(t *testing.T) {
	t.Parallel()

	tests := []string{
		"custom daemon readiness text",
		"renamed syncing readiness text",
	}

	for _, msg := range tests {
		t.Run(msg, func(t *testing.T) {
			t.Parallel()

			var runErr error
			markFailedCalls := 0
			event := handleFailure(
				t.Context(),
				waverpc.WalletNotReadyError(msg),
				&runErr,
				false,
				false,
				func(context.Context) error {
					return nil
				},
				loopfsm.EventType("expired"),
				func(context.Context, string) error {
					return nil
				},
				loopfsm.EventType("intervention"),
				func(context.Context, string) error {
					markFailedCalls++

					return nil
				},
				loopfsm.EventType("failed"),
			)

			require.Equal(t, loopfsm.NoOp, event)
			require.Equal(t, 0, markFailedCalls)
			require.Error(t, runErr)
		})
	}
}

// TestWrappedWalletNotReadyFailureIsNonTerminal verifies the structured
// readiness marker survives normal Go error wrapping.
func TestWrappedWalletNotReadyFailureIsNonTerminal(t *testing.T) {
	t.Parallel()

	var runErr error
	markFailedCalls := 0
	event := handleFailure(
		t.Context(),
		fmt.Errorf("resume receive swap: %w",
			waverpc.WalletNotReadyError("wallet still syncing")),
		&runErr,
		false,
		false,
		func(context.Context) error {
			return nil
		},
		loopfsm.EventType("expired"),
		func(context.Context, string) error {
			return nil
		},
		loopfsm.EventType("intervention"),
		func(context.Context, string) error {
			markFailedCalls++

			return nil
		},
		loopfsm.EventType("failed"),
	)

	require.Equal(t, loopfsm.NoOp, event)
	require.Equal(t, 0, markFailedCalls)
	require.Error(t, runErr)
}

// TestUnrelatedFailedPreconditionRemainsTerminal verifies the wallet-ready
// classifier is narrow and does not hide unrelated protocol failures.
func TestUnrelatedFailedPreconditionRemainsTerminal(t *testing.T) {
	t.Parallel()

	var runErr error
	var failedReason string
	err := status.Error(
		codes.FailedPrecondition, "operator rejected swap",
	)
	event := handleFailure(
		t.Context(),
		err,
		&runErr,
		false,
		false,
		func(context.Context) error {
			return nil
		},
		loopfsm.EventType("expired"),
		func(context.Context, string) error {
			return nil
		},
		loopfsm.EventType("intervention"),
		func(_ context.Context, reason string) error {
			failedReason = reason

			return nil
		},
		loopfsm.EventType("failed"),
	)

	require.Equal(t, loopfsm.EventType("failed"), event)
	require.Equal(
		t, "rpc error: code = FailedPrecondition desc = operator "+
			"rejected swap", failedReason,
	)
	require.Error(t, runErr)
}
