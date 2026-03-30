package arkscript

import (
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/stretchr/testify/require"
)

// TestNUMSKeyIsValid verifies the NUMS key is a valid secp256k1 point.
func TestNUMSKeyIsValid(t *testing.T) {
	t.Parallel()

	keyBytes := ARKNUMSKey.SerializeCompressed()
	require.Len(t, keyBytes, 33)

	// Re-parse to confirm it's a valid curve point.
	parsed, err := btcec.ParsePubKey(keyBytes)
	require.NoError(t, err)

	require.Equal(t, keyBytes, parsed.SerializeCompressed())
}

// TestNUMSKeyMatchesDocumented verifies the NUMS key matches the
// documented hex constant.
func TestNUMSKeyMatchesDocumented(t *testing.T) {
	t.Parallel()

	expected := ARKNUMSHex
	actual := hex.EncodeToString(ARKNUMSKey.SerializeCompressed())

	require.Equal(t, expected, actual)
}

// TestNUMSKeyDeterministic verifies that re-parsing the hex always
// produces the same key.
func TestNUMSKeyDeterministic(t *testing.T) {
	t.Parallel()

	keyBytes, err := hex.DecodeString(ARKNUMSHex)
	require.NoError(t, err)

	key1, err := btcec.ParsePubKey(keyBytes)
	require.NoError(t, err)

	key2, err := btcec.ParsePubKey(keyBytes)
	require.NoError(t, err)

	require.Equal(t,
		key1.SerializeCompressed(),
		key2.SerializeCompressed(),
	)
}
