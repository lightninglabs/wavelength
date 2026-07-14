package arkscript

import (
	"testing"

	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/lightninglabs/wavelength/internal/testutils"
	"github.com/stretchr/testify/require"
)

// TestEncodeTapTreeRoundTrip tests that encoding and decoding produces the
// same leaf data.
func TestEncodeTapTreeRoundTrip(t *testing.T) {
	t.Parallel()

	ownerKey, _ := testutils.CreateKey(1)
	operatorKey, _ := testutils.CreateKey(2)

	policy, err := NewVTXOPolicy(ownerKey, operatorKey, 100)
	require.NoError(t, err)

	// Encode the tap tree.
	encoded, err := EncodeTapTree(policy.CompiledPolicy)
	require.NoError(t, err)
	require.NotEmpty(t, encoded)

	// Decode the tap tree.
	decoded, err := DecodeTapTree(encoded)
	require.NoError(t, err)
	require.Len(t, decoded, len(policy.Leaves))

	// Verify each leaf matches.
	for i, leaf := range decoded {
		require.Equal(
			t, policy.Leaves[i].Leaf.Script, leaf.Script, "leaf "+
				"%d script mismatch", i,
		)
		require.Equal(
			t, byte(policy.Leaves[i].Leaf.LeafVersion),
			leaf.LeafVersion, "leaf %d version mismatch", i,
		)

		// Depth should match proof length.
		expectedDepth := uint8(len(policy.merkleProofs[i]))
		require.Equal(
			t, expectedDepth, leaf.Depth, "leaf %d depth mismatch",
			i,
		)
	}
}

// TestEncodeTapTreeFormat tests the exact encoding format.
func TestEncodeTapTreeFormat(t *testing.T) {
	t.Parallel()

	// Create a simple 2-leaf tree.
	leaves := []PolicyLeaf{
		{
			Leaf: txscript.NewBaseTapLeaf([]byte{0x01, 0x02, 0x03}),
		},
		{
			Leaf: txscript.NewBaseTapLeaf([]byte{0x04, 0x05}),
		},
	}

	policy, err := BuildTree(leaves, &ARKNUMSKey)
	require.NoError(t, err)

	encoded, err := EncodeTapTree(policy)
	require.NoError(t, err)

	// Expected format:
	// - 0x02 (leaf count = 2)
	// - Leaf 0: depth=1, version=0xc0, script_len=3, script=[01,02,03]
	// - Leaf 1: depth=1, version=0xc0, script_len=2, script=[04,05]
	//
	// Total: 1 + (1+1+1+3) + (1+1+1+2) = 12 bytes
	require.Len(t, encoded, 12)

	// Verify leaf count.
	require.Equal(t, byte(0x02), encoded[0])

	// Verify first leaf.
	require.Equal(t, byte(0x01), encoded[1]) // depth
	require.Equal(t, byte(0xc0), encoded[2]) // version
	require.Equal(t, byte(0x03), encoded[3]) // script length
	require.Equal(t, []byte{0x01, 0x02, 0x03}, encoded[4:7])

	// Verify second leaf.
	require.Equal(t, byte(0x01), encoded[7]) // depth
	require.Equal(t, byte(0xc0), encoded[8]) // version
	require.Equal(t, byte(0x02), encoded[9]) // script length
	require.Equal(t, []byte{0x04, 0x05}, encoded[10:12])
}

// TestEncodeTapTreeSingleLeaf tests encoding with a single leaf.
func TestEncodeTapTreeSingleLeaf(t *testing.T) {
	t.Parallel()

	leaves := []PolicyLeaf{
		{
			Leaf: txscript.NewBaseTapLeaf([]byte{0xab, 0xcd}),
		},
	}

	policy, err := BuildTree(leaves, &ARKNUMSKey)
	require.NoError(t, err)

	encoded, err := EncodeTapTree(policy)
	require.NoError(t, err)

	decoded, err := DecodeTapTree(encoded)
	require.NoError(t, err)
	require.Len(t, decoded, 1)

	// Single leaf should have depth 0.
	require.Equal(t, uint8(0), decoded[0].Depth)
	require.Equal(t, []byte{0xab, 0xcd}, decoded[0].Script)
}

