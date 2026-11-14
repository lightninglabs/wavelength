package waved

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/lightninglabs/wavelength/serverconn"
	"github.com/stretchr/testify/require"
)

// TestWorkWindowRearmsAfterFirstPump keeps receive recovery independent of
// an unsupported mailbox head. It never reads receipts or exposes amounts.
func TestWorkWindowRearmsAfterFirstPump(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{
			"drained",
			nil,
		},
		{
			"foreground",
			serverconn.ErrIngressNeedsForeground,
		},
		{
			"offline",
			errors.New("edge offline"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				verifyWorkWindowRearm(t, tc.err)
			})
		})
	}
}

// verifyWorkWindowRearm checks ordering and source isolation at each first-pump
// outcome, using fake time so the ordinary drain loop needs no wall-clock wait.
func verifyWorkWindowRearm(t *testing.T, pumpErr error) {
	t.Helper()
	var order []string
	ingress := &windowTestIngress{
		pump: func(context.Context) (serverconn.IngressPumpResult,
			error) {

			order = append(order, "pump")

			return serverconn.IngressPumpResult{}, pumpErr
		},
	}
	c := newWindowTestController(t, ingress)
	c.read = func(context.Context, LightningReceiptRequest) (
		[]LightningReceipt, error) {

		t.Fatal("work window read payment proof")

		return nil, nil
	}
	c.maintain = func(context.Context) (uint32, bool, bool) {
		order = append(order, "scan")

		return 0, false, false
	}
	var refs []LightningReceiptReference
	c.resume = func(_ context.Context,
		ref LightningReceiptReference) error {

		refs = append(refs, ref)
		order = append(order, "resume")

		return errors.New("source remains pending")
	}
	first := LightningReceiptReference{
		Source: LightningReceiptSwap,
		ID:     "receive",
	}
	second := LightningReceiptReference{
		Source: LightningReceiptCredit,
		ID:     "credit",
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	result, err := c.runWindowWithWork(
		ctx, ReceiptWindowRequest{
			WindowID: "work",
		},
		false,
		[]LightningReceiptReference{first, first, second},
	)
	foreground := errors.Is(pumpErr, serverconn.ErrIngressNeedsForeground)
	if pumpErr != nil && !foreground {
		require.ErrorIs(t, err, pumpErr)
	} else {
		require.NoError(t, err)
	}
	require.Equal(t, []LightningReceiptReference{first, second}, refs)
	require.GreaterOrEqual(t, len(order), 3)
	require.Equal(t, []string{"pump", "resume", "resume"}, order[:3])
	require.Empty(t, result.Receipts)
	require.Equal(t, foreground, result.NeedsForeground)
}

// TestWorkWindowValidationPrecedesNativeAccess rejects an entire invalid
// source set before even asking a server without wallet services for a lease.
func TestWorkWindowValidationPrecedesNativeAccess(t *testing.T) {
	valid := LightningReceiptReference{
		Source: LightningReceiptSwap,
		ID:     "receive",
	}
	for _, request := range []WorkWindowRequest{
		{},
		{
			WindowID: strings.Repeat("x", 129),
		},
		{
			WindowID:   "work",
			References: make([]LightningReceiptReference, 65),
		},
		{WindowID: "work", References: []LightningReceiptReference{
			valid, {
				Source: "pay",
				ID:     "existing",
			},
		}},
		{WindowID: "work", References: []LightningReceiptReference{
			{
				Source: LightningReceiptCredit,
			},
		}},
		{WindowID: "work", References: []LightningReceiptReference{
			{
				Source: LightningReceiptSwap,
				ID:     strings.Repeat("x", 129),
			},
		}},
	} {
		_, err := new(Server).RunWorkWindow(t.Context(), request)
		require.ErrorContains(t, err, "invalid")
	}
	refs := make([]LightningReceiptReference, 64)
	for i := range refs {
		refs[i] = valid
	}
	require.NoError(
		t,
		validateWorkWindow(
			WorkWindowRequest{
				WindowID:   "work",
				References: refs,
			},
		),
	)
	require.NoError(
		t,
		validateWorkWindow(
			WorkWindowRequest{
				WindowID: "maintenance",
			},
		),
	)
}

// TestWorkWindowJoinsTargetedResume keeps foreground intake excluded after OS
// cancellation until the targeted source callback has finished its last access.
func TestWorkWindowJoinsTargetedResume(t *testing.T) {
	c := newWindowTestController(
		t, &windowTestIngress{
			pump: func(context.Context) (
				serverconn.IngressPumpResult, error) {

				return serverconn.IngressPumpResult{}, nil
			},
		},
	)
	entered := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	unblock := func() {
		once.Do(func() { close(release) })
	}
	t.Cleanup(unblock)
	c.resume = func(ctx context.Context,
		_ LightningReceiptReference) error {

		close(entered)
		<-ctx.Done()
		close(canceled)
		<-release

		return ctx.Err()
	}
	windowDone := make(chan error, 1)
	go func() {
		_, err := c.runWindowWithWork(
			t.Context(), ReceiptWindowRequest{
				WindowID: "work",
			},
			false, []LightningReceiptReference{
				{Source: LightningReceiptSwap, ID: "receive"},
			},
		)
		windowDone <- err
	}()
	<-entered
	foregroundDone := make(chan error, 1)
	go func() {
		foregroundDone <- c.setForeground(t.Context(), true)
	}()
	<-canceled
	select {
	case err := <-foregroundDone:
		t.Fatalf("foreground escaped final access: %v", err)

	default:
	}
	select {
	case err := <-windowDone:
		t.Fatalf("window escaped final access: %v", err)

	default:
	}
	unblock()
	require.NoError(t, <-windowDone)
	require.NoError(t, <-foregroundDone)
}
