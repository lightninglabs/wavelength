package actordelivery

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/db"
	adsqlc "github.com/lightninglabs/wavelength/db/actordelivery/sqlc"
	dbsqlc "github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightninglabs/wavelength/internal/leasefence"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// blockingActorDeliveryDB can pause the next transaction after its BEGIN has
// succeeded but before the store callback runs. This makes clock sampling at
// the transaction-acquisition boundary deterministic.
type blockingActorDeliveryDB struct {
	BatchedActorDeliveryQueries

	mu      sync.Mutex
	entered chan struct{}
	release chan struct{}
}

// blockNext returns channels that observe and release the next transaction
// callback.
func (b *blockingActorDeliveryDB) blockNext() (<-chan struct{},
	chan<- struct{}) {

	b.mu.Lock()
	defer b.mu.Unlock()

	b.entered = make(chan struct{})
	b.release = make(chan struct{})

	return b.entered, b.release
}

// ExecTx delegates transaction acquisition, then pauses an armed callback.
func (b *blockingActorDeliveryDB) ExecTx(ctx context.Context,
	txOptions db.TxOptions, txBody func(ActorDeliveryQueries) error) error {

	return b.BatchedActorDeliveryQueries.ExecTx(
		ctx, txOptions, func(q ActorDeliveryQueries) error {
			b.mu.Lock()
			entered := b.entered
			release := b.release
			b.entered = nil
			b.release = nil
			b.mu.Unlock()

			if entered != nil {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
			}

			return txBody(q)
		},
	)
}

// newBlockingTestStore returns a store whose next transaction callback can be
// held after acquisition.
func newBlockingTestStore(t *testing.T) (actor.TxAwareDeliveryStore,
	*clock.TestClock, *blockingActorDeliveryDB) {

	t.Helper()

	testDB := db.NewTestDB(t)
	queries := adsqlc.New(testDB.DB)
	executor := db.NewTransactionExecutor(
		testDB.BaseDB,
		func(tx *sql.Tx) ActorDeliveryQueries {
			return queries.WithTx(tx)
		},
		btclog.Disabled,
	)
	blockingDB := &blockingActorDeliveryDB{
		BatchedActorDeliveryQueries: executor,
	}
	clk := clock.NewTestClock(time.Unix(1_700_000_000, 0))

	return NewTxAwareActorDeliveryStore(
		blockingDB, testDB.BaseDB, clk,
	), clk, blockingDB
}

// observingClock reports the next time a caller samples Now.
type observingClock struct {
	*clock.TestClock

	mu       sync.Mutex
	observed chan struct{}
}

// observeNext returns a channel closed by the next Now call.
func (c *observingClock) observeNext() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.observed = make(chan struct{})

	return c.observed
}

// Now returns the test time and reports an armed observation.
func (c *observingClock) Now() time.Time {
	c.mu.Lock()
	observed := c.observed
	c.observed = nil
	c.mu.Unlock()

	if observed != nil {
		close(observed)
	}

	return c.TestClock.Now()
}

// lockObservingQueries reports when a PostgreSQL row-lock statement starts,
// immediately before delegating to the query that may wait.
type lockObservingQueries struct {
	ActorDeliveryQueries

	started chan struct{}
	once    *sync.Once
}

// LockMailboxMessageForUpdate reports and acquires the mailbox row lock.
func (q *lockObservingQueries) LockMailboxMessageForUpdate(ctx context.Context,
	id string) (string, error) {

	q.once.Do(func() {
		close(q.started)
	})

	return q.ActorDeliveryQueries.LockMailboxMessageForUpdate(ctx, id)
}

// leaseMutation describes one token-fenced mailbox ownership mutation.
type leaseMutation struct {
	name string
	run  func(context.Context, actor.DeliveryStore,
		*actor.LeasedMessage) (int64, error)
}

// fencedLeaseMutations returns every mailbox mutation that requires a live
// lease. Keeping the cases together pins their shared boundary semantics.
func fencedLeaseMutations() []leaseMutation {
	return []leaseMutation{
		{
			name: "ack",
			run: func(ctx context.Context,
				store actor.DeliveryStore,
				msg *actor.LeasedMessage) (int64, error) {

				return store.AckMessage(
					ctx, msg.ID, msg.LeaseToken,
				)
			},
		},
		{
			name: "postpone",
			run: func(ctx context.Context,
				store actor.DeliveryStore,
				msg *actor.LeasedMessage) (int64, error) {

				return store.PostponeMessage(
					ctx, msg.ID, msg.LeaseToken,
					time.Minute,
				)
			},
		},
		{
			name: "nack",
			run: func(ctx context.Context,
				store actor.DeliveryStore,
				msg *actor.LeasedMessage) (int64, error) {

				return store.NackMessage(
					ctx, msg.ID, msg.LeaseToken,
					time.Minute,
				)
			},
		},
		{
			name: "extend",
			run: func(ctx context.Context,
				store actor.DeliveryStore,
				msg *actor.LeasedMessage) (int64, error) {

				return store.ExtendLease(
					ctx, msg.ID, msg.LeaseToken,
					time.Minute,
				)
			},
		},
	}
}