// TestEncodeTapTreeEmpty tests that empty policies return an error.
func TestEncodeTapTreeEmpty(t *testing.T) {
	t.Parallel()

	_, err := EncodeTapTree(nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "empty policy")

	_, err = EncodeTapTree(&CompiledPolicy{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "empty policy")
}

// TestDecodeTapTreeEmpty tests that empty data returns an error.
func TestDecodeTapTreeEmpty(t *testing.T) {
	t.Parallel()

	_, err := DecodeTapTree(nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "empty")

	_, err = DecodeTapTree([]byte{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "empty")
}

// TestDecodeTapTreeZeroLeaves tests that zero leaf count returns an error.
func TestDecodeTapTreeZeroLeaves(t *testing.T) {
	t.Parallel()

	_, err := DecodeTapTree([]byte{0x00})
	require.Error(t, err)
	require.Contains(t, err.Error(), "zero leaves")
}

// TestDecodeTapTreeExtraBytes tests that extra bytes are detected.
func TestDecodeTapTreeExtraBytes(t *testing.T) {
	t.Parallel()

	// Valid encoding for 1 leaf with 2-byte script, plus extra byte.
	data := []byte{
		0x01,       // leaf count
		0x00,       // depth
		0xc0,       // version
		0x02,       // script length
		0x01, 0x02, // script
		0xff, // extra byte
	}

	_, err := DecodeTapTree(data)
	require.Error(t, err)
	require.Contains(t, err.Error(), "extra bytes")
}

// TestDecodeTapTreeTruncated tests that truncated data is detected.
func TestDecodeTapTreeTruncated(t *testing.T) {
	t.Parallel()

	// Valid encoding truncated in the middle of script.
	data := []byte{
		0x01,       // leaf count
		0x00,       // depth
		0xc0,       // version
		0x05,       // script length = 5
		0x01, 0x02, // only 2 bytes provided
	}

	_, err := DecodeTapTree(data)
	require.Error(t, err)
}

// TestEncodeConditionWitnessRoundTrip tests condition witness encoding round
// trip.
func TestEncodeConditionWitnessRoundTrip(t *testing.T) {
	t.Parallel()

	items := [][]byte{
		[]byte("secret preimage value"),
		[]byte("second witness item"),
	}

	encoded, err := EncodeConditionWitness(items)
	require.NoError(t, err)
	require.NotEmpty(t, encoded)

	decoded, err := DecodeConditionWitness(encoded)
	require.NoError(t, err)
	require.Equal(t, items, decoded)
}

// TestEncodeConditionWitnessEmpty tests encoding an empty witness vector.
func TestEncodeConditionWitnessEmpty(t *testing.T) {
	t.Parallel()

	encoded, err := EncodeConditionWitness(nil)
	require.NoError(t, err)
	require.NotEmpty(t, encoded) // Should have count byte.

	decoded, err := DecodeConditionWitness(encoded)
	require.NoError(t, err)
	require.Empty(t, decoded)
}

// TestDecodeConditionWitnessRejectsTrailingBytes verifies the decoder rejects
// extra data after the witness vector.
func TestDecodeConditionWitnessRejectsTrailingBytes(t *testing.T) {
	t.Parallel()

	encoded, err := EncodeConditionWitness([][]byte{[]byte("preimage")})
	require.NoError(t, err)

	_, err = DecodeConditionWitness(append(encoded, 0x00))
	require.ErrorContains(t, err, "trailing bytes")
}

// TestEncodeTapTreeLargeScript tests encoding with a larger script.
func TestEncodeTapTreeLargeScript(t *testing.T) {
	t.Parallel()

	// Create a script larger than 252 bytes to test compact size encoding.
	largeScript := make([]byte, 300)
	for i := range largeScript {
		largeScript[i] = byte(i % 256)
	}

	leaves := []PolicyLeaf{
		{
			Leaf: txscript.NewBaseTapLeaf(largeScript),
		},
	}

	policy, err := BuildTree(leaves, &ARKNUMSKey)
	require.NoError(t, err)

	encoded, err := EncodeTapTree(policy)
	require.NoError(t, err)

	decoded, err := DecodeTapTree(encoded)
	require.NoError(t, err)
	require.Len(t, decoded, 1)
	require.Equal(t, largeScript, decoded[0].Script)
}

// TestPSBTKeyConstants tests that PSBT key constants are correctly defined.
func TestPSBTKeyConstants(t *testing.T) {
	t.Parallel()

	require.Equal(t, "ark/", PSBTKeyPrefix)
	require.Equal(t, "ark/taptree", PSBTKeyTapTree)
	require.Equal(t, "ark/condition", PSBTKeyConditionWitness)
}
