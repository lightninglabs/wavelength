package actor

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// fakeDeadLetterStore is an in-memory DeadLetterStore for monitor tests.
type fakeDeadLetterStore struct {
	mu sync.Mutex

	entries []DeadLetter

	purgeCutoffs []time.Time
}

// add appends a dead letter to the fake store.
func (f *fakeDeadLetterStore) add(dl DeadLetter) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.entries = append(f.entries, dl)
}

// GetDeadLetter retrieves a specific dead letter message.
func (f *fakeDeadLetterStore) GetDeadLetter(_ context.Context, id string) (
	*DeadLetter, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	for _, dl := range f.entries {
		if dl.ID == id {
			found := dl

			return &found, nil
		}
	}

	return nil, nil
}

// ListDeadLetters lists dead letters for an actor.
func (f *fakeDeadLetterStore) ListDeadLetters(_ context.Context, actorID string,
	limit int) ([]DeadLetter, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	var result []DeadLetter
	for _, dl := range f.entries {
		if dl.ActorID == actorID && len(result) < limit {
			result = append(result, dl)
		}
	}

	return result, nil
}

// ListAllDeadLetters lists dead letters across all actors.
func (f *fakeDeadLetterStore) ListAllDeadLetters(_ context.Context, limit,
	offset int) ([]DeadLetter, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	if offset >= len(f.entries) {
		return nil, nil
	}

	end := offset + limit
	if end > len(f.entries) {
		end = len(f.entries)
	}

	return append([]DeadLetter{}, f.entries[offset:end]...), nil
}

// ListDeadLettersSince lists dead letters strictly after the (since, afterID)
// cursor in (created_at, id) order, mirroring the store's keyset pagination.
func (f *fakeDeadLetterStore) ListDeadLettersSince(_ context.Context,
	since time.Time, afterID string, limit int) ([]DeadLetter, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	matches := make([]DeadLetter, 0, len(f.entries))
	for _, dl := range f.entries {
		created := dl.CreatedAt.Unix()
		if created > since.Unix() ||
			(created == since.Unix() && dl.ID > afterID) {

			matches = append(matches, dl)
		}
	}

	sort.Slice(matches, func(i, j int) bool {
		ci, cj := matches[i].CreatedAt.Unix(), matches[j].CreatedAt.Unix()
		if ci != cj {
			return ci < cj
		}

		return matches[i].ID < matches[j].ID
	})

	if len(matches) > limit {
		matches = matches[:limit]
	}

	return matches, nil
}

// CountDeadLetters counts all parked dead letters.
func (f *fakeDeadLetterStore) CountDeadLetters(_ context.Context) (int64,
	error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	return int64(len(f.entries)), nil
}

// CountDeadLettersByActor tallies parked dead letters per actor.
func (f *fakeDeadLetterStore) CountDeadLettersByActor(_ context.Context) (
	[]DeadLetterCount, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	counts := make(map[string]int64)
	for _, dl := range f.entries {
		counts[dl.ActorID]++
	}

	result := make([]DeadLetterCount, 0, len(counts))
	for actorID, count := range counts {
		result = append(result, DeadLetterCount{
			ActorID: actorID,
			Count:   count,
		})
	}

	return result, nil
}

// RequeueDeadLetter is unused by the monitor.
func (f *fakeDeadLetterStore) RequeueDeadLetter(_ context.Context, _ string) (
	string, error) {

	return "", ErrDeadLetterNotFound
}

// DeleteDeadLetter removes a dead letter.
func (f *fakeDeadLetterStore) DeleteDeadLetter(_ context.Context,
	id string) error {

	f.mu.Lock()
	defer f.mu.Unlock()

	for i, dl := range f.entries {
		if dl.ID == id {
			f.entries = append(f.entries[:i], f.entries[i+1:]...)
			break
		}
	}

	return nil
}

// PurgeDeadLetters records the cutoff and removes matching entries.
func (f *fakeDeadLetterStore) PurgeDeadLetters(_ context.Context,
	olderThan time.Time) (int64, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	f.purgeCutoffs = append(f.purgeCutoffs, olderThan)

	var (
		kept    []DeadLetter
		removed int64
	)
	for _, dl := range f.entries {
		if dl.CreatedAt.Before(olderThan) {
			removed++
			continue
		}

		kept = append(kept, dl)
	}
	f.entries = kept

	return removed, nil
}

