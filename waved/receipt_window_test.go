package waved

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/lightninglabs/wavelength/serverconn"
	"github.com/stretchr/testify/require"
)

// windowTestIngress captures lifetime ownership and a blocked final pump
// access.
type windowTestIngress struct {
	mu sync.Mutex
	//nolint:containedctx // Observe the retained foreground lifetime.
	foreground context.Context
	entered    chan struct{}
	canceled   chan struct{}
	release    chan struct{}
	pumps      int
	pump       func(context.Context) (serverconn.IngressPumpResult, error)
}

// StartIngress records the actual context retained after startup returns.
func (f *windowTestIngress) StartIngress(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.foreground = ctx

	return nil
}

// PauseIngress joins are immediate because this fake starts no background work.
func (f *windowTestIngress) PauseIngress() {}

// PumpIngressWithPolicy waits beyond cancellation until its last access ends.
func (f *windowTestIngress) PumpIngressWithPolicy(ctx context.Context, _ uint32,
	_ serverconn.IngressPumpPolicy) (serverconn.IngressPumpResult, error) {

	f.pumps++
	if f.pump != nil {
		return f.pump(ctx)
	}
	close(f.entered)
	<-ctx.Done()
	close(f.canceled)
	<-f.release

	return serverconn.IngressPumpResult{}, ctx.Err()
}

// newWindowTestController provides a pending enrolled invoice without a DB.
func newWindowTestController(t *testing.T,
	ingress *windowTestIngress) *receiptWindowController {

	t.Helper()

	return &receiptWindowController{
		root: t.Context(), ingress: ingress,
		connected: func(bool) {},
		read: func(context.Context, LightningReceiptRequest) (
			[]LightningReceipt, error) {

			return []LightningReceipt{
				{
					State: "pending",
				},
			}, nil
		},
		resume: func(context.Context, LightningReceiptReference) error {
			return nil
		},
	}
}

// TestReceiptWindowForegroundJoin proves cancellation is not a lease release.
func TestReceiptWindowForegroundJoin(t *testing.T) {
	verifyReceiptWindowJoin(t, true)
}

// TestReceiptWindowPauseJoin exercises pause while a window owns final access.
func TestReceiptWindowPauseJoin(t *testing.T) {
	verifyReceiptWindowJoin(t, false)
}

// verifyReceiptWindowJoin shares the real admission/cancellation handshake.
func verifyReceiptWindowJoin(t *testing.T, foreground bool) {
	t.Helper()
	ingress := &windowTestIngress{
		entered: make(chan struct{}), canceled: make(chan struct{}),
		release: make(chan struct{}),
	}
	controller := newWindowTestController(t, ingress)
	windowDone := make(chan ReceiptWindowResult, 1)
	go func() {
		result, err := controller.run(
			t.Context(), ReceiptWindowRequest{
				WindowID: "one",
			},
		)
		require.NoError(t, err)
		windowDone <- result
	}()
	<-ingress.entered
	duplicate, err := controller.run(
		t.Context(), ReceiptWindowRequest{
			WindowID: "duplicate",
		},
	)
	require.NoError(t, err)
	require.True(t, duplicate.NeedsForeground)
	require.Equal(t, 1, ingress.pumps)
	foregroundDone := make(chan error, 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		foregroundDone <- controller.setForeground(ctx, foreground)
	}()
	<-ingress.canceled
	// Mutex blocking is not synctest-durable. Use the explicit pump
	// cancellation handshake and retain its final-access barrier.
	select {
	case err := <-foregroundDone:
		require.NoError(t, err)
		t.Fatal("foreground escaped before the pump's final access")

	case <-time.After(100 * time.Millisecond):
	}
	close(ingress.release)
	require.True(t, (<-windowDone).Interrupted)
	require.NoError(t, <-foregroundDone)
	if !foreground {
		require.Nil(t, ingress.foreground)

		return
	}
	cancel()
	require.NoError(
		t, ingress.foreground.Err(),
		"returning caller cannot cancel foreground",
	)
	require.NoError(t, controller.setForeground(t.Context(), false))
	require.ErrorIs(t, ingress.foreground.Err(), context.Canceled)
}

// TestReceiptWindowEarlyExpiry remembers cancellation before admission.
func TestReceiptWindowEarlyExpiry(t *testing.T) {
	ingress := &windowTestIngress{}
	controller := newWindowTestController(t, ingress)
	controller.cancelWindow("expired")
	result, err := controller.run(
		t.Context(), ReceiptWindowRequest{
			WindowID: "expired",
		},
	)
	require.NoError(t, err)
	require.True(t, result.Interrupted)
	require.Zero(t, ingress.pumps)
}

