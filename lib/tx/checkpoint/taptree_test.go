package checkpoint

import (
	"bytes"
	"testing"

	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// TestTapTreeRoundTrip asserts our v0 tap tree encoding is stable and
// round-trippable.
func TestTapTreeRoundTrip(t *testing.T) {
	t.Parallel()

	leaves := [][]byte{
		{0x51, 0x51, 0x51},
		{0x6a},
		{0x00, 0x01, 0x02, 0x03},
	}

	encoded, err := EncodeTapTree(leaves)
	require.NoError(t, err)

	decoded, err := DecodeTapTree(encoded)
	require.NoError(t, err)

	require.Equal(t, leaves, decoded)
}

// TestTapTreeDecodeIgnoresTrailingUnknownRecord verifies decode stays within
// the tapscript-leaves record boundary and ignores unknown odd records in the
// enclosing TLV stream.
func TestTapTreeDecodeIgnoresTrailingUnknownRecord(t *testing.T) {
	t.Parallel()

	encoded, err := EncodeTapTree([][]byte{{0x51}})
	require.NoError(t, err)

	var (
		buf     bytes.Buffer
		scratch [8]byte
	)

	_, err = buf.Write(encoded)
	require.NoError(t, err)

	// Append an unknown odd TLV record after the known records.
	err = tlv.WriteVarInt(&buf, 9, &scratch)
	require.NoError(t, err)

	err = tlv.WriteVarInt(&buf, 1, &scratch)
	require.NoError(t, err)

	_, err = buf.Write([]byte{0x99})
	require.NoError(t, err)

	decoded, err := DecodeTapTree(buf.Bytes())
	require.NoError(t, err)
	require.Equal(t, [][]byte{{0x51}}, decoded)
}