// cutoffs returns a snapshot of the recorded purge cutoffs.
func (f *fakeDeadLetterStore) cutoffs() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]time.Time{}, f.purgeCutoffs...)
}

// fakeMaintenanceStore records CleanupExpired invocations.
type fakeMaintenanceStore struct {
	mu    sync.Mutex
	calls int
}

// CleanupExpired records the invocation.
func (f *fakeMaintenanceStore) CleanupExpired(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls++

	return nil
}

// callCount returns the number of CleanupExpired invocations.
func (f *fakeMaintenanceStore) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.calls
}

// Compile-time interface checks for the fakes.
var (
	_ DeadLetterStore  = (*fakeDeadLetterStore)(nil)
	_ MaintenanceStore = (*fakeMaintenanceStore)(nil)
)

// newMonitorHarness builds a monitor over the fake store with fast intervals
// and an OnDeadLetter hook that feeds the returned channel.
func newMonitorHarness(t *testing.T, store *fakeDeadLetterStore,
	maint MaintenanceStore, retention time.Duration,
	clk clock.Clock) (*DeadLetterMonitor, chan DeadLetter) {

	t.Helper()

	observed := make(chan DeadLetter, 100)

	monitor := NewDeadLetterMonitor(DeadLetterMonitorConfig{
		Store:         store,
		Maintenance:   maint,
		ScanInterval:  10 * time.Millisecond,
		SweepInterval: 20 * time.Millisecond,
		Retention:     retention,
		Clock:         clk,
		OnDeadLetter: func(dl DeadLetter) {
			observed <- dl
		},
	})

	t.Cleanup(monitor.Stop)

	return monitor, observed
}

// TestDeadLetterMonitorReportsNewEntries asserts each newly parked dead
// letter is surfaced exactly once, and that entries stamped in the still-
// open wall-clock second are deferred until the second closes (the keyset
// cursor must never advance into an open second, since dead-letter IDs are
// minted at enqueue time and a later same-second write can sort before the
// cursor).
func TestDeadLetterMonitorReportsNewEntries(t *testing.T) {
	t.Parallel()

	store := &fakeDeadLetterStore{}
	clk := clock.NewTestClock(time.Now())

	monitor, observed := newMonitorHarness(t, store, nil, 0, clk)
	monitor.Start()

	// Two entries landing in the monitor's start second. While the clock
	// sits inside that second, the scan must defer them.
	store.add(DeadLetter{
		ID:        "dl-1",
		ActorID:   "actor-1",
		Source:    "mailbox",
		CreatedAt: clk.Now(),
	})
	store.add(DeadLetter{
		ID:        "dl-2",
		ActorID:   "actor-2",
		Source:    "mailbox",
		CreatedAt: clk.Now(),
	})

	select {
	case dl := <-observed:
		t.Fatalf("entry %s from the open second must be deferred",
			dl.ID)

	case <-time.After(100 * time.Millisecond):
	}

	// Closing the second releases both, exactly once each.
	clk.SetTime(clk.Now().Add(2 * time.Second))

	seen := make(map[string]int)
	for range 2 {
		select {
		case dl := <-observed:
			seen[dl.ID]++

		case <-time.After(2 * time.Second):
			t.Fatal("expected dead letters to be observed")
		}
	}

	require.Equal(t, map[string]int{"dl-1": 1, "dl-2": 1}, seen)

	// A later entry is observed too once its second closes, and the
	// earlier ones are never re-reported despite dozens of intervening
	// scans.
	store.add(DeadLetter{
		ID:        "dl-3",
		ActorID:   "actor-1",
		Source:    "mailbox",
		CreatedAt: clk.Now(),
	})
	clk.SetTime(clk.Now().Add(2 * time.Second))

	select {
	case dl := <-observed:
		require.Equal(t, "dl-3", dl.ID)

	case <-time.After(2 * time.Second):
		t.Fatal("expected the later dead letter to be observed")
	}

	select {
	case dl := <-observed:
		t.Fatalf("dead letter %s reported twice", dl.ID)

	case <-time.After(100 * time.Millisecond):
	}
}

