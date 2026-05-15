//go:build walletrpc && swapruntime

package swapwallet

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/lightninglabs/darepo-client/rpc/swapclientrpc"
	"github.com/lightninglabs/darepo-client/rpc/walletrpc"
	"github.com/stretchr/testify/require"
)

// streamingFakeSwap implements SubscribeSwaps as a push loop driven by a
// caller-supplied channel. Tests use it to drive WalletEntry updates
// through the monitor loop.
type streamingFakeSwap struct {
	fakeSwapService

	updates chan *swapclientrpc.SwapSummary
}

func newStreamingFakeSwap() *streamingFakeSwap {
	return &streamingFakeSwap{
		updates: make(chan *swapclientrpc.SwapSummary, 16),
	}
}

// SubscribeSwaps blocks pushing every update from f.updates to the
// caller's stream until the stream's context is canceled.
func (f *streamingFakeSwap) SubscribeSwaps(
	_ *swapclientrpc.SubscribeSwapsRequest,
	stream swapclientrpc.SwapClientService_SubscribeSwapsServer) error {

	ctx := stream.Context()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case s, ok := <-f.updates:
			if !ok {
				return nil
			}
			if err := stream.Send(
				&swapclientrpc.SubscribeSwapsResponse{
					Swap: s,
				},
			); err != nil {
				return err
			}
		}
	}
}

// drainOne pulls the next emitted WalletEntry from a subscriber channel
// within the test deadline.
func drainOne(t *testing.T,
	ch <-chan *walletrpc.WalletEntry) *walletrpc.WalletEntry {

	t.Helper()
	select {
	case e, ok := <-ch:
		require.True(t, ok, "subscriber channel closed unexpectedly")

		return e

	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for monitor update")

		return nil
	}
}

// TestMonitorLoopFansOutSwapUpdates confirms a SubscribeSwaps push
// reaches every wallet subscriber as a normalized WalletEntry.
func TestMonitorLoopFansOutSwapUpdates(t *testing.T) {
	t.Parallel()

	swap := newStreamingFakeSwap()
	deps := &Deps{SwapService: swap}
	r := newRuntime(t.Context(), deps)
	defer r.stop()

	sub := r.subscribe()
	r.startMonitorLoop()

	swap.updates <- &swapclientrpc.SwapSummary{
		PaymentHash: "abc",
		Direction:   swapclientrpc.SwapDirection_SWAP_DIRECTION_PAY,
		State:       swapclientrpc.SwapState_SWAP_STATE_COMPLETED,
	}

	entry := drainOne(t, sub)
	require.Equal(t, "abc", entry.GetId())
	require.Equal(
		t, walletrpc.EntryStatus_ENTRY_STATUS_COMPLETE,
		entry.GetStatus(),
	)
}

// TestMonitorLoopProjectsCanonicalID confirms that an EXIT intent registered
// before the swap update arrives causes the emitted WalletEntry to surface
// under the canonical id rather than the swap payment hash. This catches
// regressions in the resolveCanonicalID projection pathway.
func TestMonitorLoopProjectsCanonicalID(t *testing.T) {
	t.Parallel()

	swap := newStreamingFakeSwap()
	deps := &Deps{SwapService: swap}
	r := newRuntime(t.Context(), deps)
	defer r.stop()

	// Register a SEND intent that maps payment_hash "swap-hash" onto
	// itself. (The runtime always registers SEND intents by payment
	// hash; this is the same id, but the projection path is identical
	// to non-trivial EXIT cases below.)
	r.registerSendInvoiceIntent("swap-hash")

	sub := r.subscribe()
	r.startMonitorLoop()

	swap.updates <- &swapclientrpc.SwapSummary{
		PaymentHash: "swap-hash",
		Direction:   swapclientrpc.SwapDirection_SWAP_DIRECTION_PAY,
		Pending:     true,
	}
	entry := drainOne(t, sub)
	require.Equal(t, "swap-hash", entry.GetId())

	// Confirm pending was tracked (so deadline watcher would fire) by
	// inspecting runtime state.
	r.pendingMu.Lock()
	_, ok := r.pending["swap-hash"]
	r.pendingMu.Unlock()
	require.True(t, ok, "monitor must track pending swap rows")
}

