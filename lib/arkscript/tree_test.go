package arkscript

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightninglabs/darepo-client/internal/testutils"
	"github.com/stretchr/testify/require"
)

// TestLeafRoleOrdering verifies that leaf roles have the correct rank values.
func TestLeafRoleOrdering(t *testing.T) {
	t.Parallel()

	require.Equal(t, 0, LeafRoleCollab.Rank())
	require.Equal(t, 1, LeafRoleExit.Rank())
	require.Equal(t, 2, LeafRoleCustom.Rank())

	require.Equal(t, "collab", LeafRoleCollab.String())
	require.Equal(t, "exit", LeafRoleExit.String())
	require.Equal(t, "custom", LeafRoleCustom.String())
}

// TestPolicyLeafCompareTo tests the canonical comparison of policy leaves.
func TestPolicyLeafCompareTo(t *testing.T) {
	t.Parallel()

	key1, _ := testutils.CreateKey(1)
	key2, _ := testutils.CreateKey(2)

	checksig1 := &Checksig{Key: key1}
	checksig2 := &Checksig{Key: key2}

	script1, err := checksig1.Script()
	require.NoError(t, err)
	script2, err := checksig2.Script()
	require.NoError(t, err)

	leaf1 := PolicyLeaf{
		Role: LeafRoleCollab,
		Leaf: txscript.NewBaseTapLeaf(script1),
	}

	leaf2 := PolicyLeaf{
		Role: LeafRoleExit,
		Leaf: txscript.NewBaseTapLeaf(script2),
	}

	leaf3 := PolicyLeaf{
		Role: LeafRoleCollab,
		Leaf: txscript.NewBaseTapLeaf(script2),
	}

	// Different roles: collab < exit.
	require.Equal(t, -1, leaf1.CompareTo(&leaf2))
	require.Equal(t, 1, leaf2.CompareTo(&leaf1))

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

	script1, _ := (&Checksig{Key: key1}).Script()
	script2, _ := (&Checksig{Key: key2}).Script()
	script3, _ := (&Checksig{Key: key3}).Script()

	// Create leaves in wrong order.
	leaves := []PolicyLeaf{
		{Role: LeafRoleCustom, Leaf: txscript.NewBaseTapLeaf(script3)},
		{Role: LeafRoleExit, Leaf: txscript.NewBaseTapLeaf(script1)},
		{Role: LeafRoleCollab, Leaf: txscript.NewBaseTapLeaf(script2)},
	}

	SortLeaves(leaves)

	// After sorting: collab, exit, custom.
	require.Equal(t, LeafRoleCollab, leaves[0].Role)
	require.Equal(t, LeafRoleExit, leaves[1].Role)
	require.Equal(t, LeafRoleCustom, leaves[2].Role)
}

// TestSortLeavesLexicographic tests lexicographic secondary sorting.
func TestSortLeavesLexicographic(t *testing.T) {
	t.Parallel()

	// Create two leaves with the same role but different scripts.
	scriptA := []byte{0x01, 0x02, 0x03}
	scriptB := []byte{0x01, 0x02, 0x04}

	leaves := []PolicyLeaf{
		{Role: LeafRoleCustom, Leaf: txscript.NewBaseTapLeaf(scriptB)},
		{Role: LeafRoleCustom, Leaf: txscript.NewBaseTapLeaf(scriptA)},
	}

	SortLeaves(leaves)

	// scriptA < scriptB lexicographically.
	require.True(t, bytes.Compare(leaves[0].Leaf.Script, leaves[1].Leaf.Script) < 0)
}

// TestBuildTreeSingleLeaf tests tree construction with a single leaf.
func TestBuildTreeSingleLeaf(t *testing.T) {
	t.Parallel()

	key, _ := testutils.CreateKey(1)
	script, _ := (&Checksig{Key: key}).Script()

	leaves := []PolicyLeaf{
		{Role: LeafRoleCollab, Leaf: txscript.NewBaseTapLeaf(script)},
	}

	policy, err := BuildTree(leaves, &ARKNUMSKey)
	require.NoError(t, err)
	require.Len(t, policy.Leaves, 1)
	require.Len(t, policy.merkleProofs[0], 0) // Single leaf has no siblings.

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
		Keys: []*btcec.PublicKey{ownerKey, operatorKey},
		Type: MultisigTypeChecksig,
	}
	collabScript, err := collabNode.Script()
	require.NoError(t, err)

	exitNode := &CSV{
		Lock:  exitDelay,
		Inner: &Checksig{Key: ownerKey},
	}
	exitScript, err := exitNode.Script()
	require.NoError(t, err)

	leaves := []PolicyLeaf{
		{Role: LeafRoleCollab, Leaf: txscript.NewBaseTapLeaf(collabScript)},
		{Role: LeafRoleExit, Leaf: txscript.NewBaseTapLeaf(exitScript)},
	}

	// Build tree using our implementation.
	policy, err := BuildTree(leaves, &ARKNUMSKey)
	require.NoError(t, err)

	// Build tree using btcd's implementation.
	btcdLeaves := []txscript.TapLeaf{
		txscript.NewBaseTapLeaf(collabScript),
		txscript.NewBaseTapLeaf(exitScript),
	}
	btcdTree := txscript.AssembleTaprootScriptTree(btcdLeaves...)
	btcdRootHash := btcdTree.RootNode.TapHash()

	// CRITICAL: Root hashes must match for backward compatibility.
	require.Equal(t, btcdRootHash[:], policy.RootHash,
		"BuildTree root hash must match btcd AssembleTaprootScriptTree")

	// Verify output keys match.
	expectedOutputKey := txscript.ComputeTaprootOutputKey(
		&ARKNUMSKey, btcdRootHash[:],
	)
	require.Equal(t, expectedOutputKey, policy.OutputKey())

	// Verify proofs: each leaf should have exactly one sibling.
	require.Len(t, policy.merkleProofs[0], 1)
	require.Len(t, policy.merkleProofs[1], 1)

	// The sibling for leaf 0 should be the hash of leaf 1, and vice versa.
	leaf0Hash := leaves[0].Leaf.TapHash()
	leaf1Hash := leaves[1].Leaf.TapHash()
	require.Equal(t, leaf1Hash, policy.merkleProofs[0][0])
	require.Equal(t, leaf0Hash, policy.merkleProofs[1][0])
}

