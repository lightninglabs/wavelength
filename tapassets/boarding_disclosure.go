package tapassets

import (
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/mssmt"
)

// CommitmentLeafHash reconstructs the V2 Taproot Asset commitment leaf
// of a single-asset anchor from its commitment root hash and amount.
// The boarding disclosure carries it as the composed output's tapscript
// sibling; the composed-script recompute authenticates it against the
// on-chain script.
func CommitmentLeafHash(assetRoot chainhash.Hash,
	amount uint64) chainhash.Hash {

	root := mssmt.NewComputedBranch(mssmt.NodeHash(assetRoot), amount)
	tapCommitment := commitment.NewTapCommitmentWithRoot(
		commitment.TapCommitmentV2, root,
	)

	return chainhash.Hash(tapCommitment.TapLeaf().TapHash())
}
