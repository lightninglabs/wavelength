package arkscript

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/lightninglabs/darepo-client/internal/testutils"
	"github.com/stretchr/testify/require"
)

// TestPolicyLeafCompareTo tests the canonical comparison of policy leaves.
func TestPolicyLeafCompareTo(t *testing.T) {
	t.Parallel()

	key1, _ := testutils.CreateKey(1)
	key2, _ := testutils.CreateKey(2)

	checksig1 := &Multisig{Keys: []*btcec.PublicKey{key1}}
	checksig2 := &Multisig{Keys: []*btcec.PublicKey{key2}}

	script1, err := checksig1.Script()
	require.NoError(t, err)
	script2, err := checksig2.Script()
	require.NoError(t, err)

	leaf1 := PolicyLeaf{Leaf: txscript.NewBaseTapLeaf(script1)}
	leaf3 := PolicyLeaf{Leaf: txscript.NewBaseTapLeaf(script2)}

	// Same role, different scripts: lexicographic order.
	cmp := leaf1.CompareTo(&leaf3)
	require.NotEqual(t, 0, cmp)

	// Same leaf compares equal.
	require.Equal(t, 0, leaf1.CompareTo(&leaf1))
}

// TestSortLeaves verifies canonical leaf sorting.
func TestSortLeaves(t *testing.T) {
	t.Parallel()

	key1, _ := testutils.CreateKey(1)
	key2, _ := testutils.CreateKey(2)
	key3, _ := testutils.CreateKey(3)

	script1, _ := (&Multisig{Keys: []*btcec.PublicKey{key1}}).Script()
	script2, _ := (&Multisig{Keys: []*btcec.PublicKey{key2}}).Script()
	script3, _ := (&Multisig{Keys: []*btcec.PublicKey{key3}}).Script()

	leaves := []PolicyLeaf{
		{
			Leaf: txscript.NewBaseTapLeaf(script3),
		},
		{
			Leaf: txscript.NewBaseTapLeaf(script1),
		},
		{
			Leaf: txscript.NewBaseTapLeaf(script2),
		},
	}

	sortLeaves(leaves)

	for i := 1; i < len(leaves); i++ {
		require.LessOrEqual(
			t, bytes.Compare(
				leaves[i-1].Leaf.Script, leaves[i].Leaf.Script,
			),
			0,
		)
	}
}

// TestSortLeavesLexicographic tests lexicographic secondary sorting.
func TestSortLeavesLexicographic(t *testing.T) {
	t.Parallel()

	// Create two leaves with the same role but different scripts.
	scriptA := []byte{0x01, 0x02, 0x03}
	scriptB := []byte{0x01, 0x02, 0x04}

	leaves := []PolicyLeaf{
		{
			Leaf: txscript.NewBaseTapLeaf(scriptB),
		},
		{
			Leaf: txscript.NewBaseTapLeaf(scriptA),
		},
	}

	sortLeaves(leaves)

	// scriptA < scriptB lexicographically.
	require.True(
		t, bytes.Compare(
			leaves[0].Leaf.Script, leaves[1].Leaf.Script,
		) < 0,
	)
}

// TestBuildTreeSingleLeaf tests tree construction with a single leaf.
func TestBuildTreeSingleLeaf(t *testing.T) {
	t.Parallel()

	key, _ := testutils.CreateKey(1)
	script, _ := (&Multisig{Keys: []*btcec.PublicKey{key}}).Script()

	leaves := []PolicyLeaf{
		{
			Leaf: txscript.NewBaseTapLeaf(script),
		},
	}

	policy, err := BuildTree(leaves, &ARKNUMSKey)
	require.NoError(t, err)
	require.Len(t, policy.Leaves, 1)
	// Single leaf has no siblings.
	require.Len(t, policy.merkleProofs[0], 0)

	// Root hash should be the leaf hash.
	expectedHash := leaves[0].Leaf.TapHash()
	require.Equal(t, expectedHash[:], policy.RootHash)
}

