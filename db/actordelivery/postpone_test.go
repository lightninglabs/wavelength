package actordelivery

import (
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/stretchr/testify/require"
)

// enqueuePostponeTestMsg parks one immediately-available message.
func enqueuePostponeTestMsg(t *testing.T, store *testActorDeliveryStore,
	id string) {

	t.Helper()

	err := store.EnqueueMessage(t.Context(), actor.EnqueueParams{
		ID:          id,
		MailboxID:   "actor-pp",
		MessageType: "test.Message",
		Payload:     []byte{1},
		AvailableAt: store.clock.Now().Add(-time.Minute),
		MaxAttempts: 3,
	})
	require.NoError(t, err)
}

// TestPostponePreservesAttemptsLeased verifies the fenced postpone: the lease
// pre-increments attempts, the postpone decrements it back, so the message's
// retry budget after redelivery is exactly what it was before the claim.
func TestPostponePreservesAttemptsLeased(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newActorDeliveryStoreForTest(t)

	enqueuePostponeTestMsg(t, store, "msg-pp-1")

	leased, err := store.LeaseNextMessage(
		ctx, "actor-pp", "token-pp", 30*time.Second,
	)
	require.NoError(t, err)
	require.NotNil(t, leased)
	require.Equal(t, 1, leased.Attempts)

	// A stale token must not postpone.
	rows, err := store.PostponeMessage(ctx, "msg-pp-1", "wrong-token", 0)
	require.NoError(t, err)
	require.Zero(t, rows)

	rows, err = store.PostponeMessage(ctx, "msg-pp-1", "token-pp", 0)
	require.NoError(t, err)
	require.EqualValues(t, 1, rows)

	// The message is claimable again with its attempts budget restored:
	// the fresh lease's pre-increment lands it back at 1, not 2.
	released, err := store.LeaseNextMessage(
		ctx, "actor-pp", "token-pp-2", 30*time.Second,
	)
	require.NoError(t, err)
	require.NotNil(t, released)
	require.Equal(t, "msg-pp-1", released.ID)
	require.Equal(t, 1, released.Attempts)
}

// TestPostponeByIDLeavesAttemptsUntouched verifies the leaseless postpone:
// the peek never bumped attempts, so the release leaves them exactly as
// stored.
func TestPostponeByIDLeavesAttemptsUntouched(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newActorDeliveryStoreForTest(t)

	enqueuePostponeTestMsg(t, store, "msg-pp-2")

	peeked, err := store.PeekNextMessage(ctx, "actor-pp")
	require.NoError(t, err)
	require.NotNil(t, peeked)
	require.Zero(t, peeked.Attempts)
	require.Empty(t, peeked.LeaseToken)

	rows, err := store.PostponeMessageByID(ctx, "msg-pp-2", time.Minute)
	require.NoError(t, err)
	require.EqualValues(t, 1, rows)

	// Not yet available: the postpone pushed available_at a minute out.
	peeked, err = store.PeekNextMessage(ctx, "actor-pp")
	require.NoError(t, err)
	require.Nil(t, peeked)

	// After the delay elapses the message is re-peeked with attempts
	// still untouched.
	store.clock.SetTime(store.clock.Now().Add(2 * time.Minute))

	peeked, err = store.PeekNextMessage(ctx, "actor-pp")
	require.NoError(t, err)
	require.NotNil(t, peeked)
	require.Equal(t, "msg-pp-2", peeked.ID)
	require.Zero(t, peeked.Attempts)
}

// TestPostponeNeverWrapsAttemptsNegative pins the clamp: a fenced postpone on
// a row whose attempts is already zero (corrupt or hand-edited state) stays
// at zero instead of wrapping negative.
func TestPostponeNeverWrapsAttemptsNegative(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newActorDeliveryStoreForTest(t)

	enqueuePostponeTestMsg(t, store, "msg-pp-3")

	leased, err := store.LeaseNextMessage(
		ctx, "actor-pp", "token-a", 30*time.Second,
	)
	require.NoError(t, err)
	require.NotNil(t, leased)

	// First postpone: 1 -> 0.
	rows, err := store.PostponeMessage(ctx, "msg-pp-3", "token-a", 0)
	require.NoError(t, err)
	require.EqualValues(t, 1, rows)

	// Re-lease (0 -> 1) and postpone twice more through fresh leases; the
	// budget oscillates 1 -> 0 and never dips below.
	for i := range 2 {
		token := generateTestID()

		leased, err = store.LeaseNextMessage(
			ctx, "actor-pp", token, 30*time.Second,
		)
		require.NoError(t, err)
		require.NotNil(t, leased, "iteration %d", i)
		require.Equal(t, 1, leased.Attempts)

		rows, err = store.PostponeMessage(ctx, "msg-pp-3", token, 0)
		require.NoError(t, err)
		require.EqualValues(t, 1, rows)
	}
}
