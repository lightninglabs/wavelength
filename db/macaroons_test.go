package db

import (
	"context"
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/stretchr/testify/require"
)

// TestMacaroonRootKeyStorePersistsRootKey verifies macaroon root keys are
// persisted in the daemon DB and reused for the same key ID.
func TestMacaroonRootKeyStorePersistsRootKey(t *testing.T) {
	t.Parallel()

	sqlDB := NewTestDB(t)
	store := NewStore(
		sqlDB.DB, sqlDB.Queries, sqlDB.Backend(), btclog.Disabled,
	)
	rootKeyStore := store.NewMacaroonRootKeyStore()

	ctx := macaroons.ContextWithRootKeyID(
		context.Background(), macaroons.DefaultRootKeyID,
	)
	rootKey, id, err := rootKeyStore.RootKey(ctx)
	require.NoError(t, err)
	require.Equal(t, macaroons.DefaultRootKeyID, id)
	require.Len(t, rootKey, macaroons.RootKeyLen)

	rootKeyAgain, idAgain, err := rootKeyStore.RootKey(ctx)
	require.NoError(t, err)
	require.Equal(t, id, idAgain)
	require.Equal(t, rootKey, rootKeyAgain)

	fetched, err := rootKeyStore.Get(context.Background(), id)
	require.NoError(t, err)
	require.Equal(t, rootKey, fetched)
}