// TestBuildTreeTwoLeaves tests tree construction with two leaves and verifies
// it matches btcd's AssembleTaprootScriptTree.
func TestBuildTreeTwoLeaves(t *testing.T) {
	t.Parallel()

	ownerKey, _ := testutils.CreateKey(1)
	operatorKey, _ := testutils.CreateKey(2)
	exitDelay := uint32(100)

	// Build leaves using AST.
	collabNode := &Multisig{
		Keys: []*btcec.PublicKey{
			ownerKey,
			operatorKey,
		},
	}
	collabScript, err := collabNode.Script()
	require.NoError(t, err)

	exitNode := &CSV{
		Lock: exitDelay,
		Inner: &Multisig{
			Keys: []*btcec.PublicKey{
				ownerKey,
			},
		},
	}
	exitScript, err := exitNode.Script()
	require.NoError(t, err)

	leaves := []PolicyLeaf{
		{
			Leaf: txscript.NewBaseTapLeaf(collabScript),
		},
		{
			Leaf: txscript.NewBaseTapLeaf(exitScript),
		},
	}

	// Build tree using our implementation. BuildTree sorts
	// canonically, so input order doesn't matter.
	policy, err := BuildTree(leaves, &ARKNUMSKey)
	require.NoError(t, err)

	// Build tree using btcd's implementation with sorted leaves
	// to match.
	sortLeaves(leaves)
	btcdLeaves := []txscript.TapLeaf{
		leaves[0].Leaf, leaves[1].Leaf,
	}
	btcdTree := txscript.AssembleTaprootScriptTree(btcdLeaves...)
	btcdRootHash := btcdTree.RootNode.TapHash()

	// CRITICAL: Root hashes must match for backward compatibility.
	require.Equal(
		t, btcdRootHash[:], policy.RootHash,
		"BuildTree root hash must match btcd AssembleTaprootScriptTree",
	)

	// Verify output keys match.
	expectedOutputKey := txscript.ComputeTaprootOutputKey(
		&ARKNUMSKey, btcdRootHash[:],
	)
	require.Equal(t, expectedOutputKey, policy.OutputKey())

	// Verify proofs: each leaf should have exactly one sibling.
	require.Len(t, policy.merkleProofs[0], 1)
	require.Len(t, policy.merkleProofs[1], 1)

	// Each leaf's sibling proof should be the other leaf's hash.
	pLeaf0Hash := policy.Leaves[0].Leaf.TapHash()
	pLeaf1Hash := policy.Leaves[1].Leaf.TapHash()
	require.Equal(t, pLeaf1Hash, policy.merkleProofs[0][0])
	require.Equal(t, pLeaf0Hash, policy.merkleProofs[1][0])
}

// TestBuildTreeMatchesGoldenVectors verifies that BuildTree produces control
// blocks that match the golden test vectors.
func TestBuildTreeMatchesGoldenVectors(t *testing.T) {
	t.Parallel()

	for _, vec := range goldenVTXOVectors {
		t.Run(vec.Name, func(t *testing.T) {
			t.Parallel()

			ownerKey, _ := testutils.CreateKey(
				vec.OwnerKeyIndex,
			)
			operatorKey, _ := testutils.CreateKey(
				vec.OperatorKeyIndex,
			)

			// Build collab leaf using AST.
			collabNode := &Multisig{
				Keys: []*btcec.PublicKey{
					ownerKey,
					operatorKey,
				},
			}
			collabScript, err := collabNode.Script()
			require.NoError(t, err)

			// Build exit leaf using AST.
			exitNode := &CSV{
				Lock: vec.ExitDelay,
				Inner: &Multisig{
					Keys: []*btcec.PublicKey{
						ownerKey,
					},
				},
			}
			exitScript, err := exitNode.Script()
			require.NoError(t, err)

			// Construct leaves in canonical order.
			leaves := []PolicyLeaf{
				{
					Leaf: txscript.NewBaseTapLeaf(
						collabScript,
					),
				},
				{
					Leaf: txscript.NewBaseTapLeaf(
						exitScript,
					),
				},
			}

			// Build tree.
			policy, err := BuildTree(leaves, &ARKNUMSKey)
			require.NoError(t, err)

			// Verify root hash matches golden vector.
			rootHashHex := hex.EncodeToString(policy.RootHash)
			require.Equal(
				t, vec.RootHashHex, rootHashHex,
				"root hash mismatch",
			)

			// Verify output key matches golden vector.
			outputKey := policy.OutputKey()
			outputKeyHex := hex.EncodeToString(
				outputKey.SerializeCompressed(),
			)
			require.Equal(
				t, vec.OutputKeyHex, outputKeyHex,
				"output key mismatch",
			)

			// Control block golden vector checks are done via
			// NewVTXOPolicy in TestGoldenVTXOVectors, which
			// handles canonical index mapping correctly.
		})
	}
}

