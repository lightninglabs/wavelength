package types

import (
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/lib/tree"
)

// Ancestry describes one rooted commitment-tree fragment that contributes
// ancestry to a VTXO. A round-direct VTXO has exactly one entry; an OOR
// VTXO whose inputs descend from multiple commitment-tree paths has one
// entry per distinct (commitment tx, tree path) pair. Several entries may
// share a commitment txid: an OOR spend of two inputs that sit at
// different leaves of the same commitment tree carries one fragment per
// leaf, because each leaf needs its own root-to-leaf path for unilateral
// exit. The operator's indexer groups fragments the same way (one
// AncestryPath per batch-tree path within a commitment).
//
// Per-entry tree fragments are minimal extracted paths (root → leaf), not
// whole batch trees, so size scales with depth, not fan-out. The unroller
// must broadcast every fragment's transactions on-chain before the OOR
// chain can be claimed.
//
// This type lives in lib/types so that both round.ClientVTXO and
// vtxo.Descriptor can carry the same multi-fragment ancestry without an
// import cycle (vtxo already imports round).
type Ancestry struct {
	// TreePath is the extracted commitment-tree path from the batch root
	// down to the input VTXO leaf served by this fragment.
	TreePath *tree.Tree

	// CommitmentTxID is the txid of the commitment tx anchoring this
	// fragment. Multiple entries within one Descriptor may share a
	// commitment txid (different leaves of the same commitment tree),
	// but no two entries may carry the same (CommitmentTxID, TreePath)
	// pair.
	CommitmentTxID chainhash.Hash

	// InputIndices lists the Ark tx input indices (within the OOR Ark tx
	// that produced the VTXO) that this fragment serves. Empty for
	// round-direct VTXOs (which are not produced by an OOR Ark tx).
	InputIndices []uint32

	// TreeDepth is the depth of the served leaf within this fragment's
	// tree. Worst-case unilateral-exit timing for the produced VTXO is
	// max(TreeDepth) across all entries.
	TreeDepth uint32

	// CommitmentHeight is the on-chain confirmation height of the
	// commitment tx anchoring this fragment. Zero means unknown (legacy
	// persisted VTXOs, or an unconfirmed/not-yet-resolved commitment), in
	// which case callers fall back to a bounded lookback floor rather than
	// trusting this value.
	CommitmentHeight int32
}

// MaxAncestryTreeDepth returns the largest TreeDepth across the given
// ancestry slice. Returns 0 for an empty slice. Drives expiry timing
// decisions for callers that need worst-case unilateral-exit timing.
func MaxAncestryTreeDepth(ancestry []Ancestry) int {
	var deepest int
	for _, a := range ancestry {
		if int(a.TreeDepth) > deepest {
			deepest = int(a.TreeDepth)
		}
	}

	return deepest
}
