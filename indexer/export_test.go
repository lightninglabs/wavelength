package indexer

import (
	"github.com/btcsuite/btcd/chaincfg/chainhash"
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
// applyLineageMetadata.
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

	return &vtxoLineage{
		roundID:        roundID,
		commitmentTxID: commitmentTxID,
		batchExpiry:    batchExpiry,
		relativeExpiry: relativeExpiry,
		treeDepth:      treeDepth,
		chainDepth:     chainDepth,
		createdHeight:  createdHeight,
		treePath:       treePath,
		treePathTLV:    treePathTLV,
	}
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

// LineageTreeDepth returns the tree depth from a vtxoLineage.
func LineageTreeDepth(l *vtxoLineage) int {
	return l.treeDepth
}

// LineageChainDepth returns the chain depth from a vtxoLineage.
func LineageChainDepth(l *vtxoLineage) int {
	return l.chainDepth
}

// LineageCreatedHeight returns the created height from a vtxoLineage.
func LineageCreatedHeight(l *vtxoLineage) int32 {
	return l.createdHeight
}

// LineageTreePath returns the tree path from a vtxoLineage.
func LineageTreePath(l *vtxoLineage) *tree.Tree {
	return l.treePath
}

// LineageTreePathTLV returns the TLV-encoded tree path bytes.
func LineageTreePathTLV(l *vtxoLineage) []byte {
	return l.treePathTLV
}

// ApplyLineageMetadata wraps the unexported applyLineageMetadata for
// external test use.
func ApplyLineageMetadata(out *arkrpc.VTXO,
	lineage *vtxoLineage) error {

	return applyLineageMetadata(out, lineage)
}

// LineageByOutpoint returns the resolver's internal lineage cache for
// test inspection.
func LineageByOutpoint(
	r *lineageResolver) map[string]*vtxoLineage {

	return r.lineageByOutpoint
}
