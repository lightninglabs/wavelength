package actordelivery

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/db"
	adsqlc "github.com/lightninglabs/wavelength/db/actordelivery/sqlc"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// testActorDeliveryStore holds the store and clock for testing.
type testActorDeliveryStore struct {
	*Store
	clock *clock.TestClock
}

// newActorDeliveryStoreForTest creates a new Store using the
// transaction executor pattern for testing. Returns the store and a test clock
// that can be manipulated to advance time.
func newActorDeliveryStoreForTest(t *testing.T) *testActorDeliveryStore {
	testDB := db.NewTestDB(t)
	actorQueries := adsqlc.New(testDB.DB)

	actorDB := db.NewTransactionExecutor(
		testDB.BaseDB,
		func(tx *sql.Tx) ActorDeliveryQueries {
			return actorQueries.WithTx(tx)
		},
		btclog.Disabled,
	)

	testClock := clock.NewTestClock(time.Now())

	return &testActorDeliveryStore{
		Store: NewStore(actorDB, testClock),
		clock: testClock,
	}
}

func newTxAwareActorDeliveryStoreForTest(
	t *testing.T) *TxAwareActorDeliveryStore {

	testDB := db.NewTestDB(t)
	actorQueries := adsqlc.New(testDB.DB)

	actorDB := db.NewTransactionExecutor(
		testDB.BaseDB,
		func(tx *sql.Tx) ActorDeliveryQueries {
			return actorQueries.WithTx(tx)
		},
		btclog.Disabled,
	)

	return NewTxAwareActorDeliveryStore(
		actorDB, testDB.BaseDB,
		clock.NewTestClock(
			time.Now(),
		),
	)
}

// generateTestID generates a random 16-byte hex-encoded ID for testing.
func generateTestID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)

	return hex.EncodeToString(b)
}

// TestActorDeliveryStoreEnqueueAndLease tests basic enqueue and lease
// operations.
func TestActorDeliveryStoreEnqueueAndLease(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newActorDeliveryStoreForTest(t)

	// Enqueue a message.
	err := store.EnqueueMessage(ctx, actor.EnqueueParams{
		ID:          "msg-001",
		MailboxID:   "actor-1",
		MessageType: "test.Message",
		Payload:     []byte{1, 2, 3, 4},
		Priority:    5,
		AvailableAt: time.Now().Add(-time.Minute),
		MaxAttempts: 3,
	})
	require.NoError(t, err)

	// Lease the message.
	leased, err := store.LeaseNextMessage(
		ctx, "actor-1", "token-abc", 30*time.Second,
	)
	require.NoError(t, err)
	require.NotNil(t, leased)

	require.Equal(t, "msg-001", leased.ID)
	require.Equal(t, "actor-1", leased.MailboxID)
	require.Equal(t, "test.Message", leased.MessageType)
	require.Equal(t, []byte{1, 2, 3, 4}, leased.Payload)
	require.Equal(t, 5, leased.Priority)
	require.Equal(t, "token-abc", leased.LeaseToken)
	require.Equal(t, 1, leased.Attempts)
	require.Equal(t, 3, leased.MaxAttempts)

	// Trying to lease again should return nil (no available messages).
	leased2, err := store.LeaseNextMessage(
		ctx, "actor-1", "token-xyz", 30*time.Second,
	)
	require.NoError(t, err)
	require.Nil(t, leased2)
}

// TestActorDeliveryStorePeekIsPureRead verifies that PeekNextMessage claims
// the next message without taking a lease: it sets no lease token, no lease
// expiry, and does NOT increment attempts, so the same message is returned on a
// repeated peek. This is the at-least-once invariant of the leaseless path: a
// crash between peek and commit leaves the row untouched for re-peek.
func TestActorDeliveryStorePeekIsPureRead(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newActorDeliveryStoreForTest(t)

	err := store.EnqueueMessage(ctx, actor.EnqueueParams{
		ID:          "msg-peek",
		MailboxID:   "actor-1",
		MessageType: "test.Message",
		Payload:     []byte{1, 2, 3},
		Priority:    5,
		AvailableAt: time.Now().Add(-time.Minute),
		MaxAttempts: 3,
	})
	require.NoError(t, err)

	peeked, err := store.PeekNextMessage(ctx, "actor-1")
	require.NoError(t, err)
	require.NotNil(t, peeked)

	require.Equal(t, "msg-peek", peeked.ID)
	require.Equal(t, []byte{1, 2, 3}, peeked.Payload)

	// No lease was taken and attempts was not incremented.
	require.Empty(t, peeked.LeaseToken)
	require.Equal(t, 0, peeked.Attempts)

	// A second peek returns the same message: peek is a pure read, so the
	// message stays claimable (unlike a lease, which would hide it).
	peeked2, err := store.PeekNextMessage(ctx, "actor-1")
	require.NoError(t, err)
	require.NotNil(t, peeked2)
	require.Equal(t, "msg-peek", peeked2.ID)
	require.Equal(t, 0, peeked2.Attempts)
}

