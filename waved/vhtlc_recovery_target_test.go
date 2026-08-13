package waved

import (
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/stretchr/testify/require"
)

// TestReceiveClaimRecoveryDescriptorBindsCommitments proves peer and indexer
// paths may have different minimal-tree encodings while remaining anchored to
// the same immutable commitment set.
func TestReceiveClaimRecoveryDescriptorBindsCommitments(t *testing.T) {
	t.Parallel()

	localTree, _ := sourceWatcherTree(t, 44)
	peerTree, _ := sourceWatcherTree(t, 45)
	encodedPeerTree, err := db.SerializeTree(peerTree)
	require.NoError(t, err)
	commitment := chainhash.Hash{46}
	desc := &vtxo.Descriptor{
		RoundID: "round", CommitmentTxID: commitment,
		BatchExpiry: 500, ChainDepth: 1, CreatedHeight: 121,
		Ancestry: []vtxo.Ancestry{{
			TreePath: localTree, CommitmentTxID: commitment,
			InputIndices: []uint32{
				0,
			}, TreeDepth: 1,
			CommitmentHeight: 121,
		}},
	}
	recovery := arkchannel.RecoveryPackage{
		Descriptor: arkchannel.RecoveryDescriptor{
			RoundID: "round", CommitmentTxID: commitment,
			BatchExpiry: 500, ChainDepth: 1, CreatedHeight: 121,
			Ancestry: []arkchannel.RecoveryAncestry{{
				TreePath:       encodedPeerTree,
				CommitmentTxID: commitment,
				InputIndices: []uint32{
					0,
				}, TreeDepth: 1,
			}},
		},
	}

	require.NoError(
		t, validateReceiveClaimRecoveryDescriptor(desc, recovery),
	)
	recovery.Descriptor.Ancestry[0].CommitmentTxID = chainhash.Hash{47}
	require.ErrorContains(
		t, validateReceiveClaimRecoveryDescriptor(desc, recovery),
		"commitments do not match indexer",
	)
}

// TestReceiveClaimRecoveryRootsBindsExactIndexerOutput proves the transported
// package graph terminates at an exact output present in indexed round
// ancestry, independent of the peer's own tree serialization.
func TestReceiveClaimRecoveryRootsBindsExactIndexerOutput(t *testing.T) {
	t.Parallel()

	treePath, root := sourceWatcherTree(t, 48)
	arkTx := wire.NewMsgTx(2)
	arkTx.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{
		Hash: chainhash.Hash{49},
	}})
	arkTx.AddTxOut(&wire.TxOut{Value: 50_000, PkScript: []byte{0x51}})
	arkPSBT, err := psbt.NewFromUnsignedTx(arkTx)
	require.NoError(t, err)
	checkpointTx := wire.NewMsgTx(2)
	checkpointTx.AddTxIn(&wire.TxIn{PreviousOutPoint: root})
	checkpointTx.AddTxOut(&wire.TxOut{Value: 49_000,
		PkScript: []byte{0x51}})
	checkpoint, err := psbt.NewFromUnsignedTx(checkpointTx)
	require.NoError(t, err)
	sessionID := arkTx.TxHash()
	packages := []parsedRecoveryPackage{{
		entry: arkchannel.RecoveryOORPackage{
			SessionID: sessionID,
		},
		arkPSBT: arkPSBT, checkpoints: []*psbt.Packet{
			checkpoint,
		},
	}}

	roots, err := receiveClaimRecoveryRoots(sessionID, packages)
	require.NoError(t, err)
	require.Equal(t, []wire.OutPoint{root}, roots)
	require.NoError(
		t,
		validateRecoveryPackageRoots(
			[]vtxo.Ancestry{
				{TreePath: treePath},
			},
			roots,
		),
	)
	roots[0].Index++
	require.ErrorContains(
		t,
		validateRecoveryPackageRoots(
			[]vtxo.Ancestry{
				{TreePath: treePath},
			},
			roots,
		),
		"missing root",
	)
}

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
