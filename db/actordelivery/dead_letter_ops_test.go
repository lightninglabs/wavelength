package actordelivery

import (
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/stretchr/testify/require"
)

// enqueueAndDeadLetter enqueues a message with the given params and
// immediately moves it to the dead-letter queue, returning the failure
// reason used.
func enqueueAndDeadLetter(t *testing.T, store *testActorDeliveryStore,
	params actor.EnqueueParams) string {

	t.Helper()

	ctx := t.Context()

	err := store.EnqueueMessage(ctx, params)
	require.NoError(t, err)

	reason := "test failure: " + params.ID
	err = store.MoveToDeadLetter(ctx, params.ID, reason)
	require.NoError(t, err)

	return reason
}

// TestDeadLetterCarriesRoutingFields asserts that MoveToDeadLetter projects
// the full routing identity of the mailbox row into the dead letter, so a
// later requeue can reconstruct the message faithfully.
func TestDeadLetterCarriesRoutingFields(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newActorDeliveryStoreForTest(t)

	enqueueAndDeadLetter(t, store, actor.EnqueueParams{
		ID:              "msg-routing",
		MailboxID:       "actor-1",
		MessageType:     "test.Message",
		Payload:         []byte{1, 2, 3},
		PromiseID:       "promise-9",
		CallbackActorID: "caller-7",
		CorrelationID:   "corr-5",
		CorrelationKey:  "lane-a",
		Priority:        42,
		AvailableAt:     time.Now().Add(-time.Minute),
		MaxAttempts:     7,
	})

	dl, err := store.GetDeadLetter(ctx, "msg-routing")
	require.NoError(t, err)
	require.NotNil(t, dl)

	require.Equal(t, "promise-9", dl.PromiseID)
	require.Equal(t, "caller-7", dl.CallbackActorID)
	require.Equal(t, "corr-5", dl.CorrelationID)
	require.Equal(t, "lane-a", dl.CorrelationKey)
	require.Equal(t, 42, dl.Priority)
	require.Equal(t, 7, dl.MaxAttempts)
}

// TestDeadLetterRequeue asserts the core recovery path: a requeued dead
// letter becomes a leasable mailbox message again under a fresh ID, with
// every routing field preserved, the retry counter reset, and the dead
// letter removed. The fresh ID matters because the retry-exhaustion path
// marks the original ID as processed, which would make a same-ID requeue
// invisible to the deduplication check.
func TestDeadLetterRequeue(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newActorDeliveryStoreForTest(t)

	enqueueAndDeadLetter(t, store, actor.EnqueueParams{
		ID:              "msg-requeue",
		MailboxID:       "actor-1",
		MessageType:     "test.Message",
		Payload:         []byte{9, 8, 7},
		PromiseID:       "promise-1",
		CallbackActorID: "caller-1",
		CorrelationID:   "corr-1",
		Priority:        3,
		AvailableAt:     time.Now().Add(-time.Minute),
		MaxAttempts:     5,
	})

	// Mirror the retry-exhaustion path, which records the original ID in
	// processed_messages before dead-lettering.
	err := store.MarkProcessed(ctx, "msg-requeue", "actor-1", time.Hour)
	require.NoError(t, err)

	newID, err := store.RequeueDeadLetter(ctx, "msg-requeue")
	require.NoError(t, err)
	require.NotEqual(t, "msg-requeue", newID)

	// The dead letter is gone.
	dl, err := store.GetDeadLetter(ctx, "msg-requeue")
	require.NoError(t, err)
	require.Nil(t, dl)

	// The requeued message is leasable now, with routing preserved and
	// the retry counter starting over.
	leased, err := store.LeaseNextMessage(
		ctx, "actor-1", "token-rq", 30*time.Second,
	)
	require.NoError(t, err)
	require.NotNil(t, leased)

	require.Equal(t, newID, leased.ID)
	require.Equal(t, "test.Message", leased.MessageType)
	require.Equal(t, []byte{9, 8, 7}, leased.Payload)
	require.Equal(t, "promise-1", leased.PromiseID)
	require.Equal(t, "caller-1", leased.CallbackActorID)
	require.Equal(t, "corr-1", leased.CorrelationID)
	require.Equal(t, 3, leased.Priority)
	require.Equal(t, 5, leased.MaxAttempts)
	require.Equal(t, 1, leased.Attempts)

	// The fresh ID is not shadowed by the original's dedup record.
	processed, err := store.IsProcessed(ctx, newID)
	require.NoError(t, err)
	require.False(t, processed)

	processed, err = store.IsProcessed(ctx, "msg-requeue")
	require.NoError(t, err)
	require.True(t, processed)
}