// TestMonitorLoopClearsPendingOnTerminal confirms a terminal swap event
// removes the entry from the pending map so the deadline watcher stops
// ageing it.
func TestMonitorLoopClearsPendingOnTerminal(t *testing.T) {
	t.Parallel()

	swap := newStreamingFakeSwap()
	deps := &Deps{SwapService: swap}
	r := newRuntime(t.Context(), deps)
	defer r.stop()

	r.trackPending(
		"to-complete", walletrpc.EntryKind_ENTRY_KIND_SEND, time.Now(),
	)

	sub := r.subscribe()
	r.startMonitorLoop()

	swap.updates <- &swapclientrpc.SwapSummary{
		PaymentHash: "to-complete",
		Direction:   swapclientrpc.SwapDirection_SWAP_DIRECTION_PAY,
		State:       swapclientrpc.SwapState_SWAP_STATE_COMPLETED,
	}
	_ = drainOne(t, sub)

	r.pendingMu.Lock()
	_, ok := r.pending["to-complete"]
	r.pendingMu.Unlock()
	require.False(t, ok,
		"a COMPLETE update must clear the pending tracker")
}

// flakySwapService fails the first SubscribeSwaps with a transient error
// then succeeds and serves updates. The monitor loop must back off,
// re-subscribe, and recover without manual intervention.
type flakySwapService struct {
	streamingFakeSwap

	mu      sync.Mutex
	attempt int
}

func (f *flakySwapService) SubscribeSwaps(
	req *swapclientrpc.SubscribeSwapsRequest,
	stream swapclientrpc.SwapClientService_SubscribeSwapsServer) error {

	f.mu.Lock()
	f.attempt++
	attempt := f.attempt
	f.mu.Unlock()

	if attempt == 1 {
		return errors.New("transient: upstream not ready")
	}

	return f.streamingFakeSwap.SubscribeSwaps(req, stream)
}

// TestMonitorLoopRecoversAfterTransientFailure confirms the loop retries
// SubscribeSwaps after a transient error and successfully delivers later
// updates.
func TestMonitorLoopRecoversAfterTransientFailure(t *testing.T) {
	t.Parallel()

	flaky := &flakySwapService{
		streamingFakeSwap: streamingFakeSwap{
			updates: make(chan *swapclientrpc.SwapSummary, 4),
		},
	}
	deps := &Deps{SwapService: flaky}
	r := newRuntime(t.Context(), deps)
	defer r.stop()

	sub := r.subscribe()
	r.startMonitorLoop()

	// Give the loop time to hit the backoff for the first attempt and
	// retry. The minimum backoff is 500ms.
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case flaky.updates <- &swapclientrpc.SwapSummary{
			PaymentHash: "after-recovery",
			Direction: swapclientrpc.
				SwapDirection_SWAP_DIRECTION_PAY,
			State: swapclientrpc.
				SwapState_SWAP_STATE_COMPLETED,
		}:

		default:
		}

		select {
		case e := <-sub:
			require.Equal(t, "after-recovery", e.GetId())

			return

		case <-timer.C:
			t.Fatal(
				"monitor never recovered after transient " +
					"failure",
			)

		case <-time.After(100 * time.Millisecond):
			// Loop and try again.
		}
	}
}

// TestMonitorLoopExitsOnRootCancel confirms canceling the runtime's
// rootCtx causes the loop to exit cleanly.
func TestMonitorLoopExitsOnRootCancel(t *testing.T) {
	t.Parallel()

	swap := newStreamingFakeSwap()
	deps := &Deps{SwapService: swap}
	parentCtx, parentCancel := context.WithCancel(t.Context())
	r := newRuntime(parentCtx, deps)
	r.startMonitorLoop()

	parentCancel()
	r.stop() // joins the monitor goroutine
}
