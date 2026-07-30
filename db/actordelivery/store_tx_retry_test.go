package actordelivery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/jackc/pgerrcode"
	pgconnv5 "github.com/jackc/pgx/v5/pgconn"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/db"
	adsqlc "github.com/lightninglabs/wavelength/db/actordelivery/sqlc"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// noBackoff removes the retry delay so the retry tests do not have to sit
// through the production backoff schedule.
func noBackoff(int) time.Duration {
	return 0
}

// newRetryTestStore builds a transaction-aware store with the given retry
// options applied on top of a zero backoff.
func newRetryTestStore(t *testing.T,
	opts ...TxRetryOption) *TxAwareActorDeliveryStore {

	t.Helper()

	testDB := db.NewTestDB(t)
	actorQueries := adsqlc.New(testDB.DB)

	actorDB := db.NewTransactionExecutor(
		testDB.BaseDB,
		func(tx *sql.Tx) ActorDeliveryQueries {
			return actorQueries.WithTx(tx)
		},
		btclog.Disabled,
	)

	allOpts := append(
		[]TxRetryOption{WithTxRetryBackoff(noBackoff)}, opts...,
	)

	return NewTxAwareActorDeliveryStore(
		actorDB, testDB.BaseDB,
		clock.NewTestClock(
			time.Now(),
		),
		allOpts...,
	)
}

// pgErr builds a postgres driver error carrying the given SQLSTATE, which is
// what the production classifier in db.MapSQLError keys off.
func pgErr(code string) error {
	return &pgconnv5.PgError{
		Code:     code,
		Message:  "synthetic " + code,
		Severity: "ERROR",
	}
}

// TestExecTxRetriesSerializationFailure asserts that a serialization failure
// raised inside the transaction body is replayed rather than surfaced. This is
// the conflict the durable actor egress path hits on Postgres when concurrent
// turns enqueue into mailboxes that their consumers are concurrently scanning.
func TestExecTxRetriesSerializationFailure(t *testing.T) {
	t.Parallel()

	store := newRetryTestStore(t)

	var attempts int
	err := store.ExecTx(
		context.Background(), false,
		func(context.Context, actor.DeliveryStore) error {
			attempts++
			if attempts < 3 {
				return pgErr(pgerrcode.SerializationFailure)
			}

			return nil
		},
	)
	require.NoError(t, err)
	require.Equal(t, 3, attempts, "body should have been replayed twice")
}

// TestExecTxRetriesAbortedTransaction asserts that the aborted-transaction
// state is replayed too. A caller that logs and swallows its own serialization
// failure leaves the transaction poisoned, so the next statement reports 25P02
// instead of the 40001 underneath it. Keying only on 40001 would miss the very
// shape that dropped a client-owed response in production.
func TestExecTxRetriesAbortedTransaction(t *testing.T) {
	t.Parallel()

	store := newRetryTestStore(t)

	var attempts int
	err := store.ExecTx(
		context.Background(), false,
		func(context.Context, actor.DeliveryStore) error {
			attempts++
			if attempts < 2 {
				return pgErr(pgerrcode.InFailedSQLTransaction)
			}

			return nil
		},
	)
	require.NoError(t, err)
	require.Equal(t, 2, attempts, "body should have been replayed once")
}

// TestExecTxDoesNotRetryPlainErrors asserts that an error the transaction
// cannot shake off by running again is handed straight back to the caller,
// unretried. Replaying those would only multiply the work behind a failure
// that is going to happen every time.
func TestExecTxDoesNotRetryPlainErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
	}{
		{
			name: "opaque error",
			err:  errors.New("behavior rejected the message"),
		},
		{
			name: "unique violation",
			err:  pgErr(pgerrcode.UniqueViolation),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			store := newRetryTestStore(t)

			var attempts int
			err := store.ExecTx(
				context.Background(), false,
				func(context.Context,
					actor.DeliveryStore) error {

					attempts++

					return test.err
				},
			)
			require.Error(t, err)
			require.Equal(t, 1, attempts, "should not have retried")
		})
	}
}

// TestExecTxGivesUpAfterRetryBudget asserts that a conflict which never clears
// exhausts a bounded budget and then reports ErrRetriesExceeded. The bound is
// what keeps a turn from retrying its way past its own message lease.
func TestExecTxGivesUpAfterRetryBudget(t *testing.T) {
	t.Parallel()

	const budget = 4

	store := newRetryTestStore(t, WithTxRetries(budget))

	var attempts int
	err := store.ExecTx(
		context.Background(), false,
		func(context.Context, actor.DeliveryStore) error {
			attempts++

			return pgErr(pgerrcode.SerializationFailure)
		},
	)
	require.ErrorIs(t, err, db.ErrRetriesExceeded)
	require.Equal(t, budget, attempts)

	// The failure that drove the retries must survive in the returned
	// error, otherwise an operator only learns that something was retried
	// and never what the conflict was.
	require.True(t, db.IsSerializationError(err))
}