// TestDeadLetterRequeuePreservesCorrelationKey round-trips a keyed message
// through dead-letter and requeue and asserts the key survives, by
// dead-lettering the requeued row again and reading the key back.
func TestDeadLetterRequeuePreservesCorrelationKey(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newActorDeliveryStoreForTest(t)

	enqueueAndDeadLetter(t, store, actor.EnqueueParams{
		ID:             "msg-keyed",
		MailboxID:      "actor-1",
		MessageType:    "test.Message",
		Payload:        []byte{1},
		CorrelationKey: "lane-b",
		AvailableAt:    time.Now().Add(-time.Minute),
		MaxAttempts:    5,
	})

	newID, err := store.RequeueDeadLetter(ctx, "msg-keyed")
	require.NoError(t, err)

	// Round-trip the requeued row back into the dead-letter table; the
	// projection reads the mailbox row, so the key it reports is the key
	// the requeue wrote.
	err = store.MoveToDeadLetter(ctx, newID, "round trip")
	require.NoError(t, err)

	dl, err := store.GetDeadLetter(ctx, newID)
	require.NoError(t, err)
	require.NotNil(t, dl)
	require.Equal(t, "lane-b", dl.CorrelationKey)
}

// TestDeadLetterRequeueNotFound asserts requeue of an unknown ID returns the
// typed not-found error rather than a silent no-op.
func TestDeadLetterRequeueNotFound(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newActorDeliveryStoreForTest(t)

	_, err := store.RequeueDeadLetter(ctx, "no-such-id")
	require.ErrorIs(t, err, actor.ErrDeadLetterNotFound)
}

// TestDeadLetterRequeueWake asserts that a successful requeue fires the
// registered mailbox wake for exactly the target mailbox, so a resident
// consumer picks the requeued message up without waiting for its poll.
func TestDeadLetterRequeueWake(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newActorDeliveryStoreForTest(t)

	enqueueAndDeadLetter(t, store, actor.EnqueueParams{
		ID:          "msg-wake",
		MailboxID:   "actor-1",
		MessageType: "test.Message",
		Payload:     []byte{1},
		AvailableAt: time.Now().Add(-time.Minute),
		MaxAttempts: 5,
	})

	woken := make(chan struct{}, 1)
	cancel := store.RegisterMailboxWake("actor-1", func() {
		select {
		case woken <- struct{}{}:
		default:
		}
	})
	defer cancel()

	otherWoken := make(chan struct{}, 1)
	cancelOther := store.RegisterMailboxWake("actor-2", func() {
		select {
		case otherWoken <- struct{}{}:
		default:
		}
	})
	defer cancelOther()

	_, err := store.RequeueDeadLetter(ctx, "msg-wake")
	require.NoError(t, err)

	select {
	case <-woken:
	case <-time.After(time.Second):
		t.Fatal("expected mailbox wake after requeue")
	}

	select {
	case <-otherWoken:
		t.Fatal("unrelated mailbox must not be woken")

	default:
	}
}

