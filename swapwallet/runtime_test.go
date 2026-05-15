//go:build walletrpc && swapruntime

package swapwallet

import (
	"testing"
	"time"

	"github.com/lightninglabs/darepo-client/rpc/walletrpc"
	"github.com/stretchr/testify/require"
)

// TestDeadlineWatcherFlagsStuckEntries asserts that pending entries past
// their wallet-level deadline are overlaid as FAILED with the timeout
// reason, while entries still inside their deadline window are left alone.
func TestDeadlineWatcherFlagsStuckEntries(t *testing.T) {
	t.Parallel()

	deps := &Deps{
		WalletDeadline: 5 * time.Minute,
	}
	r := newRuntime(t.Context(), deps)
	defer r.stop()

	now := time.Now()
	freshID := "fresh"
	staleID := "stale"

	r.trackPending(
		freshID, walletrpc.EntryKind_ENTRY_KIND_SEND,
		now.Add(-1*time.Minute),
	)
	r.trackPending(
		staleID, walletrpc.EntryKind_ENTRY_KIND_RECV,
		now.Add(-10*time.Minute),
	)

	r.applyDeadlines(now)

	_, freshTimedOut := r.overlayFor(freshID)
	require.False(t, freshTimedOut, "fresh entry must not be timed out")

	overlay, staleTimedOut := r.overlayFor(staleID)
	require.True(t, staleTimedOut, "stale entry must be flagged as timed out")
	require.Equal(t,
		walletrpc.EntryStatus_ENTRY_STATUS_FAILED, overlay.status,
	)
	require.Equal(t, "timed_out", overlay.failureReason)
}

// TestDeadlineWatcherIdempotent asserts that running applyDeadlines twice
// on the same stale entry does not produce a different overlay state. The
// watcher must be safe to invoke on the same tick boundary across reloads
// or test fixtures.
func TestDeadlineWatcherIdempotent(t *testing.T) {
	t.Parallel()

	deps := &Deps{
		WalletDeadline: 5 * time.Minute,
	}
	r := newRuntime(t.Context(), deps)
	defer r.stop()

	now := time.Now()
	id := "long-stuck"

	r.trackPending(
		id, walletrpc.EntryKind_ENTRY_KIND_SEND,
		now.Add(-10*time.Minute),
	)

	r.applyDeadlines(now)
	first, ok := r.overlayFor(id)
	require.True(t, ok)

	r.applyDeadlines(now)
	second, ok := r.overlayFor(id)
	require.True(t, ok)
	require.Equal(t, first, second,
		"second applyDeadlines must not mutate overlay state")
}

// TestClearPendingDropsOverlay asserts that clearPending removes both the
// pending record AND any overlay so a subsequent reuse of the same id (a
// caller resubmits) starts clean.
func TestClearPendingDropsOverlay(t *testing.T) {
	t.Parallel()

	deps := &Deps{
		WalletDeadline: 5 * time.Minute,
	}
	r := newRuntime(t.Context(), deps)
	defer r.stop()

	now := time.Now()
	id := "transient"
	r.trackPending(
		id, walletrpc.EntryKind_ENTRY_KIND_SEND,
		now.Add(-10*time.Minute),
	)
	r.applyDeadlines(now)
	_, ok := r.overlayFor(id)
	require.True(t, ok)

	r.clearPending(id)
	_, ok = r.overlayFor(id)
	require.False(t, ok, "clearPending must drop the overlay")
}

// TestSubscribeFanOutAndDropOnSlowConsumer asserts that emit delivers
// updates to live subscribers and drops on a saturated buffer rather than
// blocking the runtime.
func TestSubscribeFanOutAndDropOnSlowConsumer(t *testing.T) {
	t.Parallel()

	deps := &Deps{}
	r := newRuntime(t.Context(), deps)
	defer r.stop()

	fast := r.subscribe()
	slow := r.subscribe()

	entry := &walletrpc.WalletEntry{
		Id:   "abc",
		Kind: walletrpc.EntryKind_ENTRY_KIND_SEND,
	}
	r.emit(entry)

	select {
	case got := <-fast:
		require.Equal(t, entry, got)
	case <-time.After(time.Second):
		t.Fatal("fast subscriber did not receive update")
	}

	// Now saturate slow's buffer so subsequent emits are dropped on
	// that subscriber. The runtime must not block.
	for i := 0; i < int(defaultSubscribeBufferConst)+5; i++ {
		r.emit(entry)
	}

	// Drain fast so the runtime is not stuck on it either.
	for {
		select {
		case <-fast:
		default:
			r.unsubscribe(slow)

			return
		}
	}
}
