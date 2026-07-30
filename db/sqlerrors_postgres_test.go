//go:build test_postgres

package db

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
)

// beginAt opens a raw transaction at an explicit isolation level, bypassing the
// BaseDB policy so that a test can pit two specific levels against each other.
func beginAt(t *testing.T, ctx context.Context, store *PostgresStore,
	level sql.IsolationLevel) *sql.Tx {

	t.Helper()

	tx, err := store.DB.BeginTx(ctx, &sql.TxOptions{Isolation: level})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = tx.Rollback()
	})

	return tx
}

// TestPostgresConflictShapes pins down the three conflict shapes that the
// isolation work turns on, and asserts that our mapped errors carry enough
// information to tell them apart in a log.
//
// The subtests share one store, and use disjoint id ranges so that they cannot
// interfere. Each store costs a docker container, and the fixture derives its
// port from a docker port binding that is not always populated under load.
func TestPostgresConflictShapes(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := NewTestPostgresDB(t)

	// A genuine SSI abort. Two serializable transactions each read the set
	// of rows the other is about to write, which is the read-write
	// dependency cycle that only predicate locks can see. Postgres reports
	// the loser with a reason code naming its role as the pivot, and that
	// reason code is the signal that a path genuinely depends on SSI.
	t.Run("serializable pivot abort", func(t *testing.T) {
		txA := beginAt(t, ctx, store, sql.LevelSerializable)
		txB := beginAt(t, ctx, store, sql.LevelSerializable)

		var count int
		require.NoError(
			t, txA.QueryRowContext(
				ctx, "SELECT COUNT(*) FROM chain_info WHERE "+
					"id > 100",
			).Scan(&count),
		)
		require.NoError(
			t, txB.QueryRowContext(
				ctx, "SELECT COUNT(*) FROM chain_info WHERE "+
					"id > 100",
			).Scan(&count),
		)

		_, err := txA.ExecContext(
			ctx, "INSERT INTO chain_info (id, chain_name, "+
				"genesis_hash) VALUES (101, 'ssi-a', '\\x01')",
		)
		require.NoError(t, err)

		_, err = txB.ExecContext(
			ctx, "INSERT INTO chain_info (id, chain_name, "+
				"genesis_hash) VALUES (102, 'ssi-b', '\\x02')",
		)
		require.NoError(t, err)

		require.NoError(t, txA.Commit())

		err = txB.Commit()
		require.Error(t, err)

		mapped := MapSQLError(err)
		require.True(t, IsSerializationOrDeadlockError(mapped))
		require.NotEmpty(t, PgErrorDetail(err))
		require.Contains(t, mapped.Error(), "Reason code")
		require.Contains(t, mapped.Error(), "detail:")
	})

	// The load-bearing assumption behind the write-path audit: REPEATABLE
	// READ still detects a write-write conflict on the same row and still
	// reports it as a retryable 40001. This is what makes the "touch a
	// shared row" remedy work as a replacement for an SSI dependency.
	// Unlike the abort above, this one carries no reason code.
	t.Run("repeatable read same-row conflict", func(t *testing.T) {
		_, err := store.DB.ExecContext(
			ctx, "INSERT INTO chain_info (id, chain_name, "+
				"genesis_hash) VALUES (200, 'rr-seed', "+
				"'\\x00')",
		)
		require.NoError(t, err)

		txA := beginAt(t, ctx, store, sql.LevelRepeatableRead)
		txB := beginAt(t, ctx, store, sql.LevelRepeatableRead)

		// Force both snapshots to be taken before either write lands.
		var seen int
		require.NoError(
			t, txA.QueryRowContext(
				ctx, "SELECT id FROM chain_info WHERE id = 200",
			).Scan(&seen),
		)
		require.NoError(
			t, txB.QueryRowContext(
				ctx, "SELECT id FROM chain_info WHERE id = 200",
			).Scan(&seen),
		)

		_, err = txA.ExecContext(
			ctx, "UPDATE chain_info SET chain_name = 'rr-a' "+
				"WHERE id = 200",
		)
		require.NoError(t, err)
		require.NoError(t, txA.Commit())

		_, err = txB.ExecContext(
			ctx, "UPDATE chain_info SET chain_name = 'rr-b' "+
				"WHERE id = 200",
		)
		require.Error(t, err)
		require.True(
			t,
			IsSerializationOrDeadlockError(
				MapSQLError(err),
			),
		)
		require.Empty(t, PgErrorDetail(err))
	})

	// The hazard that the isolation change introduces. Under SSI a lost
	// creation race aborts with a retryable 40001, but under REPEATABLE
	// READ the two inserts do not conflict in the graph at all and the
	// loser instead gets a 23505 unique violation, which the retry loop
	// correctly refuses to retry. Any insert that can race another insert
	// of the same logical row therefore has to be a no-op upsert.
	t.Run("repeatable read lost creation race", func(t *testing.T) {
		txA := beginAt(t, ctx, store, sql.LevelRepeatableRead)
		txB := beginAt(t, ctx, store, sql.LevelRepeatableRead)

		const insertRow = "INSERT INTO chain_info (id, chain_name, " +
			"genesis_hash) VALUES ($1, 'race', '\\x01')"

		_, err := txA.ExecContext(ctx, insertRow, 300)
		require.NoError(t, err)

		// txB blocks on the unique index until txA resolves.
		errChan := make(chan error, 1)
		go func() {
			_, execErr := txB.ExecContext(ctx, insertRow, 301)
			errChan <- execErr
		}()

		require.NoError(t, txA.Commit())

		err = <-errChan
		require.Error(t, err)

		mapped := MapSQLError(err)

		// The decisive property: this is NOT classified as retryable,
		// so a retry loop gives up on it.
		require.False(t, IsSerializationOrDeadlockError(mapped))
		require.True(t, IsUniqueConstraintViolation(mapped))

		// The constraint name and detail together identify which index
		// fired and on what key. The schema carries six partial unique
		// indexes, so the constraint name is not redundant.
		require.NotEmpty(t, PgErrorConstraint(err))
		require.Contains(t, mapped.Error(), "constraint:")
		require.Contains(t, mapped.Error(), "chain_name")

		// Extraction has to work through the mapped error too, not
		// just the raw driver error. A caller downstream of ExecTx
		// only ever sees the mapped one, so this is the path that
		// actually gets used.
		require.NotEmpty(t, PgErrorConstraint(mapped))
		require.NotEmpty(t, PgErrorDetail(mapped))
	})

	// The limit of the shape above, and the reason the write-path audit
	// splits the lost creation race in two. SSI only promotes a creation
	// race to a retryable 40001 when the losing transaction read the
	// contested key first, because that read is what leaves the SIRead
	// predicate lock the conflict graph is built from. Two transactions
	// that insert blind, with no preceding read, have no dependency for
	// SSI to find, so the loser gets the same non-retryable 23505 at
	// SERIALIZABLE that it would get at REPEATABLE READ.
	//
	// This is what says that a blind-write upsert whose ON CONFLICT target
	// misses the index that can actually fire is exposed today, rather
	// than being masked by SSI and unmasked by relaxing the level.
	t.Run("serializable blind creation race", func(t *testing.T) {
		txA := beginAt(t, ctx, store, sql.LevelSerializable)
		txB := beginAt(t, ctx, store, sql.LevelSerializable)

		const insertRow = "INSERT INTO chain_info (id, chain_name, " +
			"genesis_hash) VALUES ($1, 'blind-race', '\\x01')"

		_, err := txA.ExecContext(ctx, insertRow, 400)
		require.NoError(t, err)

		// txB blocks on the unique index until txA resolves.
		errChan := make(chan error, 1)
		go func() {
			_, execErr := txB.ExecContext(ctx, insertRow, 401)
			errChan <- execErr
		}()

		require.NoError(t, txA.Commit())

		err = <-errChan
		require.Error(t, err)

		mapped := MapSQLError(err)

		// Identical classification to the REPEATABLE READ case above,
		// which is the whole point: SERIALIZABLE bought this path
		// nothing, so relaxing the level costs it nothing.
		require.False(t, IsSerializationOrDeadlockError(mapped))
		require.True(t, IsUniqueConstraintViolation(mapped))
		require.NotEmpty(t, PgErrorConstraint(err))
	})

	// The other half of the split: once the losing transaction reads the
	// contested key before inserting it, SSI does see the dependency and
	// reports a retryable 40001 carrying a pivot reason code. A read-check
	// then insert really is masked by SERIALIZABLE today, and really would
	// degrade to a bare 23505 at REPEATABLE READ.
	t.Run("serializable read-check creation race", func(t *testing.T) {
		txA := beginAt(t, ctx, store, sql.LevelSerializable)
		txB := beginAt(t, ctx, store, sql.LevelSerializable)

		const probe = "SELECT COUNT(*) FROM chain_info WHERE " +
			"chain_name = 'checked-race'"
		const insertRow = "INSERT INTO chain_info (id, chain_name, " +
			"genesis_hash) VALUES ($1, 'checked-race', '\\x01')"

		// Both transactions observe the absence of the key first,
		// which is the read that takes the predicate lock.
		var count int
		require.NoError(
			t, txA.QueryRowContext(ctx, probe).Scan(&count),
		)
		require.Zero(t, count)
		require.NoError(
			t, txB.QueryRowContext(ctx, probe).Scan(&count),
		)
		require.Zero(t, count)

		_, err := txA.ExecContext(ctx, insertRow, 500)
		require.NoError(t, err)

		errChan := make(chan error, 1)
		go func() {
			_, execErr := txB.ExecContext(ctx, insertRow, 501)
			errChan <- execErr
		}()

		require.NoError(t, txA.Commit())

		err = <-errChan
		require.Error(t, err)

		mapped := MapSQLError(err)
		require.True(t, IsSerializationOrDeadlockError(mapped))
		require.False(t, IsUniqueConstraintViolation(mapped))
		require.Contains(t, mapped.Error(), "Reason code")
	})
}