// TestActorDeliveryStorePeekMasksExpiredLease verifies that the leaseless peek
// contract is independent of stale persisted lease metadata. A row can carry an
// expired lease token from a pre-upgrade leased consumer or a crash; once peek
// selects it, the actor-layer delivery must still have an empty token so
// ack/nack route to the by-ID leaseless operations.
func TestActorDeliveryStorePeekMasksExpiredLease(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	ts := newActorDeliveryStoreForTest(t)

	err := ts.EnqueueMessage(ctx, actor.EnqueueParams{
		ID:          "msg-expired-lease-peek",
		MailboxID:   "actor-1",
		MessageType: "test.Message",
		Payload:     []byte{1, 2, 3},
		Priority:    5,
		AvailableAt: ts.clock.Now().Add(-time.Minute),
		MaxAttempts: 3,
	})
	require.NoError(t, err)

	leased, err := ts.LeaseNextMessage(
		ctx, "actor-1", "stale-token", 10*time.Second,
	)
	require.NoError(t, err)
	require.NotNil(t, leased)
	require.Equal(t, "stale-token", leased.LeaseToken)
	require.Equal(t, 1, leased.Attempts)

	ts.clock.SetTime(ts.clock.Now().Add(11 * time.Second))

	peeked, err := ts.PeekNextMessage(ctx, "actor-1")
	require.NoError(t, err)
	require.NotNil(t, peeked)
	require.Equal(t, "msg-expired-lease-peek", peeked.ID)
	require.Empty(t, peeked.LeaseToken)
	require.True(t, peeked.LeaseUntil.IsZero())
	require.Equal(t, 1, peeked.Attempts)

	rows, err := ts.NackMessageByID(ctx, peeked.ID, 0)
	require.NoError(t, err)
	require.Equal(t, int64(1), rows)

	peeked, err = ts.PeekNextMessage(ctx, "actor-1")
	require.NoError(t, err)
	require.NotNil(t, peeked)
	require.Empty(t, peeked.LeaseToken)
	require.True(t, peeked.LeaseUntil.IsZero())
	require.Equal(t, 2, peeked.Attempts)
}

// TestActorDeliveryStoreAckByID verifies the unfenced by-ID ack deletes the
// message regardless of any lease token, then becomes a no-op.
func TestActorDeliveryStoreAckByID(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newActorDeliveryStoreForTest(t)

	err := store.EnqueueMessage(ctx, actor.EnqueueParams{
		ID:          "msg-ackid",
		MailboxID:   "actor-1",
		MessageType: "test.Message",
		Payload:     []byte{1},
		AvailableAt: time.Now().Add(-time.Minute),
		MaxAttempts: 3,
	})
	require.NoError(t, err)

	// Peek (no lease), then ack by ID without any token.
	peeked, err := store.PeekNextMessage(ctx, "actor-1")
	require.NoError(t, err)
	require.NotNil(t, peeked)
	require.Empty(t, peeked.LeaseToken)

	rows, err := store.AckMessageByID(ctx, "msg-ackid")
	require.NoError(t, err)
	require.Equal(t, int64(1), rows)

	// The message is gone; a repeat ack is a no-op.
	rows, err = store.AckMessageByID(ctx, "msg-ackid")
	require.NoError(t, err)
	require.Equal(t, int64(0), rows)

	peeked2, err := store.PeekNextMessage(ctx, "actor-1")
	require.NoError(t, err)
	require.Nil(t, peeked2)
}

// TestActorDeliveryStoreNackByIDIncrementsAttempts verifies the unfenced by-ID
// nack increments attempts (the peek did not) and applies the retry backoff.
// The attempts bump is what preserves dead-lettering on the leaseless path: the
// message climbs to max_attempts and then becomes peek-ineligible.
func TestActorDeliveryStoreNackByIDIncrementsAttempts(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	ts := newActorDeliveryStoreForTest(t)

	err := ts.EnqueueMessage(ctx, actor.EnqueueParams{
		ID:          "msg-nackid",
		MailboxID:   "actor-1",
		MessageType: "test.Message",
		Payload:     []byte{1},
		AvailableAt: ts.clock.Now().Add(-time.Minute),
		MaxAttempts: 2,
	})
	require.NoError(t, err)

	// First peek: attempts is still 0 (peek does not bump).
	peeked, err := ts.PeekNextMessage(ctx, "actor-1")
	require.NoError(t, err)
	require.NotNil(t, peeked)
	require.Equal(t, 0, peeked.Attempts)

	// Nack by ID: attempts -> 1, available_at pushed into the future.
	rows, err := ts.NackMessageByID(ctx, "msg-nackid", 5*time.Minute)
	require.NoError(t, err)
	require.Equal(t, int64(1), rows)

	// Still in backoff, so not yet peekable.
	peeked, err = ts.PeekNextMessage(ctx, "actor-1")
	require.NoError(t, err)
	require.Nil(t, peeked)

	// Advance past the backoff; the message is peekable with attempts == 1.
	ts.clock.SetTime(ts.clock.Now().Add(6 * time.Minute))
	peeked, err = ts.PeekNextMessage(ctx, "actor-1")
	require.NoError(t, err)
	require.NotNil(t, peeked)
	require.Equal(t, 1, peeked.Attempts)

	// Nack again with no backoff: attempts -> 2 == max_attempts, so the
	// message is no longer peek-eligible and would be dead-lettered by the
	// consume path's ShouldDeadLetter check.
	rows, err = ts.NackMessageByID(ctx, "msg-nackid", 0)
	require.NoError(t, err)
	require.Equal(t, int64(1), rows)

	peeked, err = ts.PeekNextMessage(ctx, "actor-1")
	require.NoError(t, err)
	require.Nil(t, peeked)
}

