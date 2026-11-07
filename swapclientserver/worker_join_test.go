//go:build swapruntime

package swapclientserver

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/sdk/swaps"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// joinedSummaryRuntime holds the deferred database read after session Wait
// returns. Cancellation alone cannot prove the worker has stopped using
// storage.
type joinedSummaryRuntime struct {
	*fakeSwapRuntime
	entered chan lntypes.Hash
	release chan struct{}
}

// GetSwapSummary emulates a driver already inside a final store read when the
// daemon cancels its worker, and exits only when the test releases that read.
func (r *joinedSummaryRuntime) GetSwapSummary(ctx context.Context,
	hash lntypes.Hash) (swaps.SwapSummary, error) {

	r.entered <- hash
	<-r.release

	return r.fakeSwapRuntime.GetSwapSummary(ctx, hash)
}

// TestWorkerShutdownJoinsFinalReads proves pay and receive workers finish
// deferred summary reads before cleanup can close their store or transports.
func TestWorkerShutdownJoinsFinalReads(t *testing.T) {
	t.Parallel()
	pay, receive := testHash(91), testHash(92)
	runtime := &joinedSummaryRuntime{
		fakeSwapRuntime: newFakeSwapRuntime(
			swaps.SwapSummary{
				PaymentHash: pay,
				Direction:   swaps.SwapDirectionPay,
				Pending:     true,
			}, swaps.SwapSummary{
				PaymentHash: receive,
				Direction:   swaps.SwapDirectionReceive,
				Pending:     true,
			},
		),
		entered: make(chan lntypes.Hash, 2), release: make(
			chan struct{},
		),
	}
	service := newTestSwapClientService(runtime.fakeSwapRuntime)
	service.client = runtime
	var release sync.Once
	defer func() {
		release.Do(func() { close(runtime.release) })
		service.stopWorkers()
	}()
	service.startPayWorker(pay)
	require.Equal(t, workerStarted, service.startReceiveWorker(receive))
	runtime.awaitPayResume(t, pay)
	runtime.awaitReceiveResume(t, receive)
	closed := make(chan struct{})
	go func() {
		service.stopWorkers()
		close(closed)
	}()
	seen := make(map[lntypes.Hash]bool)
	for range 2 {
		select {
		case hash := <-runtime.entered:
			seen[hash] = true

		case <-time.After(5 * time.Second):
			t.Fatal("final read not reached")
		}
	}
	require.True(t, seen[pay])
	require.True(t, seen[receive])
	select {
	case <-closed:
		t.Fatal("cleanup passed an in-flight final read")

	case <-time.After(20 * time.Millisecond):
	}
	// Admission is closed while old owners are still draining.
	var admitted atomic.Bool
	service.launchWorker(func() { admitted.Store(true) })
	release.Do(func() { close(runtime.release) })
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("workers not joined")
	}
	require.False(t, admitted.Load())
}

// joinedRefillRuntime fails startup's first scan then holds the asynchronous
// refill read, which is another store owner that teardown must join.
type joinedRefillRuntime struct {
	*fakeSwapRuntime
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
}

// ListSwapSummaries models the startup-to-refill handoff and a blocked driver.
func (r *joinedRefillRuntime) ListSwapSummaries(ctx context.Context, _ bool) (
	[]swaps.SwapSummary, error) {

	if r.calls.Add(1) == 1 {
		return nil, errors.New("temporary startup scan failure")
	}
	close(r.entered)
	<-r.release

	return nil, ctx.Err()
}

// TestWorkerShutdownJoinsRefill proves cleanup waits for a retry scan as well
// as payment workers. Failed scans must not spawn a new owner after shutdown.
func TestWorkerShutdownJoinsRefill(t *testing.T) {
	t.Parallel()
	runtime := &joinedRefillRuntime{
		fakeSwapRuntime: newFakeSwapRuntime(),
		entered:         make(chan struct{}),
		release:         make(chan struct{}),
	}
	service := newTestSwapClientService(runtime.fakeSwapRuntime)
	service.client = runtime
	var release sync.Once
	defer func() {
		release.Do(func() { close(runtime.release) })
		service.stopWorkers()
	}()
	service.resumePending(t.Context())
	select {
	case <-runtime.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("refill never started")
	}
	stopped := make(chan struct{})
	go func() {
		service.stopWorkers()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("cleanup passed an in-flight refill")

	case <-time.After(20 * time.Millisecond):
	}
	release.Do(func() { close(runtime.release) })
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("refill not joined")
	}
	require.EqualValues(t, 2, runtime.calls.Load())
}
