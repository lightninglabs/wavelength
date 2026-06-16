package actordelivery

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/google/uuid"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/db"
	adsqlc "github.com/lightninglabs/darepo-client/db/actordelivery/sqlc"
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

// orderedUUIDv7Pair returns two UUIDv7 strings that sort in generation order.
// The helper waits for a later millisecond so the ordering is not left to the
// random suffix inside a single timestamp bucket.
func orderedUUIDv7Pair(t *testing.T) (string, string) {
	t.Helper()

	earlierID := uuid.Must(uuid.NewV7()).String()

	var laterID string
	for i := 0; i < 10; i++ {
		time.Sleep(2 * time.Millisecond)

		laterID = uuid.Must(uuid.NewV7()).String()
		if earlierID < laterID {
			return earlierID, laterID
		}
	}

	require.FailNow(t, "expected UUIDv7 ids to sort by generation time")

	return "", ""
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

	leased, err := store.LeaseNextMessage(
		ctx, "actor-1", "token-dead", 30*time.Second,
	)
	require.NoError(t, err)
	require.NotNil(t, leased)

	// Move to dead letter.
	rowsAffected, err := store.MoveToDeadLetter(
		ctx, "msg-dead", leased.LeaseToken, "max attempts exceeded",
	)
	require.NoError(t, err)
	require.Equal(t, int64(1), rowsAffected)

	// Verify it's in dead letters.
	dl, err := store.GetDeadLetter(ctx, "mailbox", "msg-dead")
	require.NoError(t, err)
	require.NotNil(t, dl)

	require.Equal(t, "msg-dead", dl.ID)
	require.Equal(t, "mailbox", dl.Source)
	require.Equal(t, "actor-1", dl.ActorID)
	require.Equal(t, "max attempts exceeded", dl.FailureReason)

	// Original message should be deleted.
	leased, err = store.LeaseNextMessage(
		ctx, "actor-1", "token", 30*time.Second,
	)
	require.NoError(t, err)
	require.Nil(t, leased)
}