// TestFencedLeaseMutationsRejectExpiredOwner verifies a matching token is not
// enough once its lease has expired. The same backend-selected test runs on
// SQLite normally and PostgreSQL under the test_postgres build tag.
func TestFencedLeaseMutationsRejectExpiredOwner(t *testing.T) {
	t.Parallel()

	for _, test := range fencedLeaseMutations() {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			store, clk := newBackendTestStore(t)
			ctx := t.Context()
			mailbox := "expired-" + test.name
			id := test.name + "-001"

			enqueueWithMaxAttempts(
				t, ctx, store, clk, id, mailbox, "", 3,
			)
			leased := requireLeasedID(
				t, ctx, store, mailbox, test.name+"-token", id,
				time.Second,
			)

			clk.SetTime(leased.LeaseUntil.Add(time.Second))

			rows, err := test.run(ctx, store, leased)
			require.NoError(t, err)
			require.Zero(t, rows)

			// None of the stale operations may make the row
			// unavailable. It remains immediately reclaimable
			// because its original availability and retry budget
			// are intact.
			reclaimed := requireLeasedID(
				t, ctx, store, mailbox, test.name+"-new-token",
				id, time.Minute,
			)
			require.Equal(t, leased.Attempts+1, reclaimed.Attempts)
		})
	}
}

// TestFencedLeaseMutationsAcceptExpiryBoundary pins the intentional inclusive
// boundary: a lease remains live at lease_until and expires only once the
// store clock advances past it.
func TestFencedLeaseMutationsAcceptExpiryBoundary(t *testing.T) {
	t.Parallel()

	for _, test := range fencedLeaseMutations() {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			store, clk := newBackendTestStore(t)
			ctx := t.Context()
			mailbox := "boundary-" + test.name
			id := test.name + "-001"

			enqueueWithMaxAttempts(
				t, ctx, store, clk, id, mailbox, "", 3,
			)
			leased := requireLeasedID(
				t, ctx, store, mailbox, test.name+"-token", id,
				time.Second,
			)

			clk.SetTime(leased.LeaseUntil)

			rows, err := test.run(ctx, store, leased)
			require.NoError(t, err)
			require.EqualValues(t, 1, rows)
		})
	}
}

// TestFencedLeaseMutationsSampleClockAfterTransactionStart verifies a writer
// that waits to enter its transaction cannot carry an earlier authorization
// time into the eventual ownership check. The predecessor is on its final
// attempt, so an expired postpone would otherwise restore its budget after the
// successor has become eligible and invert the lane.
func TestFencedLeaseMutationsSampleClockAfterTransactionStart(t *testing.T) {
	t.Parallel()

	for _, test := range fencedLeaseMutations() {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			store, clk, blocker := newBlockingTestStore(t)
			ctx := t.Context()
			mailbox := "delayed-" + test.name
			predecessor := test.name + "-001"
			successor := test.name + "-002"

			enqueueWithMaxAttempts(
				t, ctx, store, clk, predecessor, mailbox,
				"lane-a", 1,
			)
			enqueueWithMaxAttempts(
				t, ctx, store, clk, successor, mailbox,
				"lane-a", 3,
			)
			leased := requireLeasedID(
				t, ctx, store, mailbox, test.name+"-token",
				predecessor, time.Second,
			)

			entered, release := blocker.blockNext()
			type mutationResult struct {
				rows int64
				err  error
			}
			result := make(chan mutationResult, 1)
			go func() {
				rows, err := test.run(ctx, store, leased)
				result <- mutationResult{rows: rows, err: err}
			}()

			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal(
					"mutation did not acquire its " +
						"transaction",
				)
			}

			clk.SetTime(leased.LeaseUntil.Add(time.Second))
			close(release)

			select {
			case mutation := <-result:
				require.NoError(t, mutation.err)
				require.Zero(t, mutation.rows)

			case <-time.After(5 * time.Second):
				t.Fatal("mutation did not finish")
			}

			requireLeasedID(
				t, ctx, store, mailbox, test.name+"-successor",
				successor, time.Minute,
			)
		})
	}
}

