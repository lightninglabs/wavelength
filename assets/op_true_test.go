package assets

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/stretchr/testify/require"
)

func TestBuildOpTrueArtifacts(t *testing.T) {
	t.Parallel()

	// Test with NUMS internal key.
	artifacts, err := BuildOpTrueArtifacts(asset.NUMSPubKey)
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
	artifacts2, err := BuildOpTrueArtifacts(asset.NUMSPubKey)
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

	// Build artifacts with NUMS internal key.
	artifacts, err := BuildOpTrueArtifacts(asset.NUMSPubKey)
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

	// Internal key is NUMS. Compare X-coordinates only since the stored
	// OutputKey is normalized to even Y parity via schnorr serialization.
	reconstructedKey := txscript.ComputeTaprootOutputKey(
		controlBlock.InternalKey, rootHash[:],
	)

	// Compare x-coordinates (schnorr serializes just the X).
	require.Equal(t,
		schnorr.SerializePubKey(artifacts.OutputKey),
		schnorr.SerializePubKey(reconstructedKey),
		"output key X-coordinate should match reconstructed key",
	)

	// Verify control block parity matches the actual output key parity.
	actualParity := reconstructedKey.SerializeCompressed()[0] == 0x03
	require.Equal(t, actualParity, controlBlock.OutputKeyYIsOdd,
		"control block parity should match actual output key parity")
}
