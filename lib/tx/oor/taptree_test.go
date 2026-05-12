package oor

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestTapTreeRoundTrip asserts our v0 tap tree encoding is stable and
// round-trippable.
func TestTapTreeRoundTrip(t *testing.T) {
	t.Parallel()

	leaves := [][]byte{
		{
			0x51,
			0x51,
			0x51,
		},
		{
			0x6a,
		},
		{
			0x00,
			0x01,
			0x02,
			0x03,
		},
	}

	encoded, err := EncodeTapTree(leaves)
	require.NoError(t, err)

	decoded, err := DecodeTapTree(encoded)
	require.NoError(t, err)

	require.Equal(t, leaves, decoded)
}
