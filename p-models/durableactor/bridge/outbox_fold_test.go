package bridge

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/db/actordelivery"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightningnetwork/lnd/clock"
)

// foldClaimDuration is the outbox claim lease the fold bridge tests hold. It is
// long enough that a re-claim inside the window is rejected and short enough
// that advancing the test clock past it makes the row reclaimable.
const foldClaimDuration = 30 * time.Second

// errFoldInjected simulates a CompleteOutbox failure (or a crash) AFTER the
// target enqueue inside the folded delivery transaction. Returning it from the
// ExecTx closure rolls the whole transaction back, exactly as a real
// CompleteOutbox error would in deliverMessage.
var errFoldInjected = errors.New("injected outbox fold failure")

// newFoldStore builds a transaction-aware actor-delivery store backed by a
// fresh migrated SQLite database, matching the production wiring the outbox
// publisher uses (NewTxAwareDeliveryStoreFromDB + the same backend type).
func newFoldStore(t *testing.T, clk clock.Clock) actor.TxAwareDeliveryStore {
	t.Helper()

	rawDB := newSQLiteDB(t)
	requireNoError(
		t, actordelivery.RunMigrations(rawDB, sqlc.BackendTypeSqlite),
	)

	store, err := actordelivery.NewTxAwareDeliveryStoreFromDB(
		rawDB, sqlc.BackendTypeSqlite, clk, btclog.Disabled,
	)
	requireNoError(t, err)

	return store
}

// foldDelivery replays deliverMessage's transactional fold against the real
// store: inside ONE ExecTx it durably enqueues the message into the target
// mailbox (the "Tell") and then completes the outbox row. The enqueue is keyed
// by the outbox id so a redelivery is an idempotent ON CONFLICT no-op, exactly
// as WithOutboxID arranges in production. When failAfterEnqueue is set the
// closure returns errFoldInjected after the enqueue, modeling a CompleteOutbox
// error or a crash mid-fold; ExecTx then rolls the enqueue back atomically.
func foldDelivery(t *testing.T, store actor.TxAwareDeliveryStore,
	clk clock.Clock, outboxID, target, completeToken string,
	failAfterEnqueue bool) error {

	t.Helper()

	return store.ExecTx(
		t.Context(), false,
		func(txCtx context.Context, s actor.DeliveryStore) error {
			err := s.EnqueueMessage(txCtx, actor.EnqueueParams{
				ID:          outboxID,
				MailboxID:   target,
				MessageType: "model.TraceMsg",
				Payload:     []byte("fold-payload"),
				AvailableAt: clk.Now(),
				MaxAttempts: 3,
			})
			if err != nil {
				return err
			}

			// Model the failure window between the durable enqueue
			// and the completion: the fold must roll the enqueue
			// back rather than leave an orphan in the target
			// mailbox.
			if failAfterEnqueue {
				return errFoldInjected
			}

			return s.CompleteOutbox(txCtx, outboxID, completeToken)
		},
	)
}

// claimOutbox claims up to ten pending outbox rows under the given token,
// mirroring publishBatch's per-poll claim.
func claimOutbox(t *testing.T, store actor.TxAwareDeliveryStore,
	token string) []actor.OutboxMessage {

	t.Helper()

	msgs, err := store.ClaimOutboxBatch(
		t.Context(), actor.OutboxClaimParams{
			Limit:         10,
			ClaimToken:    token,
			ClaimDuration: foldClaimDuration,
		},
	)
	requireNoError(t, err)

	return msgs
}

// leaseTarget leases the next message from the target mailbox, returning nil
// when the mailbox is empty.
func leaseTarget(t *testing.T, store actor.TxAwareDeliveryStore, target,
	token string) *actor.LeasedMessage {

	t.Helper()

	leased, err := store.LeaseNextMessage(
		t.Context(), target, token, time.Minute,
	)
	requireNoError(t, err)

	return leased
}

