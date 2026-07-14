package db

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/stretchr/testify/require"
)

// TestTransactionExecutorUsesContextTx verifies that ExecTx participates in an
// existing actor transaction when present in context.
func TestTransactionExecutorUsesContextTx(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	db := NewTestDB(t)

	txExec := NewTransactionExecutor(
		db.BaseDB,
		func(tx *sql.Tx) *sqlc.Queries {
			return db.WithTx(tx)
		},
		btclog.Disabled,
	)

	outerTx, err := db.BeginTx(ctx, WriteTxOption())
	require.NoError(t, err)

	txCtx := actor.WithTx(ctx, outerTx)
	chainName := "ctx-tx-chain"

	err = txExec.ExecTx(
		txCtx, WriteTxOption(), func(q *sqlc.Queries) error {
			params := sqlc.UpsertChainInfoParams{
				ID:        1,
				ChainName: chainName,
				GenesisHash: []byte{
					0x01,
				},
			}

			return q.UpsertChainInfo(ctx, params)
		},
	)
	require.NoError(t, err)

	// Rolling back the outer transaction should remove all writes done via
	// ExecTx, proving that the executor joined the actor transaction.
	require.NoError(t, outerTx.Rollback())

	_, err = db.GetChainInfo(ctx, chainName)
	require.Error(t, err)
	require.True(t, errors.Is(err, sql.ErrNoRows))
}