// TestActorDeliveryStoreAck tests message acknowledgement.
func TestActorDeliveryStoreAck(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newActorDeliveryStoreForTest(t)

	// Enqueue and lease a message.
	err := store.EnqueueMessage(ctx, actor.EnqueueParams{
		ID:          "msg-ack",
		MailboxID:   "actor-1",
		MessageType: "test.Message",
		Payload:     []byte{1},
		AvailableAt: time.Now().Add(-time.Minute),
		MaxAttempts: 3,
	})
	require.NoError(t, err)

	leased, err := store.LeaseNextMessage(
		ctx, "actor-1", "token-123", 30*time.Second,
	)
	require.NoError(t, err)
	require.NotNil(t, leased)

	// Ack with correct token should succeed.
	rows, err := store.AckMessage(ctx, "msg-ack", "token-123")
	require.NoError(t, err)
	require.Equal(t, int64(1), rows)

	// Ack with wrong token should fail (0 rows affected).
	rows, err = store.AckMessage(ctx, "msg-ack", "wrong-token")
	require.NoError(t, err)
	require.Equal(t, int64(0), rows)
}

// TestActorDeliveryStoreNack tests message negative acknowledgement.
func TestActorDeliveryStoreNack(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newActorDeliveryStoreForTest(t)

	// Enqueue and lease a message.
	err := store.EnqueueMessage(ctx, actor.EnqueueParams{
		ID:          "msg-nack",
		MailboxID:   "actor-1",
		MessageType: "test.Message",
		Payload:     []byte{1},
		AvailableAt: time.Now().Add(-time.Minute),
		MaxAttempts: 3,
	})
	require.NoError(t, err)

	leased, err := store.LeaseNextMessage(
		ctx, "actor-1", "token-456", 30*time.Second,
	)
	require.NoError(t, err)
	require.NotNil(t, leased)

	// Nack with correct token should succeed.
	rows, err := store.NackMessage(
		ctx, "msg-nack", "token-456", 5*time.Minute,
	)
	require.NoError(t, err)
	require.Equal(t, int64(1), rows)

	// Message should not be available yet (retry delay).
	leased2, err := store.LeaseNextMessage(
		ctx, "actor-1", "token-789", 30*time.Second,
	)
	require.NoError(t, err)
	require.Nil(t, leased2)
}

// TestActorDeliveryStoreExtendLease tests lease extension.
func TestActorDeliveryStoreExtendLease(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newActorDeliveryStoreForTest(t)

	// Enqueue and lease a message.
	err := store.EnqueueMessage(ctx, actor.EnqueueParams{
		ID:          "msg-extend",
		MailboxID:   "actor-1",
		MessageType: "test.Message",
		Payload:     []byte{1},
		AvailableAt: time.Now().Add(-time.Minute),
		MaxAttempts: 3,
	})
	require.NoError(t, err)

	_, err = store.LeaseNextMessage(
		ctx, "actor-1", "token-extend", 30*time.Second,
	)
	require.NoError(t, err)

	// Extend with correct token should succeed.
	rows, err := store.ExtendLease(
		ctx, "msg-extend", "token-extend", 60*time.Second,
	)
	require.NoError(t, err)
	require.Equal(t, int64(1), rows)

	// Extend with wrong token should fail.
	rows, err = store.ExtendLease(
		ctx, "msg-extend", "wrong-token", 60*time.Second,
	)
	require.NoError(t, err)
	require.Equal(t, int64(0), rows)
}

// TestActorDeliveryStoreMoveToDeadLetter tests dead letter functionality.
func TestActorDeliveryStoreMoveToDeadLetter(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newActorDeliveryStoreForTest(t)

	// Enqueue a message.
	err := store.EnqueueMessage(ctx, actor.EnqueueParams{
		ID:          "msg-dead",
		MailboxID:   "actor-1",
		MessageType: "test.Message",
		Payload:     []byte{1, 2, 3},
		AvailableAt: time.Now().Add(-time.Minute),
		MaxAttempts: 1,
	})
	require.NoError(t, err)

	// Move to dead letter.
	err = store.MoveToDeadLetter(ctx, "msg-dead", "max attempts exceeded")
	require.NoError(t, err)

	// Verify it's in dead letters.
	dl, err := store.GetDeadLetter(ctx, "msg-dead")
	require.NoError(t, err)
	require.NotNil(t, dl)

	require.Equal(t, "msg-dead", dl.ID)
	require.Equal(t, "mailbox", dl.Source)
	require.Equal(t, "actor-1", dl.ActorID)
	require.Equal(t, "max attempts exceeded", dl.FailureReason)

	// Original message should be deleted.
	leased, err := store.LeaseNextMessage(
		ctx, "actor-1", "token", 30*time.Second,
	)
	require.NoError(t, err)
	require.Nil(t, leased)
}

