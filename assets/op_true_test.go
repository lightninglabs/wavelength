package assets

import (
	"testing"

	"github.com/btcsuite/btcd/txscript"
	"github.com/stretchr/testify/require"
)

func TestBuildOpTrueArtifacts(t *testing.T) {
	t.Parallel()

	// BuildOpTrueArtifacts now always uses NUMS, so we only test success
	// case.
	artifacts, err := BuildOpTrueArtifacts()
	require.NoError(t, err)
	require.NotNil(t, artifacts)

	// Verify sibling preimage.
	require.NotNil(t, artifacts.SiblingPreimage)

	// Verify witness structure: [script, control_block].
	require.Len(t, artifacts.Witness, 2)
	require.Equal(t,
		[]byte{txscript.OP_TRUE}, artifacts.Witness[0],
		"first witness element should be OP_TRUE",
	)
	require.Greater(t,
		len(artifacts.Witness[1]), 0,
		"second witness element (control block) should not be empty",
	)

	// Verify output key.
	require.NotNil(t, artifacts.OutputKey)

	// Make sure that the call is deterministic by building again.
	artifacts2, err := BuildOpTrueArtifacts()
	require.NoError(t, err)

	// Results should be identical (deterministic).
	require.True(t,
		artifacts.OutputKey.IsEqual(artifacts2.OutputKey),
		"output keys should be identical",
	)

	require.Equal(t,
		artifacts.Witness[0], artifacts2.Witness[0],
		"scripts should be identical",
	)

	require.Equal(t,
		artifacts.Witness[1], artifacts2.Witness[1],
		"control blocks should be identical",
	)
}

func TestBuildOpTrueArtifacts_ControlBlockStructure(t *testing.T) {
	t.Parallel()

	// Build artifacts (uses NUMS internally).
	artifacts, err := BuildOpTrueArtifacts()
	require.NoError(t, err)

	// Decode and verify control block.
	controlBlockBytes := artifacts.Witness[1]
	controlBlock, err := txscript.ParseControlBlock(controlBlockBytes)
	require.NoError(t, err)

	// Verify leaf version.
	require.Equal(t, txscript.BaseLeafVersion, controlBlock.LeafVersion,
		"leaf version should be base (0xC0)")

	// Verify we can reconstruct the output key.
	opTrueScript := []byte{txscript.OP_TRUE}
	tapLeaf := txscript.NewBaseTapLeaf(opTrueScript)
	tapTree := txscript.AssembleTaprootScriptTree(tapLeaf)
	rootHash := tapTree.RootNode.TapHash()

	// Internal key is NUMS
	reconstructedKey := txscript.ComputeTaprootOutputKey(
		controlBlock.InternalKey, rootHash[:],
	)
	require.True(t,
		artifacts.OutputKey.IsEqual(reconstructedKey),
		"output key should match reconstructed key",
	)
}
