package actordelivery

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// TestExitBatchReceiptRecovery exercises the actual round mailbox's transaction
// boundary and reopens the wallet database before asking what was admitted.
func TestExitBatchReceiptRecovery(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	cfg := db.DefaultSqliteConfig(t.TempDir())
	clk := clock.NewDefaultClock()
	open := func() (*db.SqliteStore, *db.RoundPersistenceStore,
		actor.TxAwareDeliveryStore) {

		wallet, err := db.NewSqliteStore(cfg, btclog.Disabled)
		require.NoError(t, err)
		require.NoError(t, RunMigrations(wallet.DB, wallet.Backend()))
		domain := db.NewStore(
			wallet.DB, wallet.Queries, wallet.Backend(),
			btclog.Disabled,
		)
		mail, err := NewTxAwareDeliveryStoreFromDB(
			wallet.DB, wallet.Backend(), clk, btclog.Disabled,
		)
		require.NoError(t, err)

		return wallet, domain.NewRoundStore(
			&chaincfg.RegressionNetParams, clk,
		), mail
	}
	wallet, store, mail := open()
	enqueue := func(txCtx context.Context) error {
		return mail.EnqueueMessage(txCtx, actor.EnqueueParams{
			ID:          "leave",
			MailboxID:   "round-client",
			MessageType: "round.ClientCommand",
			Payload:     []byte("intent"),
			AvailableAt: time.Now(),
			MaxAttempts: 3,
		})
	}
	fault := errors.New("crash before commit")
	require.NoError(t, store.BeginExitBatch(ctx, "rolled-back"))
	require.NoError(t, store.BeginExitBatch(ctx, "lost-reply"))
	commit := func(id string, fail bool) error {
		return mail.ExecTx(
			ctx, false,
			func(txCtx context.Context,
				tx actor.DeliveryStore) error {

				if err := store.CommitExitBatch(
					txCtx, id, []byte("original"),
				); err != nil {
					return err
				}
				if err := enqueue(txCtx); err != nil {
					return err
				}
				if fail {
					return fault
				}

				return nil
			},
		)
	}
	err := commit("rolled-back", true)
	require.ErrorIs(t, err, fault)
	msg, err := mail.PeekNextMessage(ctx, "round-client")
	require.NoError(t, err)
	require.Nil(t, msg)
	require.NoError(t, commit("lost-reply", false))
	require.ErrorIs(
		t, store.BeginExitBatch(ctx, "lost-reply"),
		db.ErrExitBatchIDUsed,
	)
	require.NoError(t, wallet.Close())

	wallet, store, mail = open()
	t.Cleanup(func() { require.NoError(t, wallet.Close()) })
	receipt, err := store.ResolveExitBatch(ctx, "lost-reply")
	require.NoError(t, err)
	require.Equal(t, []byte("original"), receipt)
	msg, err = mail.PeekNextMessage(ctx, "round-client")
	require.NoError(t, err)
	require.NotNil(t, msg)
	require.Equal(t, "leave", msg.ID)
	for _, id := range []string{"rolled-back", "never-arrived"} {
		receipt, err := store.ResolveExitBatch(ctx, id)
		require.NoError(t, err)
		require.Empty(t, receipt)
		require.ErrorIs(
			t,
			store.CommitExitBatch(
				ctx, id, []byte("late"),
			),
			db.ErrExitBatchIDUsed,
		)
	}
}
