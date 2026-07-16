package waved

import (
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/stretchr/testify/require"
)

// recoveryTestFragment builds one ancestry fragment anchored at commit
// whose tree path is differentiated by the supplied batch outpoint
// index, standing in for distinct leaf paths within one commitment
// tree.
func recoveryTestFragment(commit chainhash.Hash,
	batchIndex uint32) vtxo.Ancestry {

	return vtxo.Ancestry{
		TreePath: &tree.Tree{
			Root: &tree.Node{},
			BatchOutpoint: wire.OutPoint{
				Hash:  commit,
				Index: batchIndex,
			},
		},
		CommitmentTxID: commit,
		TreeDepth:      1,
	}
}

// TestRecoveryAncestryDedupKeepsSameCommitmentLeaves verifies that
// recoveryAncestry deduplicates fragments by their full (commitment
// txid, tree path) identity: an exact duplicate shared by two roots
// collapses to one entry, while two fragments anchored at the same
// commitment but serving different leaves both survive. Deduplicating
// on the commitment txid alone would silently drop the second leaf's
// path and leave the synthesized recovery target unable to drive a
// complete unilateral exit (wavelength#969).
func TestRecoveryAncestryDedupKeepsSameCommitmentLeaves(t *testing.T) {
	t.Parallel()

	commit := chainhash.Hash{0xaa}

	shared := recoveryTestFragment(commit, 0)
	otherLeaf := recoveryTestFragment(commit, 1)

	roots := []*vtxo.Descriptor{
		{
			Ancestry: []vtxo.Ancestry{
				shared,
			},
		},
		nil,
		{
			// The shared fragment repeats across roots and must
			// collapse; the same-commitment other-leaf fragment
			// must survive.
			Ancestry: []vtxo.Ancestry{
				shared,
				otherLeaf,
			},
		},
	}

	ancestry, err := recoveryAncestry(roots)
	require.NoError(t, err)
	require.Len(t, ancestry, 2)
	require.Equal(t, commit, ancestry[0].CommitmentTxID)
	require.Equal(t, commit, ancestry[1].CommitmentTxID)
	require.NotEqual(
		t, ancestry[0].TreePath.BatchOutpoint,
		ancestry[1].TreePath.BatchOutpoint,
	)
}
