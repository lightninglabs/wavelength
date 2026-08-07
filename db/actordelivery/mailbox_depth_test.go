package actordelivery

import (
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/stretchr/testify/require"
)

// enqueueDepthTestMsg parks one message in the given mailbox.
func enqueueDepthTestMsg(t *testing.T, store *testActorDeliveryStore,
	mailboxID string) string {

	t.Helper()

	id := generateTestID()
	err := store.EnqueueMessage(t.Context(), actor.EnqueueParams{
		ID:          id,
		MailboxID:   mailboxID,
		MessageType: "test.Message",
		Payload:     []byte{1, 2, 3},
		AvailableAt: store.clock.Now().Add(-time.Minute),
		MaxAttempts: 3,
	})
	require.NoError(t, err)

	return id
}

// TestMailboxDepthCountsBacklog verifies that MailboxDepth reports every row
// parked in a mailbox, that leasing a message does NOT shrink the depth (the
// row is still undelivered backlog until it acks), and that an ack does.
func TestMailboxDepthCountsBacklog(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newActorDeliveryStoreForTest(t)

	// An untouched mailbox has zero depth.
	depth, err := store.MailboxDepth(ctx, "actor-1")
	require.NoError(t, err)
	require.Zero(t, depth)

	for range 3 {
		enqueueDepthTestMsg(t, store, "actor-1")
	}

	depth, err = store.MailboxDepth(ctx, "actor-1")
	require.NoError(t, err)
	require.EqualValues(t, 3, depth)

	// A leased (in-flight) message still counts: it has not been
	// delivered until its ack deletes the row.
	leased, err := store.LeaseNextMessage(
		ctx, "actor-1", "token-1", 30*time.Second,
	)
	require.NoError(t, err)
	require.NotNil(t, leased)

	depth, err = store.MailboxDepth(ctx, "actor-1")
	require.NoError(t, err)
	require.EqualValues(t, 3, depth)

	// Acking deletes the row and the depth drops.
	n, err := store.AckMessage(ctx, leased.ID, "token-1")
	require.NoError(t, err)
	require.EqualValues(t, 1, n)

	depth, err = store.MailboxDepth(ctx, "actor-1")
	require.NoError(t, err)
	require.EqualValues(t, 2, depth)
}

// TestMailboxDepthsListsOnlyBackedUpMailboxes verifies that the grouped depth
// listing reports one entry per mailbox holding messages, and none for
// mailboxes that are empty or fully drained.
func TestMailboxDepthsListsOnlyBackedUpMailboxes(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newActorDeliveryStoreForTest(t)

	// No backlog anywhere: the listing is empty.
	depths, err := store.MailboxDepths(ctx)
	require.NoError(t, err)
	require.Empty(t, depths)

	for range 2 {
		enqueueDepthTestMsg(t, store, "actor-a")
	}
	id := enqueueDepthTestMsg(t, store, "actor-b")

	depths, err = store.MailboxDepths(ctx)
	require.NoError(t, err)

	expected := []actor.MailboxDepthCount{
		{
			MailboxID: "actor-a",
			Depth:     2,
		},
		{
			MailboxID: "actor-b",
			Depth:     1,
		},
	}
	require.Equal(t, expected, depths)

	// Draining actor-b removes it from the listing entirely.
	n, err := store.AckMessageByID(ctx, id)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)

	depths, err = store.MailboxDepths(ctx)
	require.NoError(t, err)

	expected = []actor.MailboxDepthCount{
		{
			MailboxID: "actor-a",
			Depth:     2,
		},
	}
	require.Equal(t, expected, depths)
}
