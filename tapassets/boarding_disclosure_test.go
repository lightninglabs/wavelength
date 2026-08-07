package tapassets

import (
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/mssmt"
	"github.com/stretchr/testify/require"
)

// TestCommitmentLeafHash proves the hash-only reconstruction reproduces
// the library's own leaf for a computed root, and varies with both
// inputs.
func TestCommitmentLeafHash(t *testing.T) {
	t.Parallel()

	assetRoot := chainhash.HashH([]byte("commitment root"))
	const amount = 1_500

	want := commitment.NewTapCommitmentWithRoot(
		commitment.TapCommitmentV2,
		mssmt.NewComputedBranch(mssmt.NodeHash(assetRoot), amount),
	).TapLeaf().TapHash()

	got := CommitmentLeafHash(assetRoot, amount)
	require.Equal(t, chainhash.Hash(want), got)

	require.NotEqual(t, got, CommitmentLeafHash(assetRoot, amount+1))
	otherRoot := chainhash.HashH([]byte("other root"))
	require.NotEqual(t, got, CommitmentLeafHash(otherRoot, amount))
}
