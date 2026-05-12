package indexer

import (
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo/rounds"
)

// NewTestLineageResolver creates a lineageResolver for testing. The
// optional roundRowByID map lets callers pre-populate round metadata
// in the resolver's cache.
func NewTestLineageResolver(store Store,
	roundRowByID map[rounds.RoundID]*RoundRow) *lineageResolver {

	return newLineageResolver(store, roundRowByID)
}

// TestLineageResolver is an alias for the unexported lineageResolver
// type so external tests can hold references.
type TestLineageResolver = lineageResolver

// TestVTXOLineage is an alias for the unexported vtxoLineage type so
// external tests can inspect results.
type TestVTXOLineage = vtxoLineage

// NewTestVTXOLineage constructs a vtxoLineage for testing
// applyLineageMetadata. The single-tree shape produces a length-1
// ancestryPaths slice; multi-tree fixtures use the lower-level
// AppendTestAncestryFragment helper.
func NewTestVTXOLineage(
	roundID string,
	commitmentTxID chainhash.Hash,
	batchExpiry int32,
	relativeExpiry uint32,
	treeDepth int,
	chainDepth int,
	createdHeight int32,
	treePath *tree.Tree,
	treePathTLV []byte,
) *vtxoLineage {

	lineage := &vtxoLineage{
		roundID:        roundID,
		commitmentTxID: commitmentTxID,
		batchExpiry:    batchExpiry,
		relativeExpiry: relativeExpiry,
		chainDepth:     chainDepth,
		createdHeight:  createdHeight,
	}

	if treePath != nil || len(treePathTLV) > 0 {
		lineage.ancestryPaths = []ancestryFragment{{
			treePath:       treePath,
			treePathTLV:    treePathTLV,
			commitmentTxID: commitmentTxID,
			treeDepth:      treeDepth,
		}}
	}

	return lineage
}

// AppendTestAncestryFragment appends a synthetic ancestryFragment to a
// vtxoLineage for cross-commitment multi-input test fixtures.
func AppendTestAncestryFragment(l *vtxoLineage, commitmentTxID chainhash.Hash,
	treePath *tree.Tree, treePathTLV []byte, inputIndices []uint32,
	treeDepth int) {

	l.ancestryPaths = append(l.ancestryPaths, ancestryFragment{
		treePath:       treePath,
		treePathTLV:    treePathTLV,
		commitmentTxID: commitmentTxID,
		inputIndices:   append([]uint32(nil), inputIndices...),
		treeDepth:      treeDepth,
	})
}

// LineageRoundID returns the round ID from a vtxoLineage.
func LineageRoundID(l *vtxoLineage) string {
	return l.roundID
}

// LineageCommitmentTxID returns the commitment txid from a vtxoLineage.
func LineageCommitmentTxID(l *vtxoLineage) chainhash.Hash {
	return l.commitmentTxID
}

// LineageBatchExpiry returns the batch expiry from a vtxoLineage.
func LineageBatchExpiry(l *vtxoLineage) int32 {
	return l.batchExpiry
}

// LineageRelativeExpiry returns the relative expiry from a vtxoLineage.
func LineageRelativeExpiry(l *vtxoLineage) uint32 {
	return l.relativeExpiry
}

// LineageTreeDepth returns the max tree depth across the ancestry fragments.
func LineageTreeDepth(l *vtxoLineage) int {
	var deepest int
	for _, f := range l.ancestryPaths {
		if f.treeDepth > deepest {
			deepest = f.treeDepth
		}
	}

	return deepest
}

// LineageChainDepth returns the chain depth from a vtxoLineage.
func LineageChainDepth(l *vtxoLineage) int {
	return l.chainDepth
}

// LineageCreatedHeight returns the created height from a vtxoLineage.
func LineageCreatedHeight(l *vtxoLineage) int32 {
	return l.createdHeight
}

// LineageTreePath returns the primary ancestry tree path (first
// fragment), preserving the legacy single-tree-only test shape. Tests
// targeting cross-commitment multi-input lineages should use
// LineageAncestryPathsLen / LineageAncestryFragmentTreePath instead.
func LineageTreePath(l *vtxoLineage) *tree.Tree {
	if len(l.ancestryPaths) == 0 {
		return nil
	}

	return l.ancestryPaths[0].treePath
}

// LineageTreePathTLV returns the primary ancestry fragment's TLV bytes.
func LineageTreePathTLV(l *vtxoLineage) []byte {
	if len(l.ancestryPaths) == 0 {
		return nil
	}

	return l.ancestryPaths[0].treePathTLV
}

// LineageAncestryPathsLen returns the number of ancestry fragments
// captured by the lineage. Useful for asserting multi-tree resolution.
func LineageAncestryPathsLen(l *vtxoLineage) int {
	return len(l.ancestryPaths)
}

// LineageAncestryFragmentTreePath returns the i-th fragment's tree path.
func LineageAncestryFragmentTreePath(l *vtxoLineage, i int) *tree.Tree {
	if i < 0 || i >= len(l.ancestryPaths) {
		return nil
	}

	return l.ancestryPaths[i].treePath
}

// LineageAncestryFragmentCommitmentTxID returns the i-th fragment's
// commitment txid.
func LineageAncestryFragmentCommitmentTxID(l *vtxoLineage,
	i int) chainhash.Hash {

	if i < 0 || i >= len(l.ancestryPaths) {
		return chainhash.Hash{}
	}

	return l.ancestryPaths[i].commitmentTxID
}

// ApplyLineageMetadata wraps the unexported applyLineageMetadata for
// external test use.
func ApplyLineageMetadata(out *arkrpc.VTXO, lineage *vtxoLineage) error {
	return applyLineageMetadata(out, lineage)
}

// LineageByOutpoint returns the resolver's internal lineage cache for
// test inspection.
func LineageByOutpoint(r *lineageResolver) map[string]*vtxoLineage {
	return r.lineageByOutpoint
}

// TxVBytes wraps the unexported txVBytes helper for cross-package
// test use. Returns the witness-discounted virtual size of a signed
// transaction.
func TxVBytes(tx *wire.MsgTx) int {
	return txVBytes(tx)
}