// TestOutboxFoldRollbackErasesEnqueueAndRetries is the bridge analog of the P
// outbox-fold contract and of deliverMessage's transactional path. A folded
// delivery that fails after the target enqueue must leave NO orphan in the
// target mailbox, must leave the outbox row pending (and unclaimable until the
// claim expires), and must redeliver exactly once after the claim expires. It
// drives the real SQLite store so the ExecTx rollback, the claim-expiry reclaim
// SQL, and the token-fenced completion are all exercised, not modeled.
func TestOutboxFoldRollbackErasesEnqueueAndRetries(t *testing.T) {
	t.Parallel()

	const (
		outboxID = "outbox-fold-1"
		target   = "target-actor"
	)

	clk := clock.NewTestClock(traceTime(0))
	store := newFoldStore(t, clk)

	requireNoError(
		t,
		store.EnqueueOutbox(
			t.Context(), actor.OutboxParams{
				ID:            outboxID,
				SourceActorID: "source-actor",
				TargetActorID: target,
				MessageType:   "model.TraceMsg",
				Payload:       []byte("fold-payload"),
			},
		),
	)

	// Publisher claims the row (token-1): delivery_attempts goes to 1.
	claimed := claimOutbox(t, store, "token-1")
	if len(claimed) != 1 {
		t.Fatalf("expected 1 claimed outbox row, got %d", len(claimed))
	}
	if claimed[0].ID != outboxID {
		t.Fatalf("expected claimed outbox id %s, got %s", outboxID,
			claimed[0].ID)
	}
	if claimed[0].DeliveryAttempts != 1 {
		t.Fatalf("expected delivery_attempts 1 after first "+
			"claim, got %d", claimed[0].DeliveryAttempts)
	}

	// The folded delivery fails after the enqueue, so the whole tx rolls
	// back.
	err := foldDelivery(t, store, clk, outboxID, target, "token-1", true)
	if !errors.Is(err, errFoldInjected) {
		t.Fatalf("expected injected fold failure, got %v", err)
	}

	// Rollback erased the target enqueue: the target mailbox is empty.
	if leased := leaseTarget(t, store, target, "probe"); leased != nil {
		t.Fatalf("rolled-back fold left an orphan enqueue %s in the "+
			"target mailbox", leased.ID)
	}

	// The outbox row is still pending but NOT reclaimable while the first
	// claim is live: a re-claim inside the claim window returns nothing.
	clk.SetTime(traceTime(10))
	if held := claimOutbox(t, store, "token-early"); len(held) != 0 {
		t.Fatalf("outbox row reclaimed inside its live claim "+
			"window: %d rows", len(held))
	}

	// After the claim expires the row is reclaimable again, and the
	// delivery_attempts bump from the first claim survived the rolled-back
	// fold (the claim commits in its own tx, separate from the fold).
	clk.SetTime(traceTime(40))
	reclaimed := claimOutbox(t, store, "token-2")
	if len(reclaimed) != 1 {
		t.Fatalf("expected the row to be reclaimable after claim "+
			"expiry, got %d rows", len(reclaimed))
	}
	if reclaimed[0].DeliveryAttempts != 2 {
		t.Fatalf("expected delivery_attempts 2 after reclaim, got %d",
			reclaimed[0].DeliveryAttempts)
	}

	// Redelivery commits the fold: target enqueue and completion land
	// atomically.
	requireNoError(
		t,
		foldDelivery(t, store, clk, outboxID, target, "token-2", false),
	)

	// The target mailbox now holds exactly one copy of the message.
	leased := leaseTarget(t, store, target, "consumer")
	if leased == nil || leased.ID != outboxID {
		t.Fatalf("expected the redelivered row %s in the target "+
			"mailbox, got %v", outboxID, leased)
	}
	rows, err := store.AckMessage(t.Context(), outboxID, leased.LeaseToken)
	requireNoError(t, err)
	if rows != 1 {
		t.Fatalf("expected to ack exactly 1 target row, got %d", rows)
	}
	if dup := leaseTarget(t, store, target, "consumer-2"); dup != nil {
		t.Fatalf("target mailbox held a duplicate row %s after the "+
			"fold redelivery", dup.ID)
	}

	// The outbox row is completed: no further claim returns it, even past a
	// later claim expiry.
	clk.SetTime(traceTime(100))
	if done := claimOutbox(t, store, "token-3"); len(done) != 0 {
		t.Fatalf("completed outbox row was reclaimed: %d rows",
			len(done))
	}
}