// TestActorDeliveryStorePriorityOrdering tests message priority ordering.
func TestActorDeliveryStorePriorityOrdering(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newActorDeliveryStoreForTest(t)

	now := time.Now().Add(-time.Minute)

	// Enqueue messages with different priorities.
	msgs := []struct {
		id       string
		priority int
	}{
		{
			"low",
			1,
		},
		{
			"high",
			10,
		},
		{
			"medium",
			5,
		},
	}

	for _, m := range msgs {
		err := store.EnqueueMessage(ctx, actor.EnqueueParams{
			ID:          m.id,
			MailboxID:   "actor-1",
			MessageType: "test.Message",
			Payload:     []byte{1},
			Priority:    m.priority,
			AvailableAt: now,
			MaxAttempts: 3,
		})
		require.NoError(t, err)
	}

	// Should receive in priority order: high, medium, low.
	expected := []string{"high", "medium", "low"}
	for i, exp := range expected {
		leased, err := store.LeaseNextMessage(
			ctx, "actor-1", "token-"+exp, 30*time.Second,
		)
		require.NoError(t, err, "iteration %d", i)
		require.NotNil(t, leased, "iteration %d", i)
		require.Equal(t, exp, leased.ID, "iteration %d", i)

		// Ack to move to next.
		_, err = store.AckMessage(ctx, leased.ID, "token-"+exp)
		require.NoError(t, err)
	}
}

// TestActorDeliveryStoreAskResult tests Ask result persistence.
func TestActorDeliveryStoreAskResult(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newActorDeliveryStoreForTest(t)

	// Save a successful result.
	err := store.SaveAskResult(ctx, actor.AskResultParams{
		PromiseID:  "promise-123",
		ResultBlob: []byte{1, 2, 3, 4},
		ExpiresAt:  time.Now().Add(time.Hour),
	})
	require.NoError(t, err)

	// Retrieve the result.
	result, err := store.GetAskResult(ctx, "promise-123")
	require.NoError(t, err)
	require.NotNil(t, result)

	require.Equal(t, "promise-123", result.PromiseID)
	require.Equal(t, []byte{1, 2, 3, 4}, result.ResultBlob)
	require.Empty(t, result.ErrorText)

	// Delete the result.
	err = store.DeleteAskResult(ctx, "promise-123")
	require.NoError(t, err)

	// Should be gone.
	result, err = store.GetAskResult(ctx, "promise-123")
	require.NoError(t, err)
	require.Nil(t, result)
}