// TestReceiptWindowWaitsForSource distinguishes empty ingress from settlement.
func TestReceiptWindowWaitsForSource(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ingress := &windowTestIngress{}
		controller := newWindowTestController(t, ingress)
		complete := false
		ingress.pump = func(context.Context) (
			serverconn.IngressPumpResult, error) {

			complete = true

			return serverconn.IngressPumpResult{
				MailboxEmpty: true,
			}, nil
		}
		reads := 0
		controller.read = func(context.Context,
			LightningReceiptRequest) ([]LightningReceipt, error) {

			reads++
			state := "pending"
			if complete {
				state = "complete"
			}

			return []LightningReceipt{
				{
					State:     state,
					AmountSat: 100000,
				},
			}, nil
		}
		result, err := controller.run(
			t.Context(), ReceiptWindowRequest{
				WindowID: "complete",
			},
		)
		require.NoError(t, err)
		require.EqualValues(t, 100000, result.Receipts[0].AmountSat)
		require.Equal(t, "complete", result.Receipts[0].State)
		require.Equal(t, 2, reads)
		require.Equal(t, 1, ingress.pumps)
		// A quiet mailbox with a source that stays pending must yield
		// at the deadline. It cannot be interpreted as a completed
		// receipt.
		controller.read = func(context.Context,
			LightningReceiptRequest) ([]LightningReceipt, error) {

			return []LightningReceipt{{State: "pending"}}, nil
		}
		result, err = controller.run(
			t.Context(), ReceiptWindowRequest{
				WindowID: "timeout",
			},
		)
		require.NoError(t, err)
		require.True(t, result.Interrupted)
		require.Equal(t, "pending", result.Receipts[0].State)
		controller.foreground = true
		result, err = controller.run(
			t.Context(), ReceiptWindowRequest{
				WindowID: "foreground",
			},
		)
		require.NoError(t, err)
		require.True(t, result.NeedsForeground)
	})
}

// TestReceiptWindowSourceCancellation reports a normal interruption at either
// source await, rather than returning an indistinguishable source failure.
func TestReceiptWindowSourceCancellation(t *testing.T) {
	for _, site := range []string{"read", "resume"} {
		t.Run(site, func(t *testing.T) {
			controller := newWindowTestController(
				t, &windowTestIngress{},
			)
			cancel := func(ctx context.Context) error {
				controller.cancelWindow("window")
				<-ctx.Done()

				return ctx.Err()
			}
			if site == "read" {
				controller.read = func(ctx context.Context,
					_ LightningReceiptRequest) (
					[]LightningReceipt, error) {

					return nil, cancel(ctx)
				}
			} else {
				controller.resume = func(ctx context.Context,
					_ LightningReceiptReference) error {

					return cancel(ctx)
				}
			}
			result, err := controller.run(
				t.Context(), ReceiptWindowRequest{
					WindowID: "window",
				},
			)
			require.NoError(t, err)
			require.True(t, result.Interrupted)
		})
	}
}

// TestReceiptWindowCanceledTransitionKeepsForeground proves an already-expired
// caller cannot pause an established healthy mode or misreport its status.
func TestReceiptWindowCanceledTransitionKeepsForeground(t *testing.T) {
	ingress := &windowTestIngress{}
	controller := newWindowTestController(t, ingress)
	connected := false
	controller.connected = func(value bool) {
		connected = value
	}
	require.NoError(t, controller.setForeground(t.Context(), true))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(
		t, controller.setForeground(ctx, false), context.Canceled,
	)
	require.True(t, connected)
	require.True(t, controller.foreground)
	require.NoError(t, ingress.foreground.Err())
	require.NoError(t, controller.setForeground(t.Context(), false))
	require.False(t, connected)
}

// TestReceiptWindowUsesHostDeadline proves both window kinds can dispatch after
// the old twenty-second cutoff and stop at the host deadline or safety ceiling.
func TestReceiptWindowUsesHostDeadline(t *testing.T) {
	for _, payment := range []bool{true, false} {
		for _, hostLimit := range []time.Duration{28 * time.Second, 0} {
			name := fmt.Sprintf("payment=%t/host=%s", payment,
				hostLimit)
			t.Run(name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					verifyHostWindowDeadline(
						t, payment, hostLimit,
					)
				})
			})
		}
	}
}

// verifyHostWindowDeadline keeps delayed round work alive until the owning host
// cancels, while retaining a finite ceiling when that host supplies no
// deadline.
func verifyHostWindowDeadline(t *testing.T, payment bool,
	hostLimit time.Duration) {

	t.Helper()
	ingress := &windowTestIngress{}
	controller := newWindowTestController(t, ingress)
	controller.maintain = func(context.Context) (uint32, bool, bool) {
		return 0, false, false
	}
	start := time.Now()
	ingress.pump = func(ctx context.Context) (serverconn.IngressPumpResult,
		error) {

		select {
		case <-time.After(25 * time.Second):
			require.NoError(
				t, ctx.Err(),
				"host still owns the window",
			)

		case <-ctx.Done():
			t.Error("native stopped before delayed round work")
		}
		<-ctx.Done()

		return serverconn.IngressPumpResult{}, ctx.Err()
	}
	ctx := t.Context()
	if hostLimit > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, hostLimit)
		defer cancel()
	}
	result, err := controller.runWindow(
		ctx, ReceiptWindowRequest{
			WindowID: "host-window",
		},
		payment,
	)
	require.NoError(t, err)
	require.True(t, result.Interrupted)
	expected := hostLimit
	if expected == 0 {
		expected = 30 * time.Second
	}
	require.Equal(t, expected, time.Since(start))
	require.Empty(t, controller.activeID)
}
