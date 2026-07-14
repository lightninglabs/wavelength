package arkscript

import (
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/lightninglabs/wavelength/internal/testutils"
	"github.com/stretchr/testify/require"
)

// TestComposeWithSiblingRoot tests basic composition functionality.
func TestComposeWithSiblingRoot(t *testing.T) {
	t.Parallel()

	ownerKey, _ := testutils.CreateKey(1)
	operatorKey, _ := testutils.CreateKey(2)

	policy, err := NewVTXOPolicy(ownerKey, operatorKey, 100)
	require.NoError(t, err)

	// Create a fake external root (e.g., from Taproot Assets).
	var externalRoot chainhash.Hash
	copy(externalRoot[:], []byte("external_commitment_root_hash123"))

	composed, err := ComposeWithSiblingRoot(
		policy.CompiledPolicy, externalRoot,
	)
	require.NoError(t, err)
	require.NotNil(t, composed)

	// Verify the composed policy has the correct roots.
	require.Equal(t, policy.RootHash, composed.PolicyRoot[:])
	require.Equal(t, externalRoot, composed.ExternalRoot)
	require.NotEqual(
		t, policy.RootHash, composed.CombinedRoot[:],
		"combined root should differ from policy root",
	)
}

// TestComposeWithSiblingRootOutputKey tests that the output key changes with
// composition.
func TestComposeWithSiblingRootOutputKey(t *testing.T) {
	t.Parallel()

	ownerKey, _ := testutils.CreateKey(1)
	operatorKey, _ := testutils.CreateKey(2)

	policy, err := NewVTXOPolicy(ownerKey, operatorKey, 100)
	require.NoError(t, err)

	var externalRoot chainhash.Hash
	copy(externalRoot[:], []byte("test_external_root_32_bytes_pad!"))

	composed, err := ComposeWithSiblingRoot(
		policy.CompiledPolicy, externalRoot,
	)
	require.NoError(t, err)

	// Output keys should differ.
	originalKey := policy.OutputKey()
	composedKey := composed.OutputKey()
	require.NotEqual(
		t, originalKey.SerializeCompressed(),
		composedKey.SerializeCompressed(),
		"composed output key should differ from original",
	)
}

// TestComposeWithSiblingRootSpendInfo tests that spend info includes the
// external root in the control block.
func TestComposeWithSiblingRootSpendInfo(t *testing.T) {
	t.Parallel()

	ownerKey, _ := testutils.CreateKey(1)
	operatorKey, _ := testutils.CreateKey(2)

	policy, err := NewVTXOPolicy(ownerKey, operatorKey, 100)
	require.NoError(t, err)

	var externalRoot chainhash.Hash
	copy(externalRoot[:], []byte("test_external_root_32_bytes_pad!"))

	composed, err := ComposeWithSiblingRoot(
		policy.CompiledPolicy, externalRoot,
	)
	require.NoError(t, err)

	// Get spend info for the collab leaf.
	info, err := composed.SpendInfo(0)
	require.NoError(t, err)

	// Control block should be longer than the original
	// due to extra sibling.
	originalInfo, err := policy.SpendInfo(0)
	require.NoError(t, err)

	require.Equal(
		t, len(originalInfo.ControlBlock)+32, len(info.ControlBlock),
		"composed control block should be 32 bytes longer",
	)

	// The last 32 bytes should be the external root.
	controlBlockLen := len(info.ControlBlock)
	lastSibling := info.ControlBlock[controlBlockLen-32:]
	require.Equal(
		t, externalRoot[:], lastSibling,
		"last sibling should be external root",
	)
}

// TestComposeWithSiblingRootDeterministic tests that composition is
// deterministic.
func TestComposeWithSiblingRootDeterministic(t *testing.T) {
	t.Parallel()

	ownerKey, _ := testutils.CreateKey(1)
	operatorKey, _ := testutils.CreateKey(2)

	policy, err := NewVTXOPolicy(ownerKey, operatorKey, 100)
	require.NoError(t, err)

	var externalRoot chainhash.Hash
	copy(externalRoot[:], []byte("deterministic_test_root_32bytes!"))

	composed1, err := ComposeWithSiblingRoot(
		policy.CompiledPolicy, externalRoot,
	)
	require.NoError(t, err)

	composed2, err := ComposeWithSiblingRoot(
		policy.CompiledPolicy, externalRoot,
	)
	require.NoError(t, err)

	// Both should produce identical results.
	require.Equal(t, composed1.CombinedRoot, composed2.CombinedRoot)
	require.Equal(
		t, composed1.OutputKey().SerializeCompressed(),
		composed2.OutputKey().SerializeCompressed(),
	)
}

