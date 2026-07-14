package actordelivery

import (
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

func TestNewTxAwareDeliveryStoreFromDBSQLite(t *testing.T) {
	t.Parallel()

	rawDB := newSQLiteDB(t)
	err := RunMigrations(rawDB, sqlc.BackendTypeSqlite)
	require.NoError(t, err)

	store, err := NewTxAwareDeliveryStoreFromDB(
		rawDB, sqlc.BackendTypeSqlite,
		clock.NewTestClock(
			time.Now(),
		),
		btclog.Disabled,
	)
	require.NoError(t, err)

	ctx := t.Context()
	err = store.EnqueueMessage(ctx, actor.EnqueueParams{
		ID:          "msg-1",
		MailboxID:   "actor-1",
		MessageType: "test.Msg",
		Payload:     []byte{1, 2, 3},
		Priority:    1,
		AvailableAt: time.Now().Add(-time.Second),
		MaxAttempts: 3,
	})
	require.NoError(t, err)

	leased, err := store.LeaseNextMessage(
		ctx, "actor-1", "lease-token-1", 30*time.Second,
	)
	require.NoError(t, err)
	require.NotNil(t, leased)
	require.Equal(t, "msg-1", leased.ID)

	rows, err := store.AckMessage(ctx, "msg-1", "lease-token-1")
	require.NoError(t, err)
	require.EqualValues(t, 1, rows)
}

func TestNewTxAwareDeliveryStoreFromDBUnsupportedBackend(t *testing.T) {
	t.Parallel()

	rawDB := newSQLiteDB(t)

	_, err := NewTxAwareDeliveryStoreFromDB(
		rawDB, sqlc.BackendTypeUnknown, clock.NewDefaultClock(),
		btclog.Disabled,
	)
	require.ErrorContains(t, err, "unsupported backend")
}