// TestExecTxRetryBudgetFloor asserts that a nonsensical budget still runs the
// transaction once. Taking a zero or negative budget literally would skip the
// loop entirely and report exhausted retries for work that was never attempted,
// which loses the caller's transaction instead of failing it.
func TestExecTxRetryBudgetFloor(t *testing.T) {
	t.Parallel()

	for _, budget := range []int{0, -1} {
		t.Run(fmt.Sprintf("budget %d", budget), func(t *testing.T) {
			t.Parallel()

			store := newRetryTestStore(t, WithTxRetries(budget))

			var attempts int
			err := store.ExecTx(
				context.Background(), false,
				func(context.Context,
					actor.DeliveryStore) error {

					attempts++

					return nil
				},
			)
			require.NoError(t, err)
			require.Equal(t, 1, attempts)
		})
	}
}

// TestExecTxStopsRetryingOnContextCancel asserts that a cancelled context ends
// the retry loop instead of waiting out the remaining budget. Shutdown must not
// have to block on a conflict that no longer matters.
func TestExecTxStopsRetryingOnContextCancel(t *testing.T) {
	t.Parallel()

	// Use the real backoff so the loop actually has a delay to be
	// interrupted during.
	store := newRetryTestStore(
		t, WithTxRetryBackoff(func(int) time.Duration {
			return time.Hour
		}),
	)

	ctx, cancel := context.WithCancel(context.Background())

	var attempts int
	errChan := make(chan error, 1)
	go func() {
		errChan <- store.ExecTx(
			ctx, false,
			func(context.Context, actor.DeliveryStore) error {
				attempts++
				cancel()

				return pgErr(pgerrcode.SerializationFailure)
			},
		)
	}()

	select {
	case err := <-errChan:
		require.ErrorIs(t, err, context.Canceled)
		require.Equal(t, 1, attempts)

	case <-time.After(30 * time.Second):
		t.Fatal("ExecTx did not abandon its backoff on cancellation")
	}
}

// TestExecTxCommitsReplayedWork asserts that work performed by a replayed body
// is actually durable. The retry rolls the first attempt back, so the row the
// caller sees afterwards must be the one the winning attempt wrote.
func TestExecTxCommitsReplayedWork(t *testing.T) {
	t.Parallel()

	store := newRetryTestStore(t)

	mailboxID := generateTestID()
	ctx := context.Background()

	var attempts int
	err := store.ExecTx(
		ctx, false,
		func(txCtx context.Context, txStore actor.DeliveryStore) error {
			attempts++

			// Tag the payload per attempt so the assertions below
			// identify which attempt's row survived, not merely
			// that one did.
			payload := []byte("replayed")
			if attempts < 2 {
				payload = []byte("rolled-back")
			}

			err := txStore.EnqueueMessage(
				txCtx, actor.EnqueueParams{
					ID:          generateTestID(),
					MailboxID:   mailboxID,
					MessageType: "test.Message",
					Payload:     payload,
					AvailableAt: time.
						Now().
						Add(-time.Minute),
					MaxAttempts: 3,
				},
			)
			if err != nil {
				return err
			}

			// Fail the first attempt after the enqueue so the
			// rollback has something to undo.
			if attempts < 2 {
				return pgErr(pgerrcode.SerializationFailure)
			}

			return nil
		},
	)
	require.NoError(t, err)
	require.Equal(t, 2, attempts)

	// The winning attempt's enqueue must be durable.
	msg, err := store.LeaseNextMessage(
		ctx, mailboxID, generateTestID(), time.Minute,
	)
	require.NoError(t, err)
	require.NotNil(t, msg)
	require.Equal(t, []byte("replayed"), msg.Payload)

	// And it must be the only one: the rolled-back attempt also enqueued,
	// so a replay that leaked its predecessor's writes would leave a
	// duplicate behind here.
	dup, err := store.LeaseNextMessage(
		ctx, mailboxID, generateTestID(), time.Minute,
	)
	require.NoError(t, err)
	require.Nil(t, dup, "rolled-back enqueue survived the replay")
}
