//go:build walletdkrpc && swapruntime

package swapwallet

import (
	"testing"
	"time"

	"github.com/lightninglabs/darepo-client/rpc/walletdkrpc"
	"github.com/stretchr/testify/require"
)

// TestDeadlineWatcherFlagsStuckEntries asserts that pending entries past
// their wallet-level deadline are overlaid as FAILED with the timeout
// reason, while entries still inside their deadline window are left
// alone. Uses a short WalletDeadline so the test can drive applyDeadlines
// past it without sleeping. The deadline base is the first-observation
// time (per H-6), so the test simulates "stale" by advancing the
// applyDeadlines clock past the deadline.
func TestDeadlineWatcherFlagsStuckEntries(t *testing.T) {
	t.Parallel()

	deadline := 100 * time.Millisecond
	deps := &Deps{
		WalletDeadline: deadline,
	}
	r := newRuntime(t.Context(), deps)
	defer r.stop()

	freshID := "fresh"
	staleID := "stale"

	r.trackPending(
		freshID, walletdkrpc.EntryKind_ENTRY_KIND_EXIT, time.Now(),
	)
	r.trackPending(
		staleID, walletdkrpc.EntryKind_ENTRY_KIND_DEPOSIT, time.Now(),
	)

	// Tick the watcher past the deadline window. Both entries were
	// first-observed essentially "now", so a tick at now+2*deadline is
	// past both.
	future := time.Now().Add(2 * deadline)
	r.applyDeadlines(future)

	overlay, staleTimedOut := r.overlayFor(staleID)
	require.True(
		t, staleTimedOut,
		"entry past its deadline must be flagged as timed out",
	)
	require.Equal(
		t, walletdkrpc.EntryStatus_ENTRY_STATUS_FAILED, overlay.status,
	)
	require.Equal(t, "timed_out", overlay.failureReason)
	require.Equal(t, timedOutCode, overlay.failureCode)

	// freshID is identical to staleID for the watcher purposes here
	// (both deadlines are equally past). To assert the "fresh" guard
	// works, tick at a time INSIDE the deadline window.
	r2 := newRuntime(t.Context(), deps)
	defer r2.stop()
	r2.trackPending(
		freshID, walletdkrpc.EntryKind_ENTRY_KIND_EXIT, time.Now(),
	)
	r2.applyDeadlines(time.Now())
	_, freshTimedOut := r2.overlayFor(freshID)
	require.False(
		t, freshTimedOut,
		"entry inside its deadline must not be flagged as timed out",
	)
}

// TestDeadlineWatcherIgnoresSwapRows asserts SEND/RECV rows are left to the
// swap FSM instead of receiving a wallet-level timeout overlay.
func TestDeadlineWatcherIgnoresSwapRows(t *testing.T) {
	t.Parallel()

	deadline := 50 * time.Millisecond
	r := newRuntime(t.Context(), &Deps{WalletDeadline: deadline})
	defer r.stop()

	r.trackPending(
		"send", walletdkrpc.EntryKind_ENTRY_KIND_SEND, time.Now(),
	)
	r.trackPending(
		"recv", walletdkrpc.EntryKind_ENTRY_KIND_RECV, time.Now(),
	)

	r.applyDeadlines(time.Now().Add(2 * deadline))

	_, sendTimedOut := r.overlayFor("send")
	_, recvTimedOut := r.overlayFor("recv")
	require.False(t, sendTimedOut)
	require.False(t, recvTimedOut)

	r.pendingMu.Lock()
	defer r.pendingMu.Unlock()
	require.NotContains(t, r.pending, "send")
	require.NotContains(t, r.pending, "recv")
}

// TestDeadlineWatcherIdempotent asserts that running applyDeadlines twice
// on the same stale entry does not produce a different overlay state.
func TestDeadlineWatcherIdempotent(t *testing.T) {
	t.Parallel()

	deadline := 50 * time.Millisecond
	deps := &Deps{
		WalletDeadline: deadline,
	}
	r := newRuntime(t.Context(), deps)
	defer r.stop()

	id := "long-stuck"
	r.trackPending(id, walletdkrpc.EntryKind_ENTRY_KIND_EXIT, time.Now())

	future := time.Now().Add(2 * deadline)
	r.applyDeadlines(future)
	first, ok := r.overlayFor(id)
	require.True(t, ok)

	r.applyDeadlines(future)
	second, ok := r.overlayFor(id)
	require.True(t, ok)
	require.Equal(
		t, first, second,
		"second applyDeadlines must not mutate overlay state",
	)
}

// TestClearPendingDropsOverlay asserts that clearPending removes both the
// pending record AND any overlay so a subsequent reuse of the same id (a
// caller resubmits) starts clean.
func TestClearPendingDropsOverlay(t *testing.T) {
	t.Parallel()

	deadline := 50 * time.Millisecond
	deps := &Deps{
		WalletDeadline: deadline,
	}
	r := newRuntime(t.Context(), deps)
	defer r.stop()

	id := "transient"
	r.trackPending(id, walletdkrpc.EntryKind_ENTRY_KIND_EXIT, time.Now())
	r.applyDeadlines(time.Now().Add(2 * deadline))
	_, ok := r.overlayFor(id)
	require.True(t, ok)

	r.clearPending(id)
	_, ok = r.overlayFor(id)
	require.False(t, ok, "clearPending must drop the overlay")
}

