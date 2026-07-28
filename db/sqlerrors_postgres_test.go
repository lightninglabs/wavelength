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
	})
}
