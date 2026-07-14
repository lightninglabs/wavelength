package actordelivery

import (
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// newTestStore opens a fresh in-memory SQLite-backed delivery store with a
// caller-supplied test clock so reorder behaviour can be exercised
// deterministically. Returns the store and the same clock so the caller
// can advance time between operations.
func newTestStore(t *testing.T) (actor.TxAwareDeliveryStore, *clock.TestClock) {
	t.Helper()

	rawDB := newSQLiteDB(t)
	require.NoError(t, RunMigrations(rawDB, sqlc.BackendTypeSqlite))

	t0 := time.Unix(1_700_000_000, 0)
	clk := clock.NewTestClock(t0)

	store, err := NewTxAwareDeliveryStoreFromDB(
		rawDB, sqlc.BackendTypeSqlite, clk, btclog.Disabled,
	)
	require.NoError(t, err)

	return store, clk
}

// enqueue is a tiny wrapper that stamps the test clock's current time as
// the message's available_at so messages are immediately claim-eligible
// unless a correlation-key invariant says otherwise.
func enqueue(t *testing.T, store actor.TxAwareDeliveryStore,
	clk *clock.TestClock, id, mailbox, key string) {

	t.Helper()

	require.NoError(
		t,
		store.EnqueueMessage(
			t.Context(), actor.EnqueueParams{
				ID:             id,
				MailboxID:      mailbox,
				MessageType:    "test.Msg",
				Payload:        []byte(id),
				Priority:       0,
				AvailableAt:    clk.Now(),
				MaxAttempts:    3,
				CorrelationKey: key,
			},
		),
	)
}

// TestPerKeyFIFOBlocksOvertakeOnNack is the canonical reproduction of the
// reorder bug the migration fixes. It enqueues two keyed messages, nacks
// the first with a delay that pushes its available_at into the future,
// and verifies the second message is NOT returned by the claim path even
// though its available_at is smaller — the anti-join keeps strict FIFO
// per key.
func TestPerKeyFIFOBlocksOvertakeOnNack(t *testing.T) {
	t.Parallel()

	store, clk := newTestStore(t)
	ctx := t.Context()

	const mailbox = "actor-A"
	const key = "alice/round-1"

	// msg1 enqueued at T=0, immediately eligible.
	enqueue(t, store, clk, "msg-1", mailbox, key)

	// Lease msg1, simulate transient send failure: nack with a long delay
	// so msg1's available_at is now strictly in the future.
	leased, err := store.LeaseNextMessage(
		ctx, mailbox, "lease-1", time.Minute,
	)
	require.NoError(t, err)
	require.NotNil(t, leased)
	require.Equal(t, "msg-1", leased.ID)

	rows, err := store.NackMessage(ctx, "msg-1", "lease-1", 5*time.Second)
	require.NoError(t, err)
	require.EqualValues(t, 1, rows)

	// Advance to T+1s and enqueue msg2 (same key). Its available_at is
	// now smaller than msg1's pushed-out available_at — under the old
	// ORDER BY available_at ASC rule the claim would return msg2.
	clk.SetTime(clk.Now().Add(1 * time.Second))
	enqueue(t, store, clk, "msg-2", mailbox, key)

	// Claim at T+1s. msg1 is still in backoff (available_at=T+5s),
	// msg2 has available_at=T+1s. With per-key FIFO, msg2 cannot
	// overtake msg1 — the claim must return nothing.
	leased2, err := store.LeaseNextMessage(
		ctx, mailbox, "lease-2", time.Minute,
	)
	require.NoError(t, err)
	require.Nil(
		t, leased2,
		"per-key FIFO must hold msg-2 behind in-backoff msg-1",
	)

	// Advance to T+5s. msg1 is now available; claim must return msg1
	// first.
	clk.SetTime(clk.Now().Add(4 * time.Second))
	leased3, err := store.LeaseNextMessage(
		ctx, mailbox, "lease-3", time.Minute,
	)
	require.NoError(t, err)
	require.NotNil(t, leased3)
	require.Equal(
		t, "msg-1", leased3.ID,
		"head-of-key must drain before any later same-key message",
	)

	// Ack msg1; now msg2 becomes the head of the key and is claimable.
	ackRows, err := store.AckMessage(ctx, "msg-1", "lease-3")
	require.NoError(t, err)
	require.EqualValues(t, 1, ackRows)

	leased4, err := store.LeaseNextMessage(
		ctx, mailbox, "lease-4", time.Minute,
	)
	require.NoError(t, err)
	require.NotNil(t, leased4)
	require.Equal(t, "msg-2", leased4.ID)
}

// TestPerKeyFIFOCrossKeyIndependence confirms that a message in backoff
// for key K1 does not block a different keyed message K2 in the same
// mailbox. Each key is its own FIFO lane.
func TestPerKeyFIFOCrossKeyIndependence(t *testing.T) {
	t.Parallel()

	store, clk := newTestStore(t)
	ctx := t.Context()

	const mailbox = "actor-A"

	// K1 message that we'll force into backoff.
	enqueue(t, store, clk, "k1-msg-1", mailbox, "alice/round-1")
	leased, err := store.LeaseNextMessage(
		ctx, mailbox, "lease-1", time.Minute,
	)
	require.NoError(t, err)
	require.Equal(t, "k1-msg-1", leased.ID)
	_, err = store.NackMessage(ctx, "k1-msg-1", "lease-1", 10*time.Second)
	require.NoError(t, err)

	// K2 message enqueued while K1 is in backoff.
	clk.SetTime(clk.Now().Add(1 * time.Second))
	enqueue(t, store, clk, "k2-msg-1", mailbox, "bob/round-2")

	// K2 must be claimable independently of K1's backoff.
	leased2, err := store.LeaseNextMessage(
		ctx, mailbox, "lease-2", time.Minute,
	)
	require.NoError(t, err)
	require.NotNil(
		t, leased2,
		"different correlation keys must not block each other",
	)
	require.Equal(t, "k2-msg-1", leased2.ID)
}

// TestPerKeyFIFOUnkeyedUnaffected confirms that unkeyed messages (empty
// correlation key) still follow the global available_at order and are
// not blocked by, nor block, keyed messages.
func TestPerKeyFIFOUnkeyedUnaffected(t *testing.T) {
	t.Parallel()

	store, clk := newTestStore(t)
	ctx := t.Context()

	const mailbox = "actor-A"

	// Keyed msg in backoff.
	enqueue(t, store, clk, "k-msg-1", mailbox, "alice/round-1")
	leased, err := store.LeaseNextMessage(
		ctx, mailbox, "lease-1", time.Minute,
	)
	require.NoError(t, err)
	require.Equal(t, "k-msg-1", leased.ID)
	_, err = store.NackMessage(ctx, "k-msg-1", "lease-1", 10*time.Second)
	require.NoError(t, err)

	// Unkeyed msg enqueued while keyed msg is in backoff.
	clk.SetTime(clk.Now().Add(1 * time.Second))
	enqueue(t, store, clk, "unkeyed-1", mailbox, "")

	// Unkeyed message uses global available_at order, unaffected by the
	// keyed lane.
	leased2, err := store.LeaseNextMessage(
		ctx, mailbox, "lease-2", time.Minute,
	)
	require.NoError(t, err)
	require.NotNil(
		t, leased2,
		"unkeyed message must not be blocked by an unrelated key",
	)
	require.Equal(t, "unkeyed-1", leased2.ID)
}

// TestPerKeyFIFOAckUnblocksKey confirms that once the head-of-line
// message for a key is acked (the normal happy path), the next same-key
// message becomes claimable immediately.
func TestPerKeyFIFOAckUnblocksKey(t *testing.T) {
	t.Parallel()

	store, clk := newTestStore(t)
	ctx := t.Context()

	const mailbox = "actor-A"
	const key = "alice/round-1"

	enqueue(t, store, clk, "msg-1", mailbox, key)
	clk.SetTime(clk.Now().Add(1 * time.Second))
	enqueue(t, store, clk, "msg-2", mailbox, key)

	// First claim returns msg-1.
	leased, err := store.LeaseNextMessage(
		ctx, mailbox, "lease-1", time.Minute,
	)
	require.NoError(t, err)
	require.Equal(t, "msg-1", leased.ID)

	// While msg-1 is leased, msg-2 must NOT be claimable because
	// msg-1 is still head of the key.
	leased2, err := store.LeaseNextMessage(
		ctx, mailbox, "lease-2", time.Minute,
	)
	require.NoError(t, err)
	require.Nil(
		t, leased2,
		"msg-2 must wait while msg-1 holds the head of the key",
	)

	// Ack msg-1; the key is now drained, msg-2 becomes head.
	_, err = store.AckMessage(ctx, "msg-1", "lease-1")
	require.NoError(t, err)

	leased3, err := store.LeaseNextMessage(
		ctx, mailbox, "lease-3", time.Minute,
	)
	require.NoError(t, err)
	require.NotNil(t, leased3)
	require.Equal(t, "msg-2", leased3.ID)
}

// enqueueWithMaxAttempts is a variant of enqueue that lets the caller
// pick a max_attempts budget. Useful for exercising the
// attempts-exhausted predecessor path without sitting through the full
// default retry budget.
func enqueueWithMaxAttempts(t *testing.T, store actor.TxAwareDeliveryStore,
	clk *clock.TestClock, id, mailbox, key string, maxAttempts int) {

	t.Helper()

	require.NoError(
		t,
		store.EnqueueMessage(
			t.Context(), actor.EnqueueParams{
				ID:             id,
				MailboxID:      mailbox,
				MessageType:    "test.Msg",
				Payload:        []byte(id),
				Priority:       0,
				AvailableAt:    clk.Now(),
				MaxAttempts:    maxAttempts,
				CorrelationKey: key,
			},
		),
	)
}

// TestPerKeyFIFOExhaustedPredecessorDoesNotBlockSuccessor closes a
// failure mode that the original anti-join left open: a same-key row
// whose attempts have hit max_attempts but which has not yet been
// physically deleted (e.g. a crash window between
// MoveMailboxToDeadLetter and DeleteMailboxMessage in
// handlePoisonMessage) used to permanently stall every later same-key
// message via the anti-join. The fix requires the anti-join predicate
// to match the outer eligibility predicate (m2.attempts <
// m2.max_attempts) so exhausted rows are skipped over rather than
// treated as live heads-of-line.
func TestPerKeyFIFOExhaustedPredecessorDoesNotBlockSuccessor(t *testing.T) {
	t.Parallel()

	store, clk := newTestStore(t)
	ctx := t.Context()

	const mailbox = "actor-A"
	const key = "alice/round-1"

	// Drive msg-1 to attempts == max_attempts without deleting it. Two
	// lease/nack cycles exhaust a budget of 2: each lease bumps attempts
	// by one before the body runs, so after the second nack the row's
	// attempts equal max_attempts and the outer SELECT will refuse to
	// re-lease it.
	enqueueWithMaxAttempts(t, store, clk, "msg-1", mailbox, key, 2)

	leased1, err := store.LeaseNextMessage(
		ctx, mailbox, "lease-1", time.Minute,
	)
	require.NoError(t, err)
	require.NotNil(t, leased1)
	require.Equal(t, "msg-1", leased1.ID)
	_, err = store.NackMessage(ctx, "msg-1", "lease-1", 1*time.Second)
	require.NoError(t, err)

	clk.SetTime(clk.Now().Add(2 * time.Second))

	leased2, err := store.LeaseNextMessage(
		ctx, mailbox, "lease-2", time.Minute,
	)
	require.NoError(t, err)
	require.NotNil(t, leased2)
	require.Equal(t, "msg-1", leased2.ID)
	_, err = store.NackMessage(ctx, "msg-1", "lease-2", 1*time.Second)
	require.NoError(t, err)

	clk.SetTime(clk.Now().Add(2 * time.Second))

	// Sanity check: msg-1 is exhausted (attempts == max_attempts) and the
	// outer eligibility predicate keeps it out of the candidate set, so
	// claiming it as the head-of-key returns nothing.
	exhausted, err := store.LeaseNextMessage(
		ctx, mailbox, "lease-sanity", time.Minute,
	)
	require.NoError(t, err)
	require.Nil(
		t, exhausted, "exhausted predecessor must not be claimable",
	)

	// Enqueue msg-2 with the same key. msg-1 is still physically present
	// in the table (it has not been moved to dead letters or deleted).
	// Before the fix, the anti-join would still see msg-1 as an earlier
	// same-key row and refuse to surface msg-2 — head-of-line blocking
	// would be permanent. After the fix, the anti-join filters out
	// exhausted rows and msg-2 becomes the new head of the key.
	enqueueWithMaxAttempts(t, store, clk, "msg-2", mailbox, key, 3)

	leased3, err := store.LeaseNextMessage(
		ctx, mailbox, "lease-3", time.Minute,
	)
	require.NoError(t, err)
	require.NotNil(
		t, leased3, "exhausted same-key predecessor must not "+
			"permanently block successor",
	)
	require.Equal(t, "msg-2", leased3.ID)
}

// TestPerKeyFIFOActiveLeasedPredecessorBlocksSuccessor pins the invariant
// that an actively leased same-key predecessor (lease_until in the future,
// retry budget remaining) blocks any later same-key message even though
// the predecessor isn't in backoff. The anti-join deliberately does NOT
// filter on lease_until: a row currently being processed by another
// worker is a live head-of-line and successors must wait. The original
// reorder fix only exercised the nack-backoff branch; this test pins the
// actively-leased branch so a future loosening of the anti-join (e.g.
// adding `AND m2.lease_until IS NULL OR m2.lease_until < now`) would
// surface as a test failure rather than a silent regression.
func TestPerKeyFIFOActiveLeasedPredecessorBlocksSuccessor(t *testing.T) {
	t.Parallel()

	store, clk := newTestStore(t)
	ctx := t.Context()

	const mailbox = "actor-A"
	const key = "alice/round-1"

	// Lease msg-1 and hold the lease (do not ack, do not nack). The
	// row's lease_until is now in the future and attempts has been
	// incremented, modelling a worker that is actively processing the
	// message.
	enqueue(t, store, clk, "msg-1", mailbox, key)

	leased, err := store.LeaseNextMessage(
		ctx, mailbox, "lease-1", time.Minute,
	)
	require.NoError(t, err)
	require.NotNil(t, leased)
	require.Equal(t, "msg-1", leased.ID)

	// Enqueue msg-2 for the same key while msg-1's lease is still
	// live. msg-2 is fully eligible by itself (available_at = now,
	// attempts = 0), but the anti-join must keep it behind msg-1.
	clk.SetTime(clk.Now().Add(100 * time.Millisecond))
	enqueue(t, store, clk, "msg-2", mailbox, key)

	leased2, err := store.LeaseNextMessage(
		ctx, mailbox, "lease-2", time.Minute,
	)
	require.NoError(t, err)
	require.Nil(
		t, leased2,
		"actively leased same-key predecessor must block successor",
	)

	// Ack msg-1; msg-2 now becomes the head of the key and is
	// claimable. This pins the unblock half of the contract: the
	// head must drain (ack OR exhaust) before successors run.
	ackRows, err := store.AckMessage(ctx, "msg-1", "lease-1")
	require.NoError(t, err)
	require.EqualValues(t, 1, ackRows)

	leased3, err := store.LeaseNextMessage(
		ctx, mailbox, "lease-3", time.Minute,
	)
	require.NoError(t, err)
	require.NotNil(t, leased3)
	require.Equal(t, "msg-2", leased3.ID)
}

// TestPerKeyFIFOMailboxIsolation confirms that the per-key FIFO scope is
// per-mailbox: the same key in two different mailboxes does not create
// cross-mailbox head-of-line dependencies.
func TestPerKeyFIFOMailboxIsolation(t *testing.T) {
	t.Parallel()

	store, clk := newTestStore(t)
	ctx := t.Context()

	// Same key in two different mailboxes.
	enqueue(t, store, clk, "a-msg-1", "mailbox-A", "shared-key")
	enqueue(t, store, clk, "b-msg-1", "mailbox-B", "shared-key")

	// Force mailbox-A's message into backoff.
	leasedA, err := store.LeaseNextMessage(
		ctx, "mailbox-A", "lease-1", time.Minute,
	)
	require.NoError(t, err)
	require.Equal(t, "a-msg-1", leasedA.ID)
	_, err = store.NackMessage(ctx, "a-msg-1", "lease-1", 10*time.Second)
	require.NoError(t, err)

	// Mailbox-B's message must be claimable independent of mailbox-A.
	leasedB, err := store.LeaseNextMessage(
		ctx, "mailbox-B", "lease-2", time.Minute,
	)
	require.NoError(t, err)
	require.NotNil(t, leasedB)
	require.Equal(
		t, "b-msg-1", leasedB.ID,
		"per-key FIFO must be scoped per mailbox_id",
	)
}