// TestActorDeliveryStoreAskResultError tests Ask result with error.
func TestActorDeliveryStoreAskResultError(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newActorDeliveryStoreForTest(t)

	// Save an error result.
	err := store.SaveAskResult(ctx, actor.AskResultParams{
		PromiseID: "promise-err",
		ErrorText: "something went wrong",
		ExpiresAt: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)

	// Retrieve the result.
	result, err := store.GetAskResult(ctx, "promise-err")
	require.NoError(t, err)
	require.NotNil(t, result)

	require.Equal(t, "something went wrong", result.ErrorText)
	require.Nil(t, result.ResultBlob)
}

// TestActorDeliveryStoreOutbox tests outbox operations.
func TestActorDeliveryStoreOutbox(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newActorDeliveryStoreForTest(t)

	// Enqueue outbox messages.
	for i := 0; i < 3; i++ {
		err := store.EnqueueOutbox(ctx, actor.OutboxParams{
			ID:            generateTestID(),
			SourceActorID: "round-actor",
			TargetActorID: "wallet-actor",
			MessageType:   "round.SignRequest",
			Payload:       []byte{byte(i)},
			Version:       int64(i),
		})
		require.NoError(t, err)
	}

	// Claim a batch with a claim token and lease duration.
	claimToken := "test-claim-token"
	batch, err := store.ClaimOutboxBatch(ctx, actor.OutboxClaimParams{
		Limit:         10,
		ClaimToken:    claimToken,
		ClaimDuration: 30 * time.Second,
	})
	require.NoError(t, err)
	require.Len(t, batch, 3)

	// Verify claim token is set on returned messages.
	for _, msg := range batch {
		require.Equal(t, claimToken, msg.ClaimToken)
	}

	// Complete one with matching claim token.
	err = store.CompleteOutbox(ctx, batch[0].ID, claimToken)
	require.NoError(t, err)

	// Fail another with matching claim token.
	err = store.FailOutbox(ctx, batch[1].ID, claimToken)
	require.NoError(t, err)
}

// TestTxAwareActorDeliveryStoreOutboxWake verifies that transaction-scoped
// outbox writes wake same-process publishers after the transaction commits.
func TestTxAwareActorDeliveryStoreOutboxWake(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newTxAwareActorDeliveryStoreForTest(t)
	wakeChan := make(chan struct{}, 1)
	store.RegisterOutboxWake(func() {
		select {
		case wakeChan <- struct{}{}:
		default:
		}
	})

	rollbackErr := errors.New("rollback")
	err := store.ExecTx(ctx, false, func(
		txCtx context.Context, txStore actor.DeliveryStore,
	) error {

		err := txStore.EnqueueOutbox(txCtx, actor.OutboxParams{
			ID:            generateTestID(),
			SourceActorID: "round-actor",
			TargetActorID: "wallet-actor",
			MessageType:   "round.SignRequest",
			Payload:       []byte{1},
		})
		require.NoError(t, err)

		return rollbackErr
	})
	require.ErrorIs(t, err, rollbackErr)

	select {
	case <-wakeChan:
		t.Fatal("rollback should not wake outbox publisher")

	case <-time.After(50 * time.Millisecond):
	}

	err = store.ExecTx(ctx, false, func(
		txCtx context.Context, txStore actor.DeliveryStore,
	) error {

		return txStore.EnqueueOutbox(txCtx, actor.OutboxParams{
			ID:            generateTestID(),
			SourceActorID: "round-actor",
			TargetActorID: "wallet-actor",
			MessageType:   "round.SignRequest",
			Payload:       []byte{2},
		})
	})
	require.NoError(t, err)

	select {
	case <-wakeChan:
	case <-time.After(time.Second):
		t.Fatal("commit did not wake outbox publisher")
	}
}

// TestTxAwareActorDeliveryStoreMailboxWake verifies that an enqueue folded into
// a write transaction wakes registered mailbox receive loops after the
// transaction commits, and that a rolled-back enqueue wakes nobody. The wake is
// coarse: every registered mailbox is roused (a non-target does one empty
// re-poll), so the test asserts that two independent registered wakes BOTH fire
// on commit, via both the join path and the tx-scoped store. This pins the
// folded outbox-delivery path: the enqueued row is invisible to the consumer
// until commit, so without a post-commit wake the target would idle until its
// poll interval fired.
func TestTxAwareActorDeliveryStoreMailboxWake(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newTxAwareActorDeliveryStoreForTest(t)

	// wakeA is registered for the mailbox that actually receives the
	// enqueues; wakeB is registered for an unrelated mailbox and must never
	// fire, proving the wake is targeted rather than a coarse broadcast.
	wakeA := make(chan struct{}, 1)
	cancelA := store.RegisterMailboxWake("target-actor", func() {
		select {
		case wakeA <- struct{}{}:
		default:
		}
	})
	defer cancelA()

	wakeB := make(chan struct{}, 1)
	cancelB := store.RegisterMailboxWake("other-actor", func() {
		select {
		case wakeB <- struct{}{}:
		default:
		}
	})
	defer cancelB()

	enqueueParams := func(id string) actor.EnqueueParams {
		return actor.EnqueueParams{
			ID:          id,
			MailboxID:   "target-actor",
			MessageType: "test.Message",
			Payload: []byte{
				1,
			},
			AvailableAt: time.Now().Add(-time.Minute),
			MaxAttempts: 3,
		}
	}

	// Drive the enqueue through the outer store's EnqueueMessage with the
	// ambient tx in context, exactly as the folded outbox-delivery path
	// does: DurableMailbox.Send calls EnqueueMessage on the target actor's
	// (*Store)-backed store, which joins the ambient tx via
	// TransactionExecutor.ExecTx rather than the tx-scoped store. The wake
	// must fire even though the tx-scoped TxActorDeliveryStore is never
	// touched.

	// A rolled-back enqueue must not wake the target: the row never became
	// visible.
	rollbackErr := errors.New("rollback")
	err := store.ExecTx(ctx, false, func(
		txCtx context.Context, _ actor.DeliveryStore,
	) error {

		require.NoError(
			t,
			store.EnqueueMessage(
				txCtx,
				enqueueParams(
					generateTestID(),
				),
			),
		)

		return rollbackErr
	})
	require.ErrorIs(t, err, rollbackErr)

	// A rolled-back enqueue wakes nobody: the row never became visible.
	time.Sleep(50 * time.Millisecond)
	select {
	case <-wakeA:
		t.Fatal("rollback should not wake a mailbox")

	default:
	}
	select {
	case <-wakeB:
		t.Fatal("rollback should not wake a mailbox")

	default:
	}

	// A committed enqueue through the join path wakes only the target
	// mailbox, so A must fire and B (an unrelated mailbox) must not.
	err = store.ExecTx(ctx, false, func(
		txCtx context.Context, _ actor.DeliveryStore,
	) error {

		return store.EnqueueMessage(
			txCtx,
			enqueueParams(
				generateTestID(),
			),
		)
	})
	require.NoError(t, err)

	select {
	case <-wakeA:
	case <-time.After(time.Second):
		t.Fatal("commit did not wake the target mailbox")
	}
	select {
	case <-wakeB:
		t.Fatal("commit woke an unrelated mailbox (not targeted)")

	default:
	}

	// An enqueue through the tx-scoped store must also fire the targeted
	// post-commit wake, so callers that hold the TxActorDeliveryStore
	// directly are covered too: A fires, B stays quiet.
	err = store.ExecTx(ctx, false, func(
		txCtx context.Context, txStore actor.DeliveryStore,
	) error {

		return txStore.EnqueueMessage(
			txCtx,
			enqueueParams(
				generateTestID(),
			),
		)
	})
	require.NoError(t, err)

	select {
	case <-wakeA:
	case <-time.After(time.Second):
		t.Fatal("tx-scoped enqueue did not wake the target mailbox")
	}
	select {
	case <-wakeB:
		t.Fatal("tx-scoped enqueue woke an unrelated mailbox")

	default:
	}
}

// TestStoreMailboxWakeCancelRemovesEntry verifies the targeted mailbox-wake
// registry: registrations under the same mailbox ID coexist under independent
// handles (so a restart reusing a durable mailbox ID never clobbers a
// still-live registration), notifyMailboxWake fires only the wakes for the
// mailboxes it is given, and the cancel returned by RegisterMailboxWake removes
// exactly its own handle, pruning the mailbox's entry when its last wake is
// gone. The cancel-on-Close contract is what bounds map growth to the set of
// live mailboxes rather than the lifetime count of mailbox constructions.
func TestStoreMailboxWakeCancelRemovesEntry(t *testing.T) {
	t.Parallel()

	store := newActorDeliveryStoreForTest(t)

	const mailboxID = "shared-mailbox"

	// Register many wakes under the SAME mailbox ID, mirroring repeated
	// actor restarts that each construct a fresh mailbox reusing one
	// durable ID against the shared store. They must coexist, not clobber.
	var fired atomic.Int64
	const registrations = 25
	cancels := make([]func(), 0, registrations)
	for i := 0; i < registrations; i++ {
		cancels = append(
			cancels, store.RegisterMailboxWake(mailboxID, func() {
				fired.Add(1)
			}),
		)
	}

	// All registrations land under one mailbox-ID entry, each owning its
	// own handle: a single outer key, all handles live.
	store.mailboxWakeMu.Lock()
	outer := len(store.mailboxWakes)
	inner := len(store.mailboxWakes[mailboxID])
	store.mailboxWakeMu.Unlock()
	require.Equal(
		t, 1, outer, "registrations did not share one mailbox key",
	)
	require.Equal(
		t, registrations, inner, "registrations clobbered each other",
	)

	// A wake targeting an unrelated mailbox fires nothing: targeting is
	// strict.
	store.notifyMailboxWake(map[string]struct{}{"other": {}})
	require.Equal(
		t, int64(0), fired.Load(),
		"notifyMailboxWake fired for an untargeted mailbox",
	)

	// A wake targeting this mailbox fires every registered closure for it.
	store.notifyMailboxWake(map[string]struct{}{mailboxID: {}})
	require.Equal(
		t, int64(registrations), fired.Load(),
		"notifyMailboxWake did not fire every registration",
	)

	// Cancelling every registration drains the registry, so stopped
	// mailboxes leave nothing behind (the entry is pruned, not just
	// emptied).
	for _, cancel := range cancels {
		cancel()
	}

	store.mailboxWakeMu.Lock()
	outer = len(store.mailboxWakes)
	store.mailboxWakeMu.Unlock()
	require.Equal(
		t, 0, outer, "cancel left stale closures behind",
	)

	// A wake after cancel fires nothing.
	fired.Store(0)
	store.notifyMailboxWake(map[string]struct{}{mailboxID: {}})
	require.Equal(
		t, int64(0), fired.Load(),
		"notifyMailboxWake fired after cancel",
	)
}

// TestActorDeliveryStoreDeduplication tests deduplication operations.
func TestActorDeliveryStoreDeduplication(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newActorDeliveryStoreForTest(t)

	// Check if unprocessed message is processed.
	processed, err := store.IsProcessed(ctx, "msg-new")
	require.NoError(t, err)
	require.False(t, processed)

	// Mark as processed.
	err = store.MarkProcessed(ctx, "msg-new", "actor-1", 24*time.Hour)
	require.NoError(t, err)

	// Now should be processed.
	processed, err = store.IsProcessed(ctx, "msg-new")
	require.NoError(t, err)
	require.True(t, processed)

	// Marking again should be idempotent (ON CONFLICT DO NOTHING).
	err = store.MarkProcessed(ctx, "msg-new", "actor-1", 24*time.Hour)
	require.NoError(t, err)
}

// TestActorDeliveryStoreCheckpoint tests FSM checkpoint operations.
func TestActorDeliveryStoreCheckpoint(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newActorDeliveryStoreForTest(t)

	// No checkpoint initially.
	cp, err := store.LoadCheckpoint(ctx, "round-actor-1")
	require.NoError(t, err)
	require.Nil(t, cp)

	// Save a checkpoint.
	err = store.SaveCheckpoint(ctx, actor.CheckpointParams{
		ActorID:   "round-actor-1",
		StateType: "AwaitingNonces",
		StateData: []byte{1, 2, 3},
		Version:   1,
	})
	require.NoError(t, err)

	// Load the checkpoint.
	cp, err = store.LoadCheckpoint(ctx, "round-actor-1")
	require.NoError(t, err)
	require.NotNil(t, cp)

	require.Equal(t, "round-actor-1", cp.ActorID)
	require.Equal(t, "AwaitingNonces", cp.StateType)
	require.Equal(t, []byte{1, 2, 3}, cp.StateData)
	require.Equal(t, int64(1), cp.Version)

	// Update the checkpoint.
	err = store.SaveCheckpoint(ctx, actor.CheckpointParams{
		ActorID:   "round-actor-1",
		StateType: "AwaitingSignatures",
		StateData: []byte{4, 5, 6},
		Version:   2,
	})
	require.NoError(t, err)

	// Load updated checkpoint.
	cp, err = store.LoadCheckpoint(ctx, "round-actor-1")
	require.NoError(t, err)
	require.Equal(t, "AwaitingSignatures", cp.StateType)
	require.Equal(t, int64(2), cp.Version)

	// Delete the checkpoint.
	err = store.DeleteCheckpoint(ctx, "round-actor-1")
	require.NoError(t, err)

	// Should be gone.
	cp, err = store.LoadCheckpoint(ctx, "round-actor-1")
	require.NoError(t, err)
	require.Nil(t, cp)
}

// TestActorDeliveryStoreDeadLetterList tests dead letter listing.
func TestActorDeliveryStoreDeadLetterList(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newActorDeliveryStoreForTest(t)

	// Create some messages and move them to dead letters.
	for i := 0; i < 3; i++ {
		id := generateTestID()

		err := store.EnqueueMessage(ctx, actor.EnqueueParams{
			ID:          id,
			MailboxID:   "actor-1",
			MessageType: "test.Message",
			Payload:     []byte{byte(i)},
			AvailableAt: time.Now().Add(-time.Minute),
			MaxAttempts: 1,
		})
		require.NoError(t, err)

		err = store.MoveToDeadLetter(ctx, id, "test failure")
		require.NoError(t, err)
	}

	// List dead letters.
	dls, err := store.ListDeadLetters(ctx, "actor-1", 10)
	require.NoError(t, err)
	require.Len(t, dls, 3)

	// Delete one.
	err = store.DeleteDeadLetter(ctx, dls[0].ID)
	require.NoError(t, err)

	// Should have 2 left.
	dls, err = store.ListDeadLetters(ctx, "actor-1", 10)
	require.NoError(t, err)
	require.Len(t, dls, 2)
}

// TestActorDeliveryStoreExpireLeases tests lease expiration.
func TestActorDeliveryStoreExpireLeases(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	ts := newActorDeliveryStoreForTest(t)

	// Enqueue a message (available in the past).
	err := ts.EnqueueMessage(ctx, actor.EnqueueParams{
		ID:          "msg-expire",
		MailboxID:   "actor-1",
		MessageType: "test.Message",
		Payload:     []byte{1},
		AvailableAt: ts.clock.Now().Add(-time.Hour),
		MaxAttempts: 3,
	})
	require.NoError(t, err)

	// Lease with 10 second duration.
	_, err = ts.LeaseNextMessage(
		ctx, "actor-1", "token-old", 10*time.Second,
	)
	require.NoError(t, err)

	// Advance time by 15 seconds so the lease has expired.
	ts.clock.SetTime(ts.clock.Now().Add(15 * time.Second))

	// Expire leases.
	err = ts.ExpireLeases(ctx)
	require.NoError(t, err)

	// Should be able to lease again.
	leased, err := ts.LeaseNextMessage(
		ctx, "actor-1", "token-new", 30*time.Second,
	)
	require.NoError(t, err)
	require.NotNil(t, leased)
	require.Equal(t, "token-new", leased.LeaseToken)
	require.Equal(t, 2, leased.Attempts) // Incremented on second lease.
}

// TestActorDeliveryStoreMultipleEnqueueLease tests enqueueing and leasing
// multiple messages.
func TestActorDeliveryStoreMultipleEnqueueLease(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newActorDeliveryStoreForTest(t)

	mailboxID := "test-mailbox"

	// Enqueue multiple messages.
	var enqueued []string
	for i := 0; i < 5; i++ {
		id := generateTestID()
		err := store.EnqueueMessage(ctx, actor.EnqueueParams{
			ID:          id,
			MailboxID:   mailboxID,
			MessageType: "test.Message",
			Payload:     []byte{byte(i)},
			Priority:    i,
			AvailableAt: time.Now().Add(-time.Hour),
			MaxAttempts: 10,
		})
		require.NoError(t, err)
		enqueued = append(enqueued, id)
	}

	// Lease and ack all messages.
	leased := 0
	for {
		msg, err := store.LeaseNextMessage(
			ctx, mailboxID, generateTestID(), 30*time.Second,
		)
		require.NoError(t, err)

		if msg == nil {
			break
		}

		leased++
		_, err = store.AckMessage(ctx, msg.ID, msg.LeaseToken)
		require.NoError(t, err)
	}

	// Should have leased exactly what we enqueued.
	require.Equal(t, len(enqueued), leased)
}

// TestActorDeliveryStoreRapidEnqueueLease is a property-based test for
// enqueue/lease operations. The store is created once before rapid.Check
// since NewTestDB requires *testing.T.
func TestActorDeliveryStoreRapidEnqueueLease(t *testing.T) {
	t.Parallel()

	// Create store with outer testing.T since rapid.T doesn't satisfy
	// testing.TB for NewTestDB.
	store := newActorDeliveryStoreForTest(t)
	ctx := t.Context()

	rapid.Check(t, func(rt *rapid.T) {
		// Generate a unique mailbox for this iteration.
		const mailboxIDPattern = `[a-z]{5,10}`
		mailboxID := rapid.StringMatching(mailboxIDPattern).Draw(
			rt, "mailboxID",
		)
		numMessages := rapid.IntRange(1, 5).Draw(rt, "numMessages")

		var enqueued []string
		for i := 0; i < numMessages; i++ {
			const msgIDPattern = `msg-[a-z0-9]{8}`
			id := rapid.StringMatching(msgIDPattern).Draw(
				rt, "msgID",
			)
			payloadStrategy := rapid.SliceOf(rapid.Byte())
			payload := payloadStrategy.Draw(rt, "payload")
			priority := rapid.IntRange(0, 100).Draw(rt, "priority")

			err := store.EnqueueMessage(ctx, actor.EnqueueParams{
				ID:          id,
				MailboxID:   mailboxID,
				MessageType: "test.Message",
				Payload:     payload,
				Priority:    priority,
				AvailableAt: time.Now().Add(-time.Hour),
				MaxAttempts: 10,
			})
			if err == nil {
				enqueued = append(enqueued, id)
			}
		}

		// Lease and ack all messages.
		leased := 0
		for {
			msg, err := store.LeaseNextMessage(
				ctx, mailboxID, generateTestID(),
				30*time.Second,
			)
			require.NoError(t, err)

			if msg == nil {
				break
			}

			leased++
			_, err = store.AckMessage(ctx, msg.ID, msg.LeaseToken)
			require.NoError(t, err)
		}

		// Should have leased exactly what we enqueued.
		require.Equal(t, len(enqueued), leased)
	})
}

// TestActorDeliveryStoreRapidCheckpoint is a property-based test for checkpoint
// operations.
func TestActorDeliveryStoreRapidCheckpoint(t *testing.T) {
	t.Parallel()

	store := newActorDeliveryStoreForTest(t)
	ctx := t.Context()

	rapid.Check(t, func(rt *rapid.T) {
		const actorIDPattern = `actor-[a-z0-9]{6}`
		actorID := rapid.StringMatching(actorIDPattern).Draw(
			rt, "actorID",
		)
		stateType := rapid.StringMatching(
			`[A-Z][a-zA-Z]{5,15}`,
		).Draw(rt, "stateType")
		stateData := rapid.SliceOf(rapid.Byte()).Draw(rt, "stateData")
		version := rapid.Int64Range(1, 1000).Draw(rt, "version")

		// Save checkpoint.
		err := store.SaveCheckpoint(ctx, actor.CheckpointParams{
			ActorID:   actorID,
			StateType: stateType,
			StateData: stateData,
			Version:   version,
		})
		require.NoError(t, err)

		// Load and verify.
		cp, err := store.LoadCheckpoint(ctx, actorID)
		require.NoError(t, err)
		require.NotNil(t, cp)

		require.Equal(t, actorID, cp.ActorID)
		require.Equal(t, stateType, cp.StateType)
		// SQLite returns nil for empty BLOBs, so compare lengths.
		require.Equal(t, len(stateData), len(cp.StateData))
		if len(stateData) > 0 {
			require.Equal(t, stateData, cp.StateData)
		}
		require.Equal(t, version, cp.Version)

		// Delete and verify gone.
		err = store.DeleteCheckpoint(ctx, actorID)
		require.NoError(t, err)

		cp, err = store.LoadCheckpoint(ctx, actorID)
		require.NoError(t, err)
		require.Nil(t, cp)
	})
}
