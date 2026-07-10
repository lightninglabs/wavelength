package vtxo

import (
	"fmt"
	"slices"

	"github.com/lightninglabs/darepo-client/arkrpc"
)

// MaxAncestryPaths bounds the per-VTXO ancestry slice the indexer is
// allowed to return. Real cross-round multi-input OOR VTXOs see at most
// a handful of contributing commitments; the cap exists so a misbehaving
// or compromised indexer cannot force unbounded allocation here before
// the per-entry validation runs.
const MaxAncestryPaths = 64

// AncestryFromRPC converts a slice of arkrpc.AncestryPath into the typed
// vtxo.Ancestry shape used by descriptors and incoming-receive
// pipelines. Returns an error when the slice is empty (a VTXO without
// ancestry would persist as unexitable, so version-skew producers that
// still send the retired tree_path/tree_depth scalars must fail closed
// here rather than silently materialize a stranded descriptor) or when
// the slice exceeds MaxAncestryPaths.
//
// Lives in the vtxo package so both the OOR receive path (which routes
// through the durable QueryIncomingMetadataRequest outbox) and the
// in-round receive path (which materializes synchronously inside
// IncomingVTXOHandler from a thin IncomingVTXOEvent push) share one
// validator and one conversion. Drift between the two would silently
// produce non-exitable descriptors on one path but not the other —
// exactly the symptom of bug-3 in the working BUGS_FOUND.md.
func AncestryFromRPC(paths []*arkrpc.AncestryPath) ([]Ancestry, error) {
	if len(paths) == 0 {
		return nil, fmt.Errorf("indexer vtxo missing ancestry paths")
	}

	if len(paths) > MaxAncestryPaths {
		return nil, fmt.Errorf("indexer vtxo ancestry exceeds cap: "+
			"got %d, max %d", len(paths), MaxAncestryPaths)
	}

	out := make([]Ancestry, 0, len(paths))
	for i, p := range paths {
		if p == nil {
			continue
		}

		treePath, err := arkrpc.AncestryPathToTree(p)
		if err != nil {
			return nil, fmt.Errorf("path[%d] tree: %w", i, err)
		}

		commitmentTxID, err := arkrpc.AncestryCommitmentTxID(p)
		if err != nil {
			return nil, fmt.Errorf("path[%d] commitment: %w", i,
				err)
		}

		// Validate the indexer-supplied tree_depth against the
		// reconstructed path before it can be persisted. A zero or
		// truncated claim would otherwise survive the rest of the
		// receive-side checks and only fail at unilateral-exit time
		// (zero) or under-report the worst-case CSV window
		// (truncated), which is a fund-availability surface for
		// OOR-received VTXOs.
		err = arkrpc.ValidateAncestryPathDepth(
			p.GetTreeDepth(), treePath,
		)
		if err != nil {
			return nil, fmt.Errorf("path[%d] depth: %w", i, err)
		}

		out = append(out, Ancestry{
			TreePath:         treePath,
			CommitmentTxID:   commitmentTxID,
			InputIndices:     slices.Clone(p.GetInputIndices()),
			TreeDepth:        p.GetTreeDepth(),
			CommitmentHeight: p.GetCommitmentHeight(),
		})
	}

	return out, nil
}