// TestBuildTreeMatchesGoldenVectors verifies that BuildTree produces control
// blocks that match the golden test vectors.
func TestBuildTreeMatchesGoldenVectors(t *testing.T) {
	t.Parallel()

	for _, vec := range goldenVTXOVectors {
		t.Run(vec.Name, func(t *testing.T) {
			t.Parallel()

			ownerKey, _ := testutils.CreateKey(vec.OwnerKeyIndex)
			operatorKey, _ := testutils.CreateKey(vec.OperatorKeyIndex)

			// Build collab leaf using AST.
			collabNode := &Multisig{
				Keys: []*btcec.PublicKey{ownerKey, operatorKey},
				Type: MultisigTypeChecksig,
			}
			collabScript, err := collabNode.Script()
			require.NoError(t, err)

			// Build exit leaf using AST.
			exitNode := &CSV{
				Lock:  vec.ExitDelay,
				Inner: &Checksig{Key: ownerKey},
			}
			exitScript, err := exitNode.Script()
			require.NoError(t, err)

			// Construct leaves in canonical order.
			leaves := []PolicyLeaf{
				{
					Role: LeafRoleCollab,
					Leaf: txscript.NewBaseTapLeaf(collabScript),
				},
				{
					Role: LeafRoleExit,
					Leaf: txscript.NewBaseTapLeaf(exitScript),
				},
			}

			// Build tree.
			policy, err := BuildTree(leaves, &ARKNUMSKey)
			require.NoError(t, err)

			// Verify root hash matches golden vector.
			rootHashHex := hex.EncodeToString(policy.RootHash)
			require.Equal(t, vec.RootHashHex, rootHashHex,
				"root hash mismatch")

			// Verify output key matches golden vector.
			outputKey := policy.OutputKey()
			outputKeyHex := hex.EncodeToString(
				outputKey.SerializeCompressed(),
			)
			require.Equal(t, vec.OutputKeyHex, outputKeyHex,
				"output key mismatch")

			// Verify collab control block matches golden vector.
			collabSpendInfo, err := policy.SpendInfo(0)
			require.NoError(t, err)
			collabControlHex := hex.EncodeToString(
				collabSpendInfo.ControlBlock,
			)
			require.Equal(t, vec.CollabControlHex, collabControlHex,
				"collab control block mismatch")

			// Verify timeout control block matches golden vector.
			timeoutSpendInfo, err := policy.SpendInfo(1)
			require.NoError(t, err)
			timeoutControlHex := hex.EncodeToString(
				timeoutSpendInfo.ControlBlock,
			)
			require.Equal(t, vec.TimeoutControlHex, timeoutControlHex,
				"timeout control block mismatch")
		})
	}
}

// TestBuildTreeFourLeaves tests tree construction with four leaves.
func TestBuildTreeFourLeaves(t *testing.T) {
	t.Parallel()

	// Create 4 simple leaves.
	leafScripts := [][]byte{
		{0x01}, {0x02}, {0x03}, {0x04},
	}

	leaves := make([]PolicyLeaf, 4)
	for i, s := range leafScripts {
		leaves[i] = PolicyLeaf{
			Role: LeafRoleCustom,
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
	require.Equal(t, btcdRootHash[:], policy.RootHash,
		"4-leaf tree root hash should match btcd")

	// Each leaf should have 2 siblings in its proof (depth 2 for 4 leaves).
	for i := 0; i < 4; i++ {
		require.Len(t, policy.merkleProofs[i], 2,
			"leaf %d should have 2 siblings in proof", i)
	}
}

// TestBuildTreeThreeLeaves tests tree construction with three leaves
// (unbalanced case). Note: our algorithm may differ from btcd's for non-power-
// of-2 leaves, which is intentional per the RFC (deterministic vs heuristics).
func TestBuildTreeThreeLeaves(t *testing.T) {
	t.Parallel()

	leafScripts := [][]byte{
		{0x01}, {0x02}, {0x03},
	}

	leaves := make([]PolicyLeaf, 3)
	for i, s := range leafScripts {
		leaves[i] = PolicyLeaf{
			Role: LeafRoleCustom,
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
	require.Equal(t, policy.RootHash, policy2.RootHash,
		"tree construction should be deterministic")
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
		{Role: LeafRoleCollab, Leaf: txscript.NewBaseTapLeaf([]byte{0x01})},
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

// TestControlBlockFormat verifies the control block format matches BIP-341.
func TestControlBlockFormat(t *testing.T) {
	t.Parallel()

	key, _ := testutils.CreateKey(1)
	script, _ := (&Checksig{Key: key}).Script()

	leaves := []PolicyLeaf{
		{Role: LeafRoleCollab, Leaf: txscript.NewBaseTapLeaf(script)},
		{Role: LeafRoleExit, Leaf: txscript.NewBaseTapLeaf([]byte{0x51})},
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