// TestActorDeliveryStoreMoveToDeadLetterClaimMismatch surfaces stale lease
// loss instead of silently dead-lettering a message after ownership moved.
func TestActorDeliveryStoreMoveToDeadLetterClaimMismatch(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newActorDeliveryStoreForTest(t)

	err := store.EnqueueMessage(ctx, actor.EnqueueParams{
		ID:          "msg-dead-stale",
		MailboxID:   "actor-1",
		MessageType: "test.Message",
		Payload:     []byte{1, 2, 3},
		AvailableAt: store.clock.Now().Add(-time.Minute),
		MaxAttempts: 2,
	})
	require.NoError(t, err)

	firstLease, err := store.LeaseNextMessage(
		ctx, "actor-1", "token-old", 5*time.Second,
	)
	require.NoError(t, err)
	require.NotNil(t, firstLease)

	store.clock.SetTime(store.clock.Now().Add(6 * time.Second))
	err = store.ExpireLeases(ctx)
	require.NoError(t, err)

	secondLease, err := store.LeaseNextMessage(
		ctx, "actor-1", "token-new", 5*time.Second,
	)
	require.NoError(t, err)
	require.NotNil(t, secondLease)
	require.Equal(t, firstLease.ID, secondLease.ID)

	rowsAffected, err := store.MoveToDeadLetter(
		ctx, firstLease.ID, firstLease.LeaseToken, "stale failure",
	)
	require.NoError(t, err)
	require.Equal(t, int64(0), rowsAffected)

	dl, err := store.GetDeadLetter(ctx, "mailbox", firstLease.ID)
	require.NoError(t, err)
	require.Nil(t, dl)

	rowsAffected, err = store.MoveToDeadLetter(
		ctx, secondLease.ID, secondLease.LeaseToken, "fresh failure",
	)
	require.NoError(t, err)
	require.Equal(t, int64(1), rowsAffected)
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

// TestActorDeliveryStoreSameSecondTieBreak uses UUIDv7 ordering when two rows
// land in the same created_at second. This pins the durable store contract to
// priority, available_at, created_at, and finally id.
func TestActorDeliveryStoreSameSecondTieBreak(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newActorDeliveryStoreForTest(t)

	// Freeze the durable store clock so both rows persist with the same
	// created_at second while still using real UUIDv7 values.
	now := time.Unix(1_700_000_000, 0)
	store.clock.SetTime(now)

	earlierID, laterID := orderedUUIDv7Pair(t)

	availableAt := now.Add(-time.Minute)

	// Insert the later UUID first so the store must choose between UUID
	// order and the SQL tie behavior when created_at is identical.
	err := store.EnqueueMessage(ctx, actor.EnqueueParams{
		ID:          laterID,
		MailboxID:   "actor-1",
		MessageType: "test.Message",
		Payload:     []byte{2},
		Priority:    5,
		AvailableAt: availableAt,
		MaxAttempts: 3,
	})
	require.NoError(t, err)

	err = store.EnqueueMessage(ctx, actor.EnqueueParams{
		ID:          earlierID,
		MailboxID:   "actor-1",
		MessageType: "test.Message",
		Payload:     []byte{1},
		Priority:    5,
		AvailableAt: availableAt,
		MaxAttempts: 3,
	})
	require.NoError(t, err)

	leased, err := store.LeaseNextMessage(
		ctx, "actor-1", "token-first", 30*time.Second,
	)
	require.NoError(t, err)
	require.NotNil(t, leased)

	require.Equal(
		t, earlierID, leased.ID,
		"same-second created_at ties must break by UUIDv7 order",
	)
}

// TestActorDeliveryStoreOutboxSameSecondTieBreak uses UUIDv7 ordering when two
// outbox rows land in the same created_at second. This pins the CDC contract
// to created_at and then id.
func TestActorDeliveryStoreOutboxSameSecondTieBreak(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newActorDeliveryStoreForTest(t)

	now := time.Unix(1_700_000_100, 0)
	store.clock.SetTime(now)

	earlierID, laterID := orderedUUIDv7Pair(t)

	err := store.EnqueueOutbox(ctx, actor.OutboxParams{
		ID:            laterID,
		SourceActorID: "round-actor",
		TargetActorID: "wallet-actor",
		MessageType:   "round.SignRequest",
		Payload:       []byte{2},
		Version:       2,
	})
	require.NoError(t, err)

	err = store.EnqueueOutbox(ctx, actor.OutboxParams{
		ID:            earlierID,
		SourceActorID: "round-actor",
		TargetActorID: "wallet-actor",
		MessageType:   "round.SignRequest",
		Payload:       []byte{1},
		Version:       1,
	})
	require.NoError(t, err)

	batch, err := store.ClaimOutboxBatch(ctx, actor.OutboxClaimParams{
		Limit:         1,
		ClaimToken:    "claim-token",
		ClaimDuration: 30 * time.Second,
	})
	require.NoError(t, err)
	require.Len(t, batch, 1)

	require.Equal(
		t, earlierID, batch[0].ID,
		"same-second created_at ties must break by UUIDv7 order",
	)
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

// TestActorDeliveryStoreAckMessageWithAskResult tests atomic Ask ack/result
// persistence at the store boundary.
func TestActorDeliveryStoreAckMessageWithAskResult(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newActorDeliveryStoreForTest(t)

	err := store.EnqueueMessage(ctx, actor.EnqueueParams{
		ID:          "msg-ask-ack",
		MailboxID:   "actor-1",
		MessageType: "test.Message",
		Payload:     []byte{9, 9, 9},
		PromiseID:   "promise-ask-ack",
		AvailableAt: time.Now().Add(-time.Minute),
		MaxAttempts: 3,
	})
	require.NoError(t, err)

	leased, err := store.LeaseNextMessage(
		ctx, "actor-1", "token-ask-ack", 30*time.Second,
	)
	require.NoError(t, err)
	require.NotNil(t, leased)

	rows, err := store.AckMessageWithAskResult(
		ctx, "msg-ask-ack", "token-ask-ack", actor.AskResultParams{
			PromiseID: "promise-ask-ack",
			ErrorText: "boom",
			ExpiresAt: time.Now().Add(time.Hour),
		},
	)
	require.NoError(t, err)
	require.Equal(t, int64(1), rows)

	leased, err = store.LeaseNextMessage(
		ctx, "actor-1", "token-again", 30*time.Second,
	)
	require.NoError(t, err)
	require.Nil(t, leased)

	result, err := store.GetAskResult(ctx, "promise-ask-ack")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "boom", result.ErrorText)
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
	rowsAffected, err := store.CompleteOutbox(
		ctx, batch[0].ID, claimToken,
	)
	require.NoError(t, err)
	require.Equal(t, int64(1), rowsAffected)

	// Fail another with matching claim token.
	rowsAffected, err = store.FailOutbox(
		ctx, batch[1].ID, claimToken, "test outbox failure",
	)
	require.NoError(t, err)
	require.Equal(t, int64(1), rowsAffected)

	dl, err := store.GetDeadLetter(ctx, "outbox", batch[1].ID)
	require.NoError(t, err)
	require.NotNil(t, dl)
	require.Equal(t, "outbox", dl.Source)
	require.Equal(t, "round-actor", dl.ActorID)
	require.Equal(t, "test outbox failure", dl.FailureReason)
}

// TestActorDeliveryStoreOutboxCompleteClaimMismatch surfaces stale claim loss
// at the store boundary instead of silently reporting success.
func TestActorDeliveryStoreOutboxCompleteClaimMismatch(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newActorDeliveryStoreForTest(t)

	err := store.EnqueueOutbox(ctx, actor.OutboxParams{
		ID:            "outbox-complete-stale",
		SourceActorID: "source-actor",
		TargetActorID: "target-actor",
		MessageType:   "test.Message",
		Payload:       []byte{1},
		Version:       1,
	})
	require.NoError(t, err)

	firstBatch, err := store.ClaimOutboxBatch(ctx, actor.OutboxClaimParams{
		Limit:         1,
		ClaimToken:    "claim-old",
		ClaimDuration: 30 * time.Second,
	})
	require.NoError(t, err)
	require.Len(t, firstBatch, 1)

	store.clock.SetTime(store.clock.Now().Add(31 * time.Second))

	secondBatch, err := store.ClaimOutboxBatch(ctx, actor.OutboxClaimParams{
		Limit:         1,
		ClaimToken:    "claim-new",
		ClaimDuration: 30 * time.Second,
	})
	require.NoError(t, err)
	require.Len(t, secondBatch, 1)
	require.Equal(t, firstBatch[0].ID, secondBatch[0].ID)

	rowsAffected, err := store.CompleteOutbox(
		ctx, firstBatch[0].ID, "claim-old",
	)
	require.NoError(t, err)
	require.Equal(t, int64(0), rowsAffected)

	rowsAffected, err = store.CompleteOutbox(
		ctx, firstBatch[0].ID, "claim-new",
	)
	require.NoError(t, err)
	require.Equal(t, int64(1), rowsAffected)
}

// TestActorDeliveryStoreOutboxFailClaimMismatch surfaces stale claim loss at
// the dead-letter boundary instead of silently reporting success.
func TestActorDeliveryStoreOutboxFailClaimMismatch(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newActorDeliveryStoreForTest(t)

	err := store.EnqueueOutbox(ctx, actor.OutboxParams{
		ID:            "outbox-fail-stale",
		SourceActorID: "source-actor",
		TargetActorID: "target-actor",
		MessageType:   "test.Message",
		Payload:       []byte{1},
		Version:       1,
	})
	require.NoError(t, err)

	firstBatch, err := store.ClaimOutboxBatch(ctx, actor.OutboxClaimParams{
		Limit:         1,
		ClaimToken:    "claim-old",
		ClaimDuration: 30 * time.Second,
	})
	require.NoError(t, err)
	require.Len(t, firstBatch, 1)

	store.clock.SetTime(store.clock.Now().Add(31 * time.Second))

	secondBatch, err := store.ClaimOutboxBatch(ctx, actor.OutboxClaimParams{
		Limit:         1,
		ClaimToken:    "claim-new",
		ClaimDuration: 30 * time.Second,
	})
	require.NoError(t, err)
	require.Len(t, secondBatch, 1)
	require.Equal(t, firstBatch[0].ID, secondBatch[0].ID)

	rowsAffected, err := store.FailOutbox(
		ctx, firstBatch[0].ID, "claim-old", "stale failure",
	)
	require.NoError(t, err)
	require.Equal(t, int64(0), rowsAffected)

	rowsAffected, err = store.FailOutbox(
		ctx, firstBatch[0].ID, "claim-new", "fresh failure",
	)
	require.NoError(t, err)
	require.Equal(t, int64(1), rowsAffected)
}

// TestActorDeliveryStoreOutboxCompleteIsTerminal pins completion as a terminal
// outbox transition under the active claim token.
func TestActorDeliveryStoreOutboxCompleteIsTerminal(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newActorDeliveryStoreForTest(t)

	err := store.EnqueueOutbox(ctx, actor.OutboxParams{
		ID:            "outbox-complete-terminal",
		SourceActorID: "source-actor",
		TargetActorID: "target-actor",
		MessageType:   "test.Message",
		Payload:       []byte{1},
		Version:       1,
	})
	require.NoError(t, err)

	batch, err := store.ClaimOutboxBatch(ctx, actor.OutboxClaimParams{
		Limit:         1,
		ClaimToken:    "claim-token",
		ClaimDuration: 30 * time.Second,
	})
	require.NoError(t, err)
	require.Len(t, batch, 1)

	rowsAffected, err := store.CompleteOutbox(
		ctx, batch[0].ID, "claim-token",
	)
	require.NoError(t, err)
	require.Equal(t, int64(1), rowsAffected)

	rowsAffected, err = store.FailOutbox(
		ctx, batch[0].ID, "claim-token", "terminal failure",
	)
	require.NoError(t, err)
	require.Equal(t, int64(0), rowsAffected)
}

// TestActorDeliveryStoreOutboxDeadLetterIsTerminal pins dead-lettering as a
// terminal outbox transition under the active claim token.
func TestActorDeliveryStoreOutboxDeadLetterIsTerminal(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newActorDeliveryStoreForTest(t)

	err := store.EnqueueOutbox(ctx, actor.OutboxParams{
		ID:            "outbox-dead-terminal",
		SourceActorID: "source-actor",
		TargetActorID: "target-actor",
		MessageType:   "test.Message",
		Payload:       []byte{1},
		Version:       1,
	})
	require.NoError(t, err)

	batch, err := store.ClaimOutboxBatch(ctx, actor.OutboxClaimParams{
		Limit:         1,
		ClaimToken:    "claim-token",
		ClaimDuration: 30 * time.Second,
	})
	require.NoError(t, err)
	require.Len(t, batch, 1)

	rowsAffected, err := store.FailOutbox(
		ctx, batch[0].ID, "claim-token", "terminal failure",
	)
	require.NoError(t, err)
	require.Equal(t, int64(1), rowsAffected)

	rowsAffected, err = store.CompleteOutbox(
		ctx, batch[0].ID, "claim-token",
	)
	require.NoError(t, err)
	require.Equal(t, int64(0), rowsAffected)
}

// TestActorDeliveryStoreOutboxFailCreatesDeadLetter verifies that failing an
// outbox row also populates the durable dead-letter store.
func TestActorDeliveryStoreOutboxFailCreatesDeadLetter(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newActorDeliveryStoreForTest(t)

	err := store.EnqueueOutbox(ctx, actor.OutboxParams{
		ID:            "outbox-dl-visible",
		SourceActorID: "source-actor",
		TargetActorID: "target-actor",
		MessageType:   "test.Message",
		Payload:       []byte{7, 8, 9},
		Version:       1,
	})
	require.NoError(t, err)

	batch, err := store.ClaimOutboxBatch(ctx, actor.OutboxClaimParams{
		Limit:         1,
		ClaimToken:    "claim-token",
		ClaimDuration: 30 * time.Second,
	})
	require.NoError(t, err)
	require.Len(t, batch, 1)

	rowsAffected, err := store.FailOutbox(
		ctx, batch[0].ID, "claim-token", "decode failure",
	)
	require.NoError(t, err)
	require.Equal(t, int64(1), rowsAffected)

	dl, err := store.GetDeadLetter(ctx, "outbox", batch[0].ID)
	require.NoError(t, err)
	require.NotNil(t, dl)
	require.Equal(t, "outbox", dl.Source)
	require.Equal(t, "source-actor", dl.ActorID)
	require.Equal(t, "test.Message", dl.MessageType)
	require.Equal(t, []byte{7, 8, 9}, dl.Payload)
	require.Equal(t, "decode failure", dl.FailureReason)
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

		leased, err := store.LeaseNextMessage(
			ctx, "actor-1", "token-"+id, 30*time.Second,
		)
		require.NoError(t, err)
		require.NotNil(t, leased)

		rowsAffected, err := store.MoveToDeadLetter(
			ctx, id, leased.LeaseToken, "test failure",
		)
		require.NoError(t, err)
		require.Equal(t, int64(1), rowsAffected)
	}

	// List dead letters.
	dls, err := store.ListDeadLetters(ctx, "actor-1", 10)
	require.NoError(t, err)
	require.Len(t, dls, 3)

	// Delete one.
	err = store.DeleteDeadLetter(ctx, dls[0].Source, dls[0].ID)
	require.NoError(t, err)

	// Should have 2 left.
	dls, err = store.ListDeadLetters(ctx, "actor-1", 10)
	require.NoError(t, err)
	require.Len(t, dls, 2)
}

