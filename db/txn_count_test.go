package db

import (
	"context"
	"database/sql"
	"testing"

	"github.com/lightninglabs/darepo-client/db/sqlc"
	"github.com/stretchr/testify/require"
)

// TestTxnAccountingCountsCommits verifies that enabling transaction
// accounting attributes committed read and write transactions to caller
// buckets, splits them by the read-only flag, and that a reset clears the
// ledger. The test is intentionally not parallel: the accounting table is
// process-global, so overlapping package tests would bleed into the counts.
func TestTxnAccountingCountsCommits(t *testing.T) {
	store := NewTestSqliteDB(t)

	executor := NewTransactionExecutor(
		store.BaseDB,
		func(tx *sql.Tx) *sqlc.Queries {
			return store.BaseDB.Queries.WithTx(tx)
		},
		store.log,
	)

	EnableTxnAccounting()
	ResetTxnAccounting()

	ctx := context.Background()
	noop := func(*sqlc.Queries) error { return nil }

	for i := 0; i < 2; i++ {
		require.NoError(
			t,
			executor.ExecTx(
				ctx, WriteTxOption(), noop,
			),
		)
	}
	require.NoError(t, executor.ExecTx(ctx, ReadTxOption(), noop))

	snapshot := TxnAccountingSnapshot()
	require.Len(t, snapshot, 2)

	var writes, reads uint64
	for _, bucket := range snapshot {
		require.NotEmpty(t, bucket.Caller)
		if bucket.ReadOnly {
			reads += bucket.Count
		} else {
			writes += bucket.Count
		}
	}
	require.Equal(t, uint64(2), writes)
	require.Equal(t, uint64(1), reads)

	ResetTxnAccounting()
	require.Empty(t, TxnAccountingSnapshot())
}