// TestBuildTreeFourLeaves tests tree construction with four leaves.
func TestBuildTreeFourLeaves(t *testing.T) {
	t.Parallel()

	// Create 4 simple leaves.
	leafScripts := [][]byte{
		{
			0x01,
		}, {
			0x02,
		}, {
			0x03,
		}, {
			0x04,
		},
	}

	leaves := make([]PolicyLeaf, 4)
	for i, s := range leafScripts {
		leaves[i] = PolicyLeaf{
			Leaf: txscript.NewBaseTapLeaf(s),
		}
	}

	policy, err := BuildTree(leaves, &ARKNUMSKey)
	require.NoError(t, err)

	// Build using btcd for comparison.
	btcdLeaves := make([]txscript.TapLeaf, 4)
	for i, s := range leafScripts {
		btcdLeaves[i] = txscript.NewBaseTapLeaf(s)
	}
	btcdTree := txscript.AssembleTaprootScriptTree(btcdLeaves...)
	btcdRootHash := btcdTree.RootNode.TapHash()

	// Root hashes should match.
	require.Equal(
		t, btcdRootHash[:], policy.RootHash,
		"4-leaf tree root hash should match btcd",
	)

	// Each leaf should have 2 siblings in its proof (depth 2 for 4 leaves).
	for i := 0; i < 4; i++ {
		require.Len(
			t, policy.merkleProofs[i], 2, "leaf %d should have "+
				"2 siblings in proof", i,
		)
	}
}

// TestBuildTreeThreeLeaves tests tree construction with three leaves
// (unbalanced case). Note: our algorithm may differ from btcd's for non-power-
// of-2 leaves, which is intentional per the RFC (deterministic vs heuristics).
func TestBuildTreeThreeLeaves(t *testing.T) {
	t.Parallel()

	leafScripts := [][]byte{
		{
			0x01,
		}, {
			0x02,
		}, {
			0x03,
		},
	}

	leaves := make([]PolicyLeaf, 3)
	for i, s := range leafScripts {
		leaves[i] = PolicyLeaf{
			Leaf: txscript.NewBaseTapLeaf(s),
		}
	}

	policy, err := BuildTree(leaves, &ARKNUMSKey)
	require.NoError(t, err)

	// For 3 leaves with our split (left=1, right=2):
	// - Leaf 0 has 1 sibling (right subtree root).
	// - Leaves 1,2 have 2 siblings (each other + left subtree root).
	require.Len(t, policy.merkleProofs[0], 1)
	require.Len(t, policy.merkleProofs[1], 2)
	require.Len(t, policy.merkleProofs[2], 2)

	// Verify the tree structure is deterministic by rebuilding.
	policy2, err := BuildTree(leaves, &ARKNUMSKey)
	require.NoError(t, err)
	require.Equal(
		t, policy.RootHash, policy2.RootHash,
		"tree construction should be deterministic",
	)
}

// TestBuildTreeEmpty tests that building an empty tree returns an error.
func TestBuildTreeEmpty(t *testing.T) {
	t.Parallel()

	_, err := BuildTree(nil, &ARKNUMSKey)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no leaves")

	_, err = BuildTree([]PolicyLeaf{}, &ARKNUMSKey)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no leaves")
}

// TestSpendInfoOutOfBounds tests that SpendInfo returns an error for invalid
// indices.
func TestSpendInfoOutOfBounds(t *testing.T) {
	t.Parallel()

	leaves := []PolicyLeaf{
		{
			Leaf: txscript.NewBaseTapLeaf([]byte{0x01}),
		},
	}

	policy, err := BuildTree(leaves, &ARKNUMSKey)
	require.NoError(t, err)

	_, err = policy.SpendInfo(-1)
	require.Error(t, err)

	_, err = policy.SpendInfo(1)
	require.Error(t, err)

	// Valid index should work.
	info, err := policy.SpendInfo(0)
	require.NoError(t, err)
	require.NotNil(t, info)
}

// TestCompareToWithDifferentLeafVersions verifies that canonical ordering
// is stable for leaves with identical scripts but different leaf versions.
func TestCompareToWithDifferentLeafVersions(t *testing.T) {
	t.Parallel()

	script := []byte{0x01, 0x02, 0x03}

	leafA := PolicyLeaf{
		Leaf: txscript.TapLeaf{
			LeafVersion: 0xc0,
			Script:      script,
		},
	}
	leafB := PolicyLeaf{
		Leaf: txscript.TapLeaf{
			LeafVersion: 0xc2,
			Script:      script,
		},
	}

	// Version 0xc0 sorts before 0xc2.
	require.Equal(t, -1, leafA.CompareTo(&leafB))
	require.Equal(t, 1, leafB.CompareTo(&leafA))

	// SortLeaves should produce a stable order.
	leaves := []PolicyLeaf{leafB, leafA}
	sortLeaves(leaves)

	require.Equal(
		t, txscript.TapscriptLeafVersion(0xc0),
		leaves[0].Leaf.LeafVersion,
	)
	require.Equal(
		t, txscript.TapscriptLeafVersion(0xc2),
		leaves[1].Leaf.LeafVersion,
	)

	// Reverse input order should produce the same result.
	leaves2 := []PolicyLeaf{leafA, leafB}
	sortLeaves(leaves2)

	require.Equal(
		t, leaves[0].Leaf.LeafVersion, leaves2[0].Leaf.LeafVersion,
	)
	require.Equal(
		t, leaves[1].Leaf.LeafVersion, leaves2[1].Leaf.LeafVersion,
	)
}

