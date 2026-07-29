package types

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
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
	// TreePath is either the extracted commitment-tree path from the batch
	// root down to the input VTXO leaf, or an explicit external-root
	// sentinel. The sentinel is a non-nil tree with Root == nil and exact
	// BatchOutpoint/BatchOutput fields describing an already-confirmed
	// direct VTXO. A nil TreePath is always malformed.
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

// IsExternalRoot reports whether this ancestry entry is the explicit
// direct-on-chain variant. A non-nil rootless TreePath is reserved for that
// variant; a nil TreePath remains ordinary missing ancestry and is never
// interpreted as an external root.
func (a Ancestry) IsExternalRoot() bool {
	return a.TreePath != nil && a.TreePath.Root == nil
}

// ValidateExternalRoot validates the strict rootless ancestry sentinel. The
// sentinel carries no recovery transaction of its own: BatchOutpoint is the
// already-confirmed external VTXO, while BatchOutput is the authoritative
// output the first checkpoint spends.
func (a Ancestry) ValidateExternalRoot() error {
	switch {
	case !a.IsExternalRoot():
		return fmt.Errorf("ancestry is not an external root")

	case a.CommitmentTxID == (chainhash.Hash{}):
		return fmt.Errorf("external root commitment txid is zero")

	case a.TreePath.BatchOutpoint.Hash != a.CommitmentTxID:
		return fmt.Errorf("external root outpoint hash %s does not "+
			"match commitment %s", a.TreePath.BatchOutpoint.Hash,
			a.CommitmentTxID)

	case a.TreePath.BatchOutput == nil:
		return fmt.Errorf("external root output is missing")

	case a.TreePath.BatchOutput.Value <= 0:
		return fmt.Errorf("external root output value %d must be "+
			"positive", a.TreePath.BatchOutput.Value)

	case len(a.TreePath.BatchOutput.PkScript) == 0:
		return fmt.Errorf("external root output script is empty")

	case len(a.TreePath.SweepTapscriptRoot) != 0:
		return fmt.Errorf("external root must not carry a sweep root")

	case a.TreeDepth != 0:
		return fmt.Errorf("external root tree depth must be "+
			"zero, got %d", a.TreeDepth)

	case a.CommitmentHeight <= 0:
		return fmt.Errorf("external root confirmation height must be "+
			"positive, got %d", a.CommitmentHeight)

	case len(a.InputIndices) == 0:
		return fmt.Errorf("external root input indices are empty")
	}

	return nil
}

// ExternalRootOutpoint returns the exact direct-on-chain root outpoint. The
// boolean is false for ordinary tree ancestry.
func (a Ancestry) ExternalRootOutpoint() (wire.OutPoint, bool) {
	if !a.IsExternalRoot() {
		return wire.OutPoint{}, false
	}

	return a.TreePath.BatchOutpoint, true
}

// ExternalRootOutputMatches reports whether output is byte-identical to the
// authoritative external output carried by this entry.
func (a Ancestry) ExternalRootOutputMatches(output *wire.TxOut) bool {
	if !a.IsExternalRoot() || a.TreePath.BatchOutput == nil ||
		output == nil {
		return false
	}

	return a.TreePath.BatchOutput.Value == output.Value &&
		bytes.Equal(
			a.TreePath.BatchOutput.PkScript, output.PkScript,
		)
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