// TestOutboxFoldConcurrentReclaimDeliversExactlyOnce drives the cross-publisher
// reclaim race the claim token exists to fence. A slow publisher's claim
// expires and a second publisher reclaims the row; the stale publisher then
// commits its fold with its OLD token. Its enqueue commits (idempotent by id)
// but its CompleteOutbox is a zero-row no-op because the row is now owned by
// the new token, so the outbox stays pending. The current owner's fold then
// finds the enqueue already present (ON CONFLICT no-op) and completes under its
// matching token. The target mailbox must end with exactly one copy of the
// message: receiver-side dedup collapses the duplicate enqueue and the
// token-fenced completion admits only the current owner.
func TestOutboxFoldConcurrentReclaimDeliversExactlyOnce(t *testing.T) {
	t.Parallel()

	const (
		outboxID = "outbox-fold-2"
		target   = "target-actor"
	)

	clk := clock.NewTestClock(traceTime(0))
	store := newFoldStore(t, clk)

	requireNoError(
		t,
		store.EnqueueOutbox(
			t.Context(), actor.OutboxParams{
				ID:            outboxID,
				SourceActorID: "source-actor",
				TargetActorID: target,
				MessageType:   "model.TraceMsg",
				Payload:       []byte("fold-payload"),
			},
		),
	)

	// Publisher P1 claims at t=0 (token-1).
	first := claimOutbox(t, store, "token-1")
	if len(first) != 1 || first[0].DeliveryAttempts != 1 {
		t.Fatalf("expected 1 row at attempts 1 for P1, got %+v", first)
	}

	// P1 stalls past its claim; P2 reclaims the same row at t=40 (token-2).
	clk.SetTime(traceTime(40))
	second := claimOutbox(t, store, "token-2")
	if len(second) != 1 || second[0].DeliveryAttempts != 2 {
		t.Fatalf("expected 1 row at attempts 2 for P2, got %+v", second)
	}

	// Stale P1 finishes first with its now-superseded token. The enqueue
	// commits, but CompleteOutbox(token-1) matches zero rows (the row is
	// owned by token-2), so the tx commits with the outbox still pending.
	requireNoError(
		t,
		foldDelivery(t, store, clk, outboxID, target, "token-1", false),
	)

	// The outbox row is still pending: P1's stale completion was fenced
	// out. (It is owned by token-2 until that claim expires, so a fresh
	// claim inside the window sees nothing.)
	if held := claimOutbox(t, store, "token-peek"); len(held) != 0 {
		t.Fatalf("expected the row to stay owned by token-2, got %d "+
			"reclaimable rows", len(held))
	}

	// Current owner P2 runs its fold: the enqueue is an idempotent no-op
	// (the id already exists from P1) and CompleteOutbox(token-2) matches,
	// so the row is completed.
	requireNoError(
		t,
		foldDelivery(t, store, clk, outboxID, target, "token-2", false),
	)

	// Despite two folds enqueuing, the target mailbox holds exactly one
	// row.
	leased := leaseTarget(t, store, target, "consumer")
	if leased == nil || leased.ID != outboxID {
		t.Fatalf("expected exactly the delivered row %s, got %v",
			outboxID, leased)
	}
	rows, err := store.AckMessage(t.Context(), outboxID, leased.LeaseToken)
	requireNoError(t, err)
	if rows != 1 {
		t.Fatalf("expected to ack exactly 1 target row, got %d", rows)
	}
	if dup := leaseTarget(t, store, target, "consumer-2"); dup != nil {
		t.Fatalf("concurrent reclaim double-delivered to the target "+
			"mailbox: duplicate %s", dup.ID)
	}

	// The outbox row is completed: no further claim returns it.
	clk.SetTime(traceTime(200))
	if done := claimOutbox(t, store, "token-3"); len(done) != 0 {
		t.Fatalf("completed outbox row was reclaimed: %d rows",
			len(done))
	}
}
