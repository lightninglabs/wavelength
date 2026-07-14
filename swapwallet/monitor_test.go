//go:build walletdkrpc && swapruntime

package swapwallet

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/rpc/swapclientrpc"
	"github.com/lightninglabs/wavelength/rpc/walletdkrpc"
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
func drainOne(t *testing.T, sub *subscriber) *walletdkrpc.WalletEntry {
	t.Helper()
	select {
	case u, ok := <-sub.ch:
		require.True(t, ok, "subscriber channel closed unexpectedly")

		return u.entry

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
		t, walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE,
		entry.GetStatus(),
	)
}

// TestMonitorLoopLeavesSwapPendingToFSM confirms a swap row fanned out from
// the monitor reaches subscribers under its payment_hash id, but is not aged by
// the wallet-level deadline tracker even before its direction is populated.
func TestMonitorLoopLeavesSwapPendingToFSM(t *testing.T) {
	t.Parallel()

	swap := newStreamingFakeSwap()
	deps := &Deps{SwapService: swap}
	r := newRuntime(t.Context(), deps)
	defer r.stop()

	sub := r.subscribe()
	r.startMonitorLoop()

	swap.updates <- &swapclientrpc.SwapSummary{
		PaymentHash: "swap-hash",
		Pending:     true,
	}
	entry := drainOne(t, sub)
	require.Equal(
		t, "swap-hash", entry.GetId(),
		"swap row id must surface as payment_hash without further "+
			"projection",
	)
	require.Equal(
		t, walletdkrpc.EntryKind_ENTRY_KIND_UNSPECIFIED,
		entry.GetKind(),
	)

	r.pendingMu.Lock()
	_, ok := r.pending["swap-hash"]
	r.pendingMu.Unlock()
	require.False(
		t, ok, "monitor must leave pending swap rows to the swap FSM",
	)
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
		"to-complete", walletdkrpc.EntryKind_ENTRY_KIND_SEND,
		time.Now(),
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
// re-subscribe, and recover without manual intervention. Captures the
// include_existing flag from every subscribe attempt so tests can assert
// only the first one requested a snapshot.
type flakySwapService struct {
	streamingFakeSwap

	mu           sync.Mutex
	attempt      int
	includeFlags []bool
}

func (f *flakySwapService) SubscribeSwaps(
	req *swapclientrpc.SubscribeSwapsRequest,
	stream swapclientrpc.SwapClientService_SubscribeSwapsServer) error {

	f.mu.Lock()
	f.attempt++
	attempt := f.attempt
	f.includeFlags = append(f.includeFlags, req.GetIncludeExisting())
	f.mu.Unlock()

	if attempt == 1 {
		return errors.New("transient: upstream not ready")
	}

	return f.streamingFakeSwap.SubscribeSwaps(req, stream)
}

// capturedIncludeFlags returns a snapshot of include_existing flags seen
// across SubscribeSwaps attempts so tests can assert reconnect behaviour.
func (f *flakySwapService) capturedIncludeFlags() []bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]bool, len(f.includeFlags))
	copy(out, f.includeFlags)

	return out
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
		case u := <-sub.ch:
			require.Equal(t, "after-recovery", u.entry.GetId())

			// Reconnect must NOT replay the existing-row
			// snapshot. The first subscribe gets
			// include_existing=true; every reconnect gets false
			// so subscribers don't see duplicate terminal-state
			// events on every transient failure.
			flags := flaky.capturedIncludeFlags()
			require.GreaterOrEqual(
				t, len(flags), 2, "monitor must have "+
					"attempted at least one reconnect "+
					"after the transient error",
			)
			require.True(
				t, flags[0],
				"first subscribe must request the snapshot",
			)
			for i := 1; i < len(flags); i++ {
				require.False(
					t, flags[i], "reconnect #%d must "+
						"NOT request the snapshot", i,
				)
			}

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

