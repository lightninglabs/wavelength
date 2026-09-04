package vtxo

import (
	"bytes"
	"fmt"
	"slices"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/lib/arkscript"
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
		if p.GetCommitmentHeight() < 0 {
			return nil, fmt.Errorf("path[%d] commitment height must "+
				"not be negative", i)
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

		sweepDelay := p.GetCommitmentSweepDelay()
		rawSweepKey := p.GetCommitmentSweepKey()
		var sweepKey *btcec.PublicKey
		switch {
		case sweepDelay == 0 && len(rawSweepKey) == 0:
			// Older indexers omitted both additive fields. Conversion
			// remains possible, but AuthenticateBatchExpiry rejects the
			// missing evidence before any new descriptor is accepted.

		case sweepDelay == 0 || len(rawSweepKey) == 0:
			return nil, fmt.Errorf("path[%d] sweep key and delay must "+
				"both be provided", i)

		default:
			sweepKey, err = btcec.ParsePubKey(rawSweepKey)
			if err != nil {
				return nil, fmt.Errorf("path[%d] sweep key: %w", i, err)
			}

			sweepLeaf, err := arkscript.UnilateralCSVTimeoutTapLeaf(
				sweepKey, sweepDelay,
			)
			if err != nil {
				return nil, fmt.Errorf("path[%d] sweep leaf: %w", i, err)
			}
			sweepRoot := sweepLeaf.TapHash()
			if !bytes.Equal(
				sweepRoot[:], treePath.SweepTapscriptRoot,
			) {
				return nil, fmt.Errorf("path[%d] sweep key and delay do "+
					"not match committed sweep root", i)
			}
		}

		out = append(out, Ancestry{
			TreePath:             treePath,
			CommitmentTxID:       commitmentTxID,
			InputIndices:         slices.Clone(p.GetInputIndices()),
			TreeDepth:            p.GetTreeDepth(),
			CommitmentHeight:     p.GetCommitmentHeight(),
			CommitmentSweepDelay: sweepDelay,
			CommitmentSweepKey:   sweepKey,
		})
	}

	return out, nil
}