// TestPostgresFencedMutationSamplesClockAfterRowLock verifies statement-level
// lock waits cannot preserve an earlier lease authorization. PostgreSQL may
// acquire its transaction promptly and then wait on the mailbox row, so the
// store must lock that row before reading its clock.
func TestPostgresFencedMutationSamplesClockAfterRowLock(t *testing.T) {
	t.Parallel()

	testDB := db.NewTestDB(t)
	if testDB.Backend() != dbsqlc.BackendTypePostgres {
		t.Skip("PostgreSQL row-lock regression")
	}

	ctx := t.Context()
	baseClock := clock.NewTestClock(time.Unix(1_700_000_000, 0))
	clk := &observingClock{TestClock: baseClock}
	store, err := NewTxAwareDeliveryStoreFromDB(
		testDB.DB, testDB.Backend(), baseClock, btclog.Disabled,
	)
	require.NoError(t, err)

	const (
		mailbox     = "postgres-row-lock"
		predecessor = "row-lock-001"
	)
	enqueueWithMaxAttempts(
		t, ctx, store, baseClock, predecessor, mailbox, "", 3,
	)

	holder, err := testDB.BaseDB.BeginTx(ctx, db.WriteTxOption())
	require.NoError(t, err)
	defer func() {
		_ = holder.Rollback()
	}()

	_, err = adsqlc.New(holder).LockMailboxMessageForUpdate(
		ctx, predecessor,
	)
	require.NoError(t, err)

	waiter, err := testDB.BaseDB.BeginTx(ctx, db.WriteTxOption())
	require.NoError(t, err)
	defer func() {
		_ = waiter.Rollback()
	}()

	lockStarted := make(chan struct{})
	var lockOnce sync.Once
	waiterQueries := &lockObservingQueries{
		ActorDeliveryQueries: adsqlc.New(waiter),
		started:              lockStarted,
		once:                 &lockOnce,
	}
	clockObserved := clk.observeNext()
	type timeResult struct {
		now time.Time
		err error
	}
	result := make(chan timeResult, 1)
	go func() {
		now, err := mailboxMutationTime(
			ctx, waiterQueries, dbsqlc.BackendTypePostgres, clk,
			predecessor,
		)
		result <- timeResult{now: now, err: err}
	}()

	select {
	case <-lockStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("postpone did not reach the mailbox row lock")
	}

	select {
	case <-clockObserved:
		t.Fatal(
			"store sampled time before acquiring the mailbox " +
				"row lock",
		)

	default:
	}

	advanced := baseClock.Now().Add(time.Minute)
	baseClock.SetTime(advanced)
	require.NoError(t, holder.Rollback())

	select {
	case sample := <-result:
		require.NoError(t, sample.err)
		require.Equal(t, advanced, sample.now)

	case <-time.After(5 * time.Second):
		t.Fatal(
			"postpone did not finish after the row lock was " +
				"released",
		)
	}

	select {
	case <-clockObserved:
	case <-time.After(5 * time.Second):
		t.Fatal(
			"postpone did not sample time after acquiring the " +
				"row lock",
		)
	}

}

// TestExpiredPostponeCannotRestoreFinalAttempt verifies the final-attempt
// ordering boundary. An expired owner cannot restore its retry budget, so the
// exhausted predecessor remains terminal and its same-key successor proceeds.
func TestExpiredPostponeCannotRestoreFinalAttempt(t *testing.T) {
	t.Parallel()

	store, clk := newBackendTestStore(t)
	ctx := t.Context()

	const (
		mailbox = "expired-postpone-final"
		key     = "lane-a"
	)

	enqueueWithMaxAttempts(
		t, ctx, store, clk, "postpone-001", mailbox, key, 1,
	)
	enqueueWithMaxAttempts(
		t, ctx, store, clk, "postpone-002", mailbox, key, 3,
	)

	first := requireLeasedID(
		t, ctx, store, mailbox, "stale-token", "postpone-001",
		time.Second,
	)
	clk.SetTime(first.LeaseUntil.Add(time.Second))

	rows, err := store.PostponeMessage(
		ctx, first.ID, first.LeaseToken, 0,
	)
	require.NoError(t, err)
	require.Zero(t, rows)

	requireLeasedID(
		t, ctx, store, mailbox, "successor-token", "postpone-002",
		time.Minute,
	)
}