// TestMonitorLoopTerminalStatusBeatsStaleOverlay asserts that a stale
// FAILED-overlay (left over from a deadline tick that fired before the
// swap actually completed) does NOT corrupt a subsequent terminal-state
// update from the swap subsystem. The source-of-truth COMPLETE status
// must win, and clearPending must release the pending tracker so the
// next deadline tick cannot re-overlay an already-terminal row.
func TestMonitorLoopTerminalStatusBeatsStaleOverlay(t *testing.T) {
	t.Parallel()

	swap := newStreamingFakeSwap()
	deps := &Deps{SwapService: swap}
	r := newRuntime(t.Context(), deps)
	defer r.stop()

	// Set up the conditions: a PENDING entry tracked by the runtime
	// with a stale "timed_out" overlay written by an earlier deadline
	// tick.
	r.trackPending(
		"hash-late", walletdkrpc.EntryKind_ENTRY_KIND_SEND,
		time.Now().Add(-time.Hour),
	)
	r.pendingMu.Lock()
	r.overlay["hash-late"] = overlayStatus{
		status:        walletdkrpc.EntryStatus_ENTRY_STATUS_FAILED,
		failureReason: "timed_out",
	}
	r.pendingMu.Unlock()

	sub := r.subscribe()
	r.startMonitorLoop()

	// The swap actually completed: push the terminal summary.
	swap.updates <- &swapclientrpc.SwapSummary{
		PaymentHash: "hash-late",
		Direction:   swapclientrpc.SwapDirection_SWAP_DIRECTION_PAY,
		State:       swapclientrpc.SwapState_SWAP_STATE_COMPLETED,
	}

	got := drainOne(t, sub)
	require.Equal(
		t, walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE,
		got.GetStatus(),
		"terminal source status must beat the stale FAILED overlay",
	)
	require.Empty(
		t, got.GetFailureReason(),
		"failure_reason must not carry the stale timed_out string "+
			"once the swap completes",
	)

	r.pendingMu.Lock()
	_, stillPending := r.pending["hash-late"]
	r.pendingMu.Unlock()
	require.False(
		t, stillPending,
		"a terminal source status must release the pending tracker",
	)
}

// TestMonitorLoopPendingSwapBeatsStaleOverlay asserts that a PENDING source
// update from the swap subsystem clears any stale wallet-level timeout overlay,
// even before the swap direction is populated.
func TestMonitorLoopPendingSwapBeatsStaleOverlay(t *testing.T) {
	t.Parallel()

	swap := newStreamingFakeSwap()
	deps := &Deps{SwapService: swap}
	r := newRuntime(t.Context(), deps)
	defer r.stop()

	r.trackPending(
		"hash-stuck", walletdkrpc.EntryKind_ENTRY_KIND_SEND,
		time.Now().Add(-time.Hour),
	)
	r.pendingMu.Lock()
	r.overlay["hash-stuck"] = overlayStatus{
		status:        walletdkrpc.EntryStatus_ENTRY_STATUS_FAILED,
		failureReason: "timed_out",
	}
	r.pendingMu.Unlock()

	sub := r.subscribe()
	r.startMonitorLoop()

	// The swap subsystem still reports the swap as in-flight.
	swap.updates <- &swapclientrpc.SwapSummary{
		PaymentHash: "hash-stuck",
		Pending:     true,
	}

	got := drainOne(t, sub)
	require.Equal(
		t, walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING,
		got.GetStatus(),
		"swap source status must beat a stale wallet timeout overlay",
	)
	require.Empty(t, got.GetFailureReason())
	require.Equal(
		t, walletdkrpc.EntryKind_ENTRY_KIND_UNSPECIFIED, got.GetKind(),
	)

	r.pendingMu.Lock()
	_, overlayKept := r.overlay["hash-stuck"]
	r.pendingMu.Unlock()
	require.False(
		t, overlayKept, "a PENDING swap update must clear the "+
			"stale wallet-layer overlay",
	)
}