// TestDeadLetterMonitorSameSecondFlood asserts a flood of entries sharing
// one created_at second larger than the scan batch limit is fully surfaced,
// exactly once each. This is the regression test for the scan livelock: a
// created_at-only boundary would re-read the same first page forever and
// never reach the tail of the flood.
func TestDeadLetterMonitorSameSecondFlood(t *testing.T) {
	t.Parallel()

	store := &fakeDeadLetterStore{}
	clk := clock.NewTestClock(time.Now())

	monitor, observed := newMonitorHarness(t, store, nil, 0, clk)
	monitor.Start()

	// Park 2.5x the batch limit in a single second.
	const flood = 2*deadLetterScanBatchLimit + 50
	for i := range flood {
		store.add(DeadLetter{
			ID:        fmt.Sprintf("flood-%04d", i),
			ActorID:   "actor-1",
			Source:    "mailbox",
			CreatedAt: clk.Now(),
		})
	}

	clk.SetTime(clk.Now().Add(2 * time.Second))

	seen := make(map[string]int)
	for range flood {
		select {
		case dl := <-observed:
			seen[dl.ID]++

		case <-time.After(5 * time.Second):
			t.Fatalf("flood drain stalled: %d of %d observed",
				len(seen), flood)
		}
	}

	require.Len(t, seen, flood)
	for id, count := range seen {
		require.Equal(
			t, 1, count, "entry %s reported %d times", id, count,
		)
	}

	select {
	case dl := <-observed:
		t.Fatalf("dead letter %s reported twice", dl.ID)

	case <-time.After(100 * time.Millisecond):
	}
}

// TestDeadLetterMonitorSkipsBacklog asserts entries parked before the
// monitor started are summarized but not individually re-surfaced through
// the hook.
func TestDeadLetterMonitorSkipsBacklog(t *testing.T) {
	t.Parallel()

	store := &fakeDeadLetterStore{}
	clk := clock.NewTestClock(time.Now())

	// Parked well before the monitor starts.
	store.add(DeadLetter{
		ID:        "dl-old",
		ActorID:   "actor-1",
		Source:    "mailbox",
		CreatedAt: clk.Now().Add(-time.Hour),
	})

	monitor, observed := newMonitorHarness(t, store, nil, 0, clk)
	monitor.Start()

	select {
	case dl := <-observed:
		t.Fatalf("backlog entry %s must not re-surface", dl.ID)

	case <-time.After(100 * time.Millisecond):
	}
}

// TestDeadLetterMonitorRetention asserts the sweep purges with the
// configured retention cutoff and drives the maintenance cleanup, and that
// zero retention disables the purge entirely while cleanup still runs.
func TestDeadLetterMonitorRetention(t *testing.T) {
	t.Parallel()

	store := &fakeDeadLetterStore{}
	maint := &fakeMaintenanceStore{}
	baseTime := time.Now()
	clk := clock.NewTestClock(baseTime)

	retention := time.Hour
	monitor, _ := newMonitorHarness(t, store, maint, retention, clk)
	monitor.Start()

	require.Eventually(t, func() bool {
		return len(store.cutoffs()) >= 1 && maint.callCount() >= 1
	}, 2*time.Second, 10*time.Millisecond)

	// Every recorded cutoff is exactly now-retention against the test
	// clock.
	for _, cutoff := range store.cutoffs() {
		require.Equal(t, baseTime.Add(-retention).Unix(), cutoff.Unix())
	}
}

// TestDeadLetterMonitorRetentionDisabled asserts zero retention never
// purges, while the maintenance cleanup still runs on the sweep cadence.
func TestDeadLetterMonitorRetentionDisabled(t *testing.T) {
	t.Parallel()

	store := &fakeDeadLetterStore{}
	maint := &fakeMaintenanceStore{}
	clk := clock.NewTestClock(time.Now())

	monitor, _ := newMonitorHarness(t, store, maint, 0, clk)
	monitor.Start()

	require.Eventually(t, func() bool {
		return maint.callCount() >= 2
	}, 2*time.Second, 10*time.Millisecond)

	require.Empty(t, store.cutoffs())
}

// TestDeadLetterMonitorStopIdempotent asserts Start/Stop are safe to call
// repeatedly and Stop terminates the loop.
func TestDeadLetterMonitorStopIdempotent(t *testing.T) {
	t.Parallel()

	store := &fakeDeadLetterStore{}
	monitor, _ := newMonitorHarness(
		t, store, nil, 0,
		clock.NewTestClock(
			time.Now(),
		),
	)

	monitor.Start()
	monitor.Start()

	monitor.Stop()
	monitor.Stop()
}
