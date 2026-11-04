package arkscript

import (
	"bytes"
	"io"
	"math"
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
)

// tapTreeLengthPayload encodes a single leaf with a caller-chosen declared
// length and actual script bytes, without allocating the declared size.
func tapTreeLengthPayload(t testing.TB, declared uint64, script []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.Write([]byte{1, 0, 0xc0})
	require.NoError(t, wire.WriteVarInt(&buf, 0, declared))
	buf.Write(script)

	return buf.Bytes()
}

// TestDecodeTapTreeLengthBudget proves truncated declarations fail at the
// allocation guard, including values that cannot be represented as an int.
func TestDecodeTapTreeLengthBudget(t *testing.T) {
	t.Parallel()
	for _, size := range []uint64{1, 1 << 20, math.MaxInt32 + 1,
		math.MaxInt64, math.MaxUint64} {
		data := tapTreeLengthPayload(t, size, nil)
		leaves, err := DecodeTapTree(data)
		require.ErrorIs(t, err, io.ErrUnexpectedEOF)
		require.ErrorContains(t, err, "exceeds remaining")
		require.Nil(t, leaves)
	}
}

// TestDecodeTapTreeSharedBudget proves later leaves cannot reuse the bytes
// consumed by earlier leaves, while the existing leaf cap remains inclusive.
func TestDecodeTapTreeSharedBudget(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	require.NoError(t, wire.WriteVarInt(&buf, 0, maxTapTreeLeaves))
	for i := 0; i < maxTapTreeLeaves; i++ {
		buf.Write([]byte{8, 0xc0, 1, 0x51})
	}
	data := buf.Bytes()
	leaves, err := DecodeTapTree(data)
	require.NoError(t, err)
	require.Len(t, leaves, maxTapTreeLeaves)
	for _, leaf := range leaves {
		require.Equal(t, []byte{0x51}, leaf.Script)
	}
	// The final script is absent although the rest of the tree is valid.
	_, err = DecodeTapTree(data[:len(data)-1])
	require.ErrorContains(t, err, "exceeds remaining")
	// A count beyond the established cap is rejected before leaf
	// allocation.
	buf.Reset()
	require.NoError(t, wire.WriteVarInt(&buf, 0, maxTapTreeLeaves+1))
	_, err = DecodeTapTree(buf.Bytes())
	require.ErrorContains(t, err, "leaf count")
}

// FuzzDecodeTapTree checks allocation safety and the shared script budget for
// arbitrary compact-size lengths, truncations, and multi-leaf encodings.
func FuzzDecodeTapTree(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{1, 0, 0xc0, 0})
	f.Add([]byte{2, 1, 0xc0, 1, 0x51, 1, 0xc0, 1, 0x52})
	for _, size := range []uint64{1 << 20, math.MaxInt32 + 1,
		math.MaxInt64, math.MaxUint64} {
		f.Add(tapTreeLengthPayload(f, size, nil))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		leaves, err := DecodeTapTree(data)
		if err != nil {
			return
		}
		require.LessOrEqual(t, len(leaves), maxTapTreeLeaves)
		total := 0
		for _, leaf := range leaves {
			total += len(leaf.Script)
		}
		require.LessOrEqual(t, total, len(data))
	})
}