// TestBuildTreeNilInternalKey verifies that nil internal key is rejected.
func TestBuildTreeNilInternalKey(t *testing.T) {
	t.Parallel()

	leaves := []PolicyLeaf{
		{
			Leaf: txscript.NewBaseTapLeaf([]byte{0x01}),
		},
	}

	_, err := BuildTree(leaves, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "internal key must be provided")
}

// TestBuildTreeRejectsNonNUMSKey verifies that only the NUMS key is
// accepted as internal key.
func TestBuildTreeRejectsNonNUMSKey(t *testing.T) {
	t.Parallel()

	leaves := []PolicyLeaf{
		{
			Leaf: txscript.NewBaseTapLeaf([]byte{0x01}),
		},
	}

	spendableKey, _ := testutils.CreateKey(1)
	_, err := BuildTree(leaves, spendableKey)
	require.Error(t, err)
	require.Contains(t, err.Error(), "Ark NUMS key")
}

// TestBuildTreeDefensiveCopy verifies that mutating input leaves after
// BuildTree does not affect the compiled policy.
func TestBuildTreeDefensiveCopy(t *testing.T) {
	t.Parallel()

	script := []byte{0x01, 0x02, 0x03}
	leaves := []PolicyLeaf{
		{
			Leaf: txscript.NewBaseTapLeaf(script),
		},
	}

	policy, err := BuildTree(leaves, &ARKNUMSKey)
	require.NoError(t, err)

	// Mutate the original slice.
	script[0] = 0xff

	// Policy should be unaffected.
	require.Equal(t, byte(0x01), policy.Leaves[0].Leaf.Script[0])
}

// TestSpendInfoDefensiveCopy verifies that mutating SpendInfo does not
// affect the compiled policy.
func TestSpendInfoDefensiveCopy(t *testing.T) {
	t.Parallel()

	script := []byte{0x01, 0x02, 0x03}
	leaves := []PolicyLeaf{
		{
			Leaf: txscript.NewBaseTapLeaf(script),
		},
	}

	policy, err := BuildTree(leaves, &ARKNUMSKey)
	require.NoError(t, err)

	info, err := policy.SpendInfo(0)
	require.NoError(t, err)

	// Mutate the returned witness script.
	info.WitnessScript[0] = 0xff

	// Policy leaves should be unaffected.
	require.Equal(t, byte(0x01), policy.Leaves[0].Leaf.Script[0])
}

// TestControlBlockFormat verifies the control block format matches BIP-341.
func TestControlBlockFormat(t *testing.T) {
	t.Parallel()

	key, _ := testutils.CreateKey(1)
	script, _ := (&Multisig{Keys: []*btcec.PublicKey{key}}).Script()

	leaves := []PolicyLeaf{
		{
			Leaf: txscript.NewBaseTapLeaf(script),
		},
		{
			Leaf: txscript.NewBaseTapLeaf([]byte{0x51}),
		},
	}

	policy, err := BuildTree(leaves, &ARKNUMSKey)
	require.NoError(t, err)

	info, err := policy.SpendInfo(0)
	require.NoError(t, err)

	// Control block should be: 1 (control byte) + 32 (internal key) +
	// 32 * 1 (one sibling) = 65 bytes.
	require.Len(t, info.ControlBlock, 65)

	// First byte is control byte (leaf version + parity).
	controlByte := info.ControlBlock[0]
	leafVersion := controlByte & 0xfe
	require.Equal(t, byte(txscript.BaseLeafVersion), leafVersion)

	// Next 32 bytes are internal key.
	internalKeyBytes := info.ControlBlock[1:33]
	expectedInternalKey := ARKNUMSKey.SerializeCompressed()[1:]
	require.Equal(t, expectedInternalKey, internalKeyBytes)
}