// TestTrackPendingBasesDeadlineOnFirstObservation asserts that an entry
// trackPending'd with a stale createdAt (e.g. a restored wallet-local
// operation submitted hours ago) gets a FRESH
// deadline based on time.Now(), not on the source row's submit time.
// Without this guard a restart would flip every long-pending wallet row to
// FAILED(timed_out) within the first deadline tick.
func TestTrackPendingBasesDeadlineOnFirstObservation(t *testing.T) {
	t.Parallel()

	deadline := 5 * time.Minute
	deps := &Deps{WalletDeadline: deadline}
	r := newRuntime(t.Context(), deps)
	defer r.stop()

	// Simulate a restored wallet-local row submitted 24h ago.
	stale := time.Now().Add(-24 * time.Hour)
	r.trackPending(
		"backfilled", walletdkrpc.EntryKind_ENTRY_KIND_EXIT, stale,
	)

	r.pendingMu.Lock()
	entry, ok := r.pending["backfilled"]
	r.pendingMu.Unlock()
	require.True(t, ok)

	// The original createdAt is preserved for display, but the deadline
	// is computed from now.
	require.True(
		t, entry.createdAt.Equal(stale),
		"createdAt should preserve the original submit time",
	)
	require.True(
		t,
		entry.deadline.After(
			time.Now().Add(deadline/2),
		),
		"deadline must be in the FUTURE, not based on a 24h-old "+
			"createdAt: got %s",
		entry.deadline,
	)
}

// TestTrackPendingIdempotentPreservesOriginalDeadline asserts that
// follow-up trackPending calls for the same id do not extend the
// deadline. Otherwise every monitor refresh would slide the deadline
// forward indefinitely and the watcher could never time out a row.
func TestTrackPendingIdempotentPreservesOriginalDeadline(t *testing.T) {
	t.Parallel()

	deps := &Deps{WalletDeadline: 5 * time.Minute}
	r := newRuntime(t.Context(), deps)
	defer r.stop()

	r.trackPending(
		"id", walletdkrpc.EntryKind_ENTRY_KIND_EXIT, time.Now(),
	)
	r.pendingMu.Lock()
	first := r.pending["id"].deadline
	r.pendingMu.Unlock()

	// Sleep a small amount and re-track; the deadline must NOT advance.
	time.Sleep(10 * time.Millisecond)
	r.trackPending(
		"id", walletdkrpc.EntryKind_ENTRY_KIND_EXIT, time.Now(),
	)
	r.pendingMu.Lock()
	second := r.pending["id"].deadline
	r.pendingMu.Unlock()

	require.True(
		t, first.Equal(second),
		"subsequent trackPending must NOT advance the deadline",
	)
}

// TestDeadlineWatcherEmitsTimeoutToSubscribers asserts that elevating
// an entry to FAILED via the deadline overlay also pushes a synthesized
// WalletEntry to every live SubscribeWallet subscriber. Without this
// emission a long-lived subscriber would never observe the timeout (the
// hung swap, by hypothesis, never drives another monitor push).
func TestDeadlineWatcherEmitsTimeoutToSubscribers(t *testing.T) {
	t.Parallel()

	deadline := 50 * time.Millisecond
	deps := &Deps{
		WalletDeadline: deadline,
	}
	r := newRuntime(t.Context(), deps)
	defer r.stop()

	sub := r.subscribe()

	r.trackPending(
		"stuck", walletdkrpc.EntryKind_ENTRY_KIND_EXIT, time.Now(),
	)
	tick := time.Now().Add(2 * deadline)
	r.applyDeadlines(tick)

	select {
	case got := <-sub.ch:
		require.Equal(t, "stuck", got.entry.GetId())
		require.Equal(
			t, walletdkrpc.EntryKind_ENTRY_KIND_EXIT,
			got.entry.GetKind(),
		)
		require.Equal(
			t, walletdkrpc.EntryStatus_ENTRY_STATUS_FAILED,
			got.entry.GetStatus(),
		)
		require.Equal(t, "timed_out", got.entry.GetFailureReason())
		require.Equal(t, timedOutCode, got.entry.GetFailureCode())
		require.Equal(
			t, tick.Unix(), got.entry.GetUpdatedAtUnix(),
			"updated_at must reflect the watcher tick time",
		)

	case <-time.After(time.Second):
		t.Fatal(
			"subscriber must observe the deadline transition " +
				"without polling List",
		)
	}
}

