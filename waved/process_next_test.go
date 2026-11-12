package waved

import (
	"context"
	"errors"
	"testing"

	"github.com/lightninglabs/wavelength/serverconn"
	"github.com/stretchr/testify/require"
)

// stepTestIngress observes the exact pump contract rather than emulating
// downstream actor completion, which this API intentionally cannot prove.
type stepTestIngress struct {
	windowTestIngress
	limit  uint32
	policy serverconn.IngressPumpPolicy
}

// PumpIngressWithPolicy captures the single-batch limit and invocation policy.
func (f *stepTestIngress) PumpIngressWithPolicy(ctx context.Context,
	limit uint32, policy serverconn.IngressPumpPolicy) (
	serverconn.IngressPumpResult, error) {

	f.limit, f.policy = limit, policy

	return f.windowTestIngress.PumpIngressWithPolicy(ctx, limit, policy)
}

// TestProcessNextSingleBatch proves the step neither loops nor reads payment
// state and retains the audited route policy before any dispatch.
func TestProcessNextSingleBatch(t *testing.T) {
	failure := errors.New("storage unavailable")
	for _, test := range []struct {
		name   string
		pumped serverconn.IngressPumpResult
		err    error
	}{
		{
			name: "processed",
			pumped: serverconn.IngressPumpResult{
				Batches: 1,
			},
		},
		{
			name: "empty",
			pumped: serverconn.IngressPumpResult{
				MailboxEmpty: true,
			},
		},
		{
			name: "foreground",
			err:  serverconn.ErrIngressNeedsForeground,
		},
		{
			name: "error",
			err:  failure,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ingress := &stepTestIngress{}
			ingress.pump = func(ctx context.Context) (
				serverconn.IngressPumpResult, error) {

				_, bounded := ctx.Deadline()
				require.True(t, bounded)

				return test.pumped, test.err
			}
			controller := &receiptWindowController{
				root: t.Context(), ingress: ingress,
			}
			result, err := controller.processNext(
				t.Context(),
				"one",
			)
			if errors.Is(
				test.err, serverconn.ErrIngressNeedsForeground,
			) {

				require.NoError(t, err)
				require.True(t, result.NeedsForeground)
			} else {
				require.ErrorIs(t, err, test.err)
			}
			require.Equal(t, test.pumped.Batches, result.Batches)
			require.Equal(
				t, test.pumped.MailboxEmpty,
				result.MailboxEmpty,
			)
			require.EqualValues(t, 1, ingress.limit)
			require.Equal(t, 1, ingress.pumps)
			require.Equal(t, receiptIngressPolicy(), ingress.policy)
			require.Empty(t, controller.activeID)
		})
	}
}

// TestProcessNextAdmission preserves foreground ownership and an OS expiry
// that arrives before the matching invocation enters the controller.
func TestProcessNextAdmission(t *testing.T) {
	ingress := &windowTestIngress{
		pump: func(ctx context.Context) (serverconn.IngressPumpResult,
			error) {

			return serverconn.IngressPumpResult{}, ctx.Err()
		},
	}
	controller := newWindowTestController(t, ingress)
	controller.foreground = true
	result, err := controller.processNext(t.Context(), "foreground")
	require.NoError(t, err)
	require.True(t, result.NeedsForeground)
	require.Zero(t, ingress.pumps)
	controller.foreground = false
	controller.cancelWindow("expired")
	_, err = controller.processNext(t.Context(), "expired")
	require.ErrorIs(t, err, context.Canceled)
	_, err = controller.processNext(t.Context(), "fresh")
	require.NoError(t, err)
}

// TestProcessNextForegroundJoinsFinalAccess proves a canceled step still owns
// its mode lease until the pump's final resource access actually returns.
func TestProcessNextForegroundJoinsFinalAccess(t *testing.T) {
	ingress := &windowTestIngress{
		entered: make(chan struct{}), canceled: make(chan struct{}),
		release: make(chan struct{}),
	}
	controller := newWindowTestController(t, ingress)
	returned := make(chan error, 1)
	go func() {
		_, err := controller.processNext(t.Context(), "one")
		returned <- err
	}()
	<-ingress.entered
	foreground := make(chan error, 1)
	go func() {
		foreground <- controller.setForeground(t.Context(), true)
	}()
	<-ingress.canceled
	duplicate, err := controller.processNext(t.Context(), "two")
	require.NoError(t, err)
	require.True(t, duplicate.NeedsForeground)
	select {
	case err := <-foreground:
		t.Fatalf("foreground returned before final pump access: %v",
			err)

	default:
	}
	close(ingress.release)
	require.ErrorIs(t, <-returned, context.Canceled)
	require.NoError(t, <-foreground)
}
