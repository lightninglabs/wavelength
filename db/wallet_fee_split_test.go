package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestWalletFeeSplitOwnership proves that retries keep the original signed
// candidate and that clearing a stale candidate cannot erase a new one.
func TestWalletFeeSplitOwnership(t *testing.T) {
	t.Parallel()
	store := NewTestDB(t)
	ctx := t.Context()
	first, second := []byte("first signed transaction"), []byte(
		"second signed transaction",
	)
	// A retry with identical bytes must acquire the same journal slot.
	for _, candidate := range [][]byte{first, first} {
		count, err := store.SaveWalletFeeSplit(ctx, candidate)
		require.NoError(t, err)
		require.EqualValues(t, 1, count)
	}
	// A competing candidate can neither replace nor clear the owner.
	count, err := store.SaveWalletFeeSplit(ctx, second)
	require.NoError(t, err)
	require.Zero(t, count)
	require.NoError(t, store.ClearWalletFeeSplit(ctx, second))
	saved, err := store.LoadWalletFeeSplit(ctx)
	require.NoError(t, err)
	require.Equal(t, first, saved)
	require.NoError(t, store.ClearWalletFeeSplit(ctx, first))
	_, err = store.LoadWalletFeeSplit(ctx)
	require.ErrorIs(t, err, sql.ErrNoRows)
}