// TestExpiredAckRollsBackEffectsAfterSuccessorClaim verifies a stale final
// attempt cannot commit transaction effects after its successor becomes the
// live lane owner. The zero-row ack aborts every preceding write in the same
// transaction.
func TestExpiredAckRollsBackEffectsAfterSuccessorClaim(t *testing.T) {
	t.Parallel()

	store, clk := newBackendTestStore(t)
	ctx := t.Context()

	const (
		mailbox      = "expired-ack-effects"
		key          = "lane-a"
		predecessor  = "ack-001"
		successor    = "ack-002"
		checkpointID = "expired-ack-actor"
	)

	enqueueWithMaxAttempts(
		t, ctx, store, clk, predecessor, mailbox, key, 1,
	)
	enqueueWithMaxAttempts(
		t, ctx, store, clk, successor, mailbox, key, 3,
	)

	first := requireLeasedID(
		t, ctx, store, mailbox, "predecessor-token", predecessor,
		time.Second,
	)
	clk.SetTime(first.LeaseUntil.Add(time.Second))

	second := requireLeasedID(
		t, ctx, store, mailbox, "successor-token", successor,
		time.Minute,
	)

	errLeaseFence := errors.New("lease fence rejected stale owner")
	err := store.ExecTx(ctx, false, func(txCtx context.Context,
		txStore actor.DeliveryStore) error {

		if err := txStore.SaveCheckpoint(
			txCtx, actor.CheckpointParams{
				ActorID:   checkpointID,
				StateType: "test.State",
				StateData: []byte("stale-effect"),
				Version:   1,
			},
		); err != nil {
			return err
		}

		if err := txStore.MarkProcessed(
			txCtx, predecessor, checkpointID, time.Hour,
		); err != nil {
			return err
		}

		rows, err := txStore.AckMessage(
			txCtx, predecessor, first.LeaseToken,
		)
		if err != nil {
			return err
		}
		if rows == 0 {
			return errLeaseFence
		}

		return nil
	})
	require.ErrorIs(t, err, errLeaseFence)

	checkpoint, err := store.LoadCheckpoint(ctx, checkpointID)
	require.NoError(t, err)
	require.Nil(t, checkpoint)

	processed, err := store.IsProcessed(ctx, predecessor)
	require.NoError(t, err)
	require.False(t, processed)

	rows, err := store.AckMessage(ctx, second.ID, second.LeaseToken)
	require.NoError(t, err)
	require.EqualValues(t, 1, rows)
}

// TestFencedDeadLetterRequiresLiveLease verifies a terminal by-ID move carries
// the same ownership boundary as ack, nack, postpone, and extend.
func TestFencedDeadLetterRequiresLiveLease(t *testing.T) {
	t.Parallel()

	t.Run("expired owner rejected", func(t *testing.T) {
		t.Parallel()

		store, clk := newBackendTestStore(t)
		ctx := t.Context()

		const (
			mailbox = "expired-dead-letter"
			key     = "lane-a"
		)

		enqueueWithMaxAttempts(
			t, ctx, store, clk, "dead-001", mailbox, key, 1,
		)
		enqueueWithMaxAttempts(
			t, ctx, store, clk, "dead-002", mailbox, key, 3,
		)

		first := requireLeasedID(
			t, ctx, store, mailbox, "stale-token", "dead-001",
			time.Second,
		)
		clk.SetTime(first.LeaseUntil.Add(time.Second))

		fencedCtx := leasefence.WithToken(ctx, first.LeaseToken)
		err := store.MoveToDeadLetter(
			fencedCtx, first.ID, "terminal failure",
		)
		require.ErrorIs(t, err, actor.ErrLeaseExpired)

		deadLetter, err := store.GetDeadLetter(ctx, first.ID)
		require.NoError(t, err)
		require.Nil(t, deadLetter)

		requireLeasedID(
			t, ctx, store, mailbox, "successor-token", "dead-002",
			time.Minute,
		)
	})

	t.Run("expiry boundary accepted", func(t *testing.T) {
		t.Parallel()

		store, clk := newBackendTestStore(t)
		ctx := t.Context()

		enqueueWithMaxAttempts(
			t, ctx, store, clk, "dead-boundary", "boundary", "", 1,
		)
		leased := requireLeasedID(
			t, ctx, store, "boundary", "boundary-token",
			"dead-boundary", time.Second,
		)
		clk.SetTime(leased.LeaseUntil)

		fencedCtx := leasefence.WithToken(ctx, leased.LeaseToken)
		require.NoError(
			t, store.MoveToDeadLetter(
				fencedCtx, leased.ID, "terminal failure",
			),
		)

		deadLetter, err := store.GetDeadLetter(ctx, leased.ID)
		require.NoError(t, err)
		require.NotNil(t, deadLetter)

		peeked, err := store.PeekNextMessage(ctx, "boundary")
		require.NoError(t, err)
		require.Nil(t, peeked)
	})
}