// TestTrackPendingEntryPreservesTimestampsOnRefresh confirms that refreshing a
// wallet-local pending row cannot make an existing activity row look new again.
// Some later sources rebuild entries with fallback timestamps, so the runtime
// keeps the original created/updated values while still accepting refreshed
// metadata like amount and counterparty.
func TestTrackPendingEntryPreservesTimestampsOnRefresh(t *testing.T) {
	t.Parallel()

	r := newRuntime(t.Context(), &Deps{})
	defer r.stop()

	r.trackPendingEntry(&walletdkrpc.WalletEntry{
		Id:            "exit-outpoint:0",
		Kind:          walletdkrpc.EntryKind_ENTRY_KIND_EXIT,
		Status:        walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING,
		AmountSat:     -1_000,
		Counterparty:  "first",
		CreatedAtUnix: 100,
		UpdatedAtUnix: 100,
	})
	r.trackPendingEntry(&walletdkrpc.WalletEntry{
		Id:            "exit-outpoint:0",
		Kind:          walletdkrpc.EntryKind_ENTRY_KIND_EXIT,
		Status:        walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING,
		AmountSat:     -2_000,
		Counterparty:  "refreshed",
		CreatedAtUnix: 200,
		UpdatedAtUnix: 250,
	})

	entries := r.pendingSnapshot()
	require.Len(t, entries, 1)
	require.Equal(t, int64(100), entries[0].GetCreatedAtUnix())
	require.Equal(t, int64(100), entries[0].GetUpdatedAtUnix())
	require.Equal(t, int64(-2_000), entries[0].GetAmountSat())
	require.Equal(t, "refreshed", entries[0].GetCounterparty())
}

// TestDeadlineWatcherDoesNotReEmitAlreadyTimedOut asserts that running
// applyDeadlines again on the same tick does not re-emit; the watcher
// only emits on the elevation edge.
func TestDeadlineWatcherDoesNotReEmitAlreadyTimedOut(t *testing.T) {
	t.Parallel()

	deadline := 50 * time.Millisecond
	deps := &Deps{
		WalletDeadline: deadline,
	}
	r := newRuntime(t.Context(), deps)
	defer r.stop()

	sub := r.subscribe()

	r.trackPending(
		"stuck", walletdkrpc.EntryKind_ENTRY_KIND_EXIT, time.Now(),
	)
	tick := time.Now().Add(2 * deadline)
	r.applyDeadlines(tick)
	<-sub.ch // drain the first emit

	r.applyDeadlines(tick)
	select {
	case <-sub.ch:
		t.Fatal(
			"watcher must not re-emit on an already-timed-out " +
				"entry",
		)

	case <-time.After(50 * time.Millisecond):
	}
}

// TestDeadlineWatcherSkipsNoTimeoutEntries asserts that wallet-local rows
// explicitly marked as source-owned stay pending instead of receiving a
// synthetic FAILED overlay from the generic wallet deadline. Cooperative
// leave uses this mode because the confirmed sweep row has a different v1 id,
// so timing out the pending stub would create a false failed exit beside the
// later successful on-chain row.
func TestDeadlineWatcherSkipsNoTimeoutEntries(t *testing.T) {
	t.Parallel()

	deadline := 50 * time.Millisecond
	deps := &Deps{
		WalletDeadline: deadline,
	}
	r := newRuntime(t.Context(), deps)
	defer r.stop()

	sub := r.subscribe()
	entry := leaveEntryStub("", []string{"exit:0"}, "bcrt1qdest", 1_000, "")
	r.trackPendingEntryWithoutTimeout(entry)

	r.applyDeadlines(time.Now().Add(2 * deadline))

	_, ok := r.overlayFor("exit:0")
	require.False(t, ok, "no-timeout row must not receive overlay")

	select {
	case got := <-sub.ch:
		t.Fatalf("no-timeout row must not emit timeout update: %v",
			got.entry)

	case <-time.After(50 * time.Millisecond):
	}
}

// TestSubscribeFanOutAndFlagsOverflow asserts that emit delivers updates to
// live subscribers with their event_seq, and on a saturated buffer sets the
// subscriber's overflowed flag (so the handler can signal a gap) rather than
// dropping silently or blocking the runtime.
func TestSubscribeFanOutAndFlagsOverflow(t *testing.T) {
	t.Parallel()

	deps := &Deps{}
	r := newRuntime(t.Context(), deps)
	defer r.stop()

	fast := r.subscribe()
	slow := r.subscribe()

	entry := &walletdkrpc.WalletEntry{
		Id:   "abc",
		Kind: walletdkrpc.EntryKind_ENTRY_KIND_EXIT,
	}
	r.emit(1, entry)

	select {
	case got := <-fast.ch:
		require.Equal(t, entry, got.entry)
		require.Equal(t, int64(1), got.seq)

	case <-time.After(time.Second):
		t.Fatal("fast subscriber did not receive update")
	}

	// Saturate slow's buffer so subsequent emits cannot enqueue. The
	// runtime must not block, and slow must be flagged overflowed rather
	// than losing the updates silently.
	for i := 0; i < int(defaultSubscribeBufferConst)+5; i++ {
		r.emit(int64(i+2), entry)
	}
	require.True(
		t, slow.overflowed.Load(),
		"a saturated subscriber must be flagged overflowed",
	)

	// Drain fast so the runtime is not stuck on it either.
	for {
		select {
		case <-fast.ch:
		default:
			r.unsubscribe(slow)

			return
		}
	}
}
