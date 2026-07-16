package waved

import (
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	lib_tree "github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/stretchr/testify/require"
)

// TestRecoveryAncestryPreservesSharedCommitmentLeaves is a regression for
// wavelength#969. recoveryAncestry aggregates the ancestry fragments of a
// recovery target's roots. When two roots both descend from the same
// commitment tx they contribute distinct leaves (each its own root->leaf
// path), and every fragment is required to prove the target on-chain. The
// previous commitment-keyed de-duplication silently dropped the second
// same-commitment fragment, so the synthesized recovery target could not
// unilaterally exit that input. Every fragment must survive.
func TestRecoveryAncestryPreservesSharedCommitmentLeaves(t *testing.T) {
	t.Parallel()

	commit := chainhash.Hash{0x0b, 0x17}

	frag := func(input uint32) vtxo.Ancestry {
		return vtxo.Ancestry{
			TreePath: &lib_tree.Tree{
				Root: &lib_tree.Node{},
				BatchOutpoint: wire.OutPoint{
					Hash: commit,
				},
			},
			CommitmentTxID: commit,
			InputIndices: []uint32{
				input,
			},
			TreeDepth: 1,
		}
	}

	// Two roots anchored at the same commitment but at different leaves --
	// the shape of change from a send that consumed two coins from one
	// round.
	rootA := &vtxo.Descriptor{Ancestry: []vtxo.Ancestry{frag(0)}}
	rootB := &vtxo.Descriptor{Ancestry: []vtxo.Ancestry{frag(1)}}

	got := recoveryAncestry([]*vtxo.Descriptor{rootA, rootB})
	require.Len(t, got, 2, "both same-commitment leaves must survive")
	require.Equal(t, commit, got[0].CommitmentTxID)
	require.Equal(t, commit, got[1].CommitmentTxID)

	// A nil root is skipped without panicking or dropping real fragments.
	require.Len(
		t,
		recoveryAncestry(
			[]*vtxo.Descriptor{rootA, nil, rootB},
		),
		2,
	)
}