// TestComposeWithSiblingRootNilPolicy tests error handling for nil policy.
func TestComposeWithSiblingRootNilPolicy(t *testing.T) {
	t.Parallel()

	var externalRoot chainhash.Hash
	_, err := ComposeWithSiblingRoot(nil, externalRoot)
	require.Error(t, err)
	require.Contains(t, err.Error(), "policy is nil")
}

// TestComposedRootOrdering tests that the combined root is computed with
// proper BIP-341 ordering (min first).
func TestComposedRootOrdering(t *testing.T) {
	t.Parallel()

	ownerKey, _ := testutils.CreateKey(1)
	operatorKey, _ := testutils.CreateKey(2)

	policy, err := NewVTXOPolicy(ownerKey, operatorKey, 100)
	require.NoError(t, err)

	// Create two different external roots.
	var extRoot1, extRoot2 chainhash.Hash
	copy(extRoot1[:], []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	copy(extRoot2[:], []byte("zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"))

	composed1, err := ComposeWithSiblingRoot(
		policy.CompiledPolicy, extRoot1,
	)
	require.NoError(t, err)

	composed2, err := ComposeWithSiblingRoot(
		policy.CompiledPolicy, extRoot2,
	)
	require.NoError(t, err)

	// Different external roots should produce different combined roots.
	require.NotEqual(t, composed1.CombinedRoot, composed2.CombinedRoot)
}

// TestPolicyRootHelper tests the PolicyRoot helper function.
func TestPolicyRootHelper(t *testing.T) {
	t.Parallel()

	ownerKey, _ := testutils.CreateKey(1)
	operatorKey, _ := testutils.CreateKey(2)

	policy, err := NewVTXOPolicy(ownerKey, operatorKey, 100)
	require.NoError(t, err)

	root := PolicyRoot(policy.CompiledPolicy)
	require.Equal(t, policy.RootHash, root[:])
}

// TestComposeWithSiblingRootPreservesWitnessScript tests that the witness
// script is preserved in composed spend info.
func TestComposeWithSiblingRootPreservesWitnessScript(t *testing.T) {
	t.Parallel()

	ownerKey, _ := testutils.CreateKey(1)
	operatorKey, _ := testutils.CreateKey(2)

	policy, err := NewVTXOPolicy(ownerKey, operatorKey, 100)
	require.NoError(t, err)

	var externalRoot chainhash.Hash
	copy(externalRoot[:], []byte("test_root_for_witness_script_tst"))

	composed, err := ComposeWithSiblingRoot(
		policy.CompiledPolicy, externalRoot,
	)
	require.NoError(t, err)

	// Check both leaves.
	for i := 0; i < len(policy.Leaves); i++ {
		originalInfo, err := policy.SpendInfo(i)
		require.NoError(t, err)

		composedInfo, err := composed.SpendInfo(i)
		require.NoError(t, err)

		// Witness scripts should be identical.
		require.Equal(
			t, originalInfo.WitnessScript,
			composedInfo.WitnessScript, "leaf %d witness script "+
				"should be preserved", i,
		)
	}
}

// TestTapBranchHashComposeCommutative verifies that composition is consistent
// regardless of input order (both produce the same hash due to sorting).
func TestTapBranchHashComposeCommutative(t *testing.T) {
	t.Parallel()

	var a, b chainhash.Hash
	copy(a[:], []byte("first_hash_32_bytes_padded_here!"))
	copy(b[:], []byte("second_hash_32_bytes_padded_yay!"))

	// Compose in both orders.
	result1 := tapBranchHashCompose(a, b)
	result2 := tapBranchHashCompose(b, a)

	// Results should be identical due to min/max ordering.
	require.Equal(t, result1, result2)
}

// TestComposedPolicyControlBlockValidation tests that the composed control
// block can be validated.
func TestComposedPolicyControlBlockValidation(t *testing.T) {
	t.Parallel()

	ownerKey, _ := testutils.CreateKey(1)
	operatorKey, _ := testutils.CreateKey(2)

	policy, err := NewVTXOPolicy(ownerKey, operatorKey, 100)
	require.NoError(t, err)

	var externalRoot chainhash.Hash
	copy(externalRoot[:], []byte("validation_test_external_root!!!"))

	composed, err := ComposeWithSiblingRoot(
		policy.CompiledPolicy, externalRoot,
	)
	require.NoError(t, err)

	// Get spend info.
	info, err := composed.SpendInfo(0)
	require.NoError(t, err)

	// Parse the control block.
	ctrlBlock, err := txscript.ParseControlBlock(info.ControlBlock)
	require.NoError(t, err)

	// Verify the leaf version.
	require.Equal(t, txscript.BaseLeafVersion, ctrlBlock.LeafVersion)

	// Verify the internal key matches.
	require.Equal(
		t, ARKNUMSKey.SerializeCompressed()[1:],
		ctrlBlock.InternalKey.SerializeCompressed()[1:],
	)
}