// TestDeadLetterListAndCount exercises the global enumeration surface: the
// paginated global list, the incremental since-scan, and the count queries.
func TestDeadLetterListAndCount(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newActorDeliveryStoreForTest(t)

	// Three dead letters for actor-1 and one for actor-2, with the clock
	// advanced between each so created_at strictly increases.
	ids := []struct {
		id      string
		mailbox string
	}{
		{
			"dl-1",
			"actor-1",
		},
		{
			"dl-2",
			"actor-1",
		},
		{
			"dl-3",
			"actor-2",
		},
		{
			"dl-4",
			"actor-1",
		},
	}

	var sinceCutoff time.Time
	for i, m := range ids {
		if i == 2 {
			// Everything from dl-3 on is "recent" for the
			// since-scan below.
			sinceCutoff = store.clock.Now()
		}

		enqueueAndDeadLetter(t, store, actor.EnqueueParams{
			ID:          m.id,
			MailboxID:   m.mailbox,
			MessageType: "test.Message",
			Payload:     []byte{byte(i)},
			AvailableAt: time.Now().Add(-time.Minute),
			MaxAttempts: 1,
		})

		store.clock.SetTime(store.clock.Now().Add(time.Minute))
	}

	// Global count and per-actor tallies.
	count, err := store.CountDeadLetters(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 4, count)

	perActor, err := store.CountDeadLettersByActor(ctx)
	require.NoError(t, err)
	expectedCounts := []actor.DeadLetterCount{
		{
			ActorID: "actor-1",
			Count:   3,
		},
		{
			ActorID: "actor-2",
			Count:   1,
		},
	}
	require.Equal(t, expectedCounts, perActor)

	// Global list is newest-first with working pagination.
	page1, err := store.ListAllDeadLetters(ctx, 3, 0)
	require.NoError(t, err)
	require.Len(t, page1, 3)
	require.Equal(t, "dl-4", page1[0].ID)
	require.Equal(t, "dl-3", page1[1].ID)
	require.Equal(t, "dl-2", page1[2].ID)

	page2, err := store.ListAllDeadLetters(ctx, 3, 3)
	require.NoError(t, err)
	require.Len(t, page2, 1)
	require.Equal(t, "dl-1", page2[0].ID)

	// The since-scan returns only entries strictly after the (cutoff, "")
	// cursor, oldest first. An empty afterID admits every ID at the
	// cutoff second itself.
	recent, err := store.ListDeadLettersSince(ctx, sinceCutoff, "", 10)
	require.NoError(t, err)
	require.Len(t, recent, 2)
	require.Equal(t, "dl-3", recent[0].ID)
	require.Equal(t, "dl-4", recent[1].ID)
}

// TestDeadLetterSinceKeysetPagination asserts the since-scan pages through
// entries sharing one created_at second via the (created_at, id) keyset:
// continuing from the last row of a page returns the remainder rather than
// the same page again.
func TestDeadLetterSinceKeysetPagination(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newActorDeliveryStoreForTest(t)

	// Three entries, all dead-lettered within the same clock second.
	for i, id := range []string{"same-a", "same-b", "same-c"} {
		enqueueAndDeadLetter(t, store, actor.EnqueueParams{
			ID:          id,
			MailboxID:   "actor-1",
			MessageType: "test.Message",
			Payload:     []byte{byte(i)},
			AvailableAt: time.Now().Add(-time.Minute),
			MaxAttempts: 1,
		})
	}

	since := store.clock.Now().Add(-time.Minute)

	page1, err := store.ListDeadLettersSince(ctx, since, "", 2)
	require.NoError(t, err)
	require.Len(t, page1, 2)
	require.Equal(t, "same-a", page1[0].ID)
	require.Equal(t, "same-b", page1[1].ID)

	// Continue strictly after the last row of page one.
	last := page1[len(page1)-1]
	page2, err := store.ListDeadLettersSince(
		ctx, last.CreatedAt, last.ID, 2,
	)
	require.NoError(t, err)
	require.Len(t, page2, 1)
	require.Equal(t, "same-c", page2[0].ID)

	// Continuing from the final row yields nothing.
	last = page2[0]
	page3, err := store.ListDeadLettersSince(
		ctx, last.CreatedAt, last.ID, 2,
	)
	require.NoError(t, err)
	require.Empty(t, page3)
}

// TestDeadLetterPurge asserts the retention sweep deletes only entries older
// than the threshold and reports how many rows it removed.
func TestDeadLetterPurge(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newActorDeliveryStoreForTest(t)

	for i, id := range []string{"old-1", "old-2", "new-1"} {
		enqueueAndDeadLetter(t, store, actor.EnqueueParams{
			ID:          id,
			MailboxID:   "actor-1",
			MessageType: "test.Message",
			Payload:     []byte{byte(i)},
			AvailableAt: time.Now().Add(-time.Minute),
			MaxAttempts: 1,
		})

		store.clock.SetTime(store.clock.Now().Add(time.Hour))
	}

	// The clock advanced an hour after each entry; purge everything
	// created more than 90 minutes before "now", which covers old-1 and
	// old-2 but not new-1.
	cutoff := store.clock.Now().Add(-90 * time.Minute)

	removed, err := store.PurgeDeadLetters(ctx, cutoff)
	require.NoError(t, err)
	require.EqualValues(t, 2, removed)

	count, err := store.CountDeadLetters(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, count)

	remaining, err := store.ListAllDeadLetters(ctx, 10, 0)
	require.NoError(t, err)
	require.Len(t, remaining, 1)
	require.Equal(t, "new-1", remaining[0].ID)

	// A second sweep with the same cutoff removes nothing.
	removed, err = store.PurgeDeadLetters(ctx, cutoff)
	require.NoError(t, err)
	require.Zero(t, removed)
}
