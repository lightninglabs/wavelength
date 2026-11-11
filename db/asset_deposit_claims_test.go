package db

import (
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/stretchr/testify/require"
)

// TestAssetDepositClaimLifecycle checks that a deposit holds one claim, that
// recording it again is a no-op, and that marking it claimed is idempotent.
func TestAssetDepositClaimLifecycle(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newTestAssetDepositStore(t)

	_, err := store.GetAssetDepositClaim(ctx, "alice-1")
	require.ErrorIs(t, err, ErrAssetDepositClaimNotFound)
	require.ErrorIs(
		t, store.MarkAssetDepositClaimed(ctx, "alice-1"),
		ErrAssetDepositClaimNotFound,
	)

	_, err = store.CreateAssetDeposit(
		ctx, testAssetDepositRequest(t, "alice-1"),
	)
	require.NoError(t, err)

	txid := chainhash.HashH([]byte("claim"))
	claim, err := store.CreateAssetDepositClaim(ctx, "alice-1", txid)
	require.NoError(t, err)
	require.Equal(t, txid, claim.ClaimTxid)
	require.Equal(t, AssetDepositClaiming, claim.Status)

	_, err = store.CreateAssetDepositClaim(ctx, "alice-1", txid)
	require.NoError(t, err)
	_, err = store.CreateAssetDepositClaim(
		ctx, "alice-1",
		chainhash.HashH(
			[]byte("other"),
		),
	)
	require.ErrorIs(t, err, ErrAssetDepositMismatch)

	pending, err := store.ListAssetDepositClaimsByStatus(
		ctx, AssetDepositClaiming,
	)
	require.NoError(t, err)
	require.Len(t, pending, 1)

	require.NoError(t, store.MarkAssetDepositClaimed(ctx, "alice-1"))
	require.NoError(t, store.MarkAssetDepositClaimed(ctx, "alice-1"))
	claim, err = store.GetAssetDepositClaim(ctx, "alice-1")
	require.NoError(t, err)
	require.Equal(t, AssetDepositClaimed, claim.Status)

	pending, err = store.ListAssetDepositClaimsByStatus(
		ctx, AssetDepositClaiming,
	)
	require.NoError(t, err)
	require.Empty(t, pending)
}