// TestActorDeliveryStoreDeadLetterSourceScopesIdentity verifies mailbox and
// outbox dead letters can coexist under the same propagated message ID.
func TestActorDeliveryStoreDeadLetterSourceScopesIdentity(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newActorDeliveryStoreForTest(t)

	const sharedID = "shared-dead-letter-id"

	err := store.EnqueueMessage(ctx, actor.EnqueueParams{
		ID:          sharedID,
		MailboxID:   "actor-1",
		MessageType: "test.Message",
		Payload:     []byte{1, 2, 3},
		AvailableAt: time.Now().Add(-time.Minute),
		MaxAttempts: 1,
	})
	require.NoError(t, err)

	leased, err := store.LeaseNextMessage(
		ctx, "actor-1", "mailbox-token", 30*time.Second,
	)
	require.NoError(t, err)
	require.NotNil(t, leased)

	rowsAffected, err := store.MoveToDeadLetter(
		ctx, sharedID, leased.LeaseToken, "mailbox failure",
	)
	require.NoError(t, err)
	require.Equal(t, int64(1), rowsAffected)

	err = store.EnqueueOutbox(ctx, actor.OutboxParams{
		ID:            sharedID,
		SourceActorID: "source-actor",
		TargetActorID: "target-actor",
		MessageType:   "test.Message",
		Payload:       []byte{9, 8, 7},
		Version:       1,
	})
	require.NoError(t, err)

	batch, err := store.ClaimOutboxBatch(ctx, actor.OutboxClaimParams{
		Limit:         1,
		ClaimToken:    "outbox-token",
		ClaimDuration: 30 * time.Second,
	})
	require.NoError(t, err)
	require.Len(t, batch, 1)

	rowsAffected, err = store.FailOutbox(
		ctx, sharedID, "outbox-token", "outbox failure",
	)
	require.NoError(t, err)
	require.Equal(t, int64(1), rowsAffected)

	mailboxDL, err := store.GetDeadLetter(ctx, "mailbox", sharedID)
	require.NoError(t, err)
	require.NotNil(t, mailboxDL)
	require.Equal(t, "mailbox failure", mailboxDL.FailureReason)

	outboxDL, err := store.GetDeadLetter(ctx, "outbox", sharedID)
	require.NoError(t, err)
	require.NotNil(t, outboxDL)
	require.Equal(t, "outbox failure", outboxDL.FailureReason)
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
