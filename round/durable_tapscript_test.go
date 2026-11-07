package round

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/stretchr/testify/require"
)

// TestDurableTapscript preserves non-base leaves and every wallet variant.
func TestDurableTapscript(t *testing.T) {
	_, key := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{7}, 32))
	for kind := uint8(0); kind <= 3; kind++ {
		original := &waddrmgr.Tapscript{
			Type: waddrmgr.TapscriptType(kind),
			ControlBlock: &txscript.ControlBlock{
				InternalKey: key, OutputKeyYIsOdd: true,
				LeafVersion:    txscript.BaseLeafVersion,
				InclusionProof: bytes.Repeat([]byte{9}, 32),
			},
			Leaves: []txscript.TapLeaf{
				{
					LeafVersion: 0xc2,
					Script: []byte{
						0x51,
					},
				},
				{
					LeafVersion: 0xc0,
					Script: []byte{
						0x52,
					},
				},
			},
			RevealedScript: []byte{
				0x53,
			},
			RootHash:      bytes.Repeat([]byte{3}, 32),
			FullOutputKey: key,
		}
		raw, err := encodeDurableTapscript(original)
		require.NoError(t, err)
		restored, err := decodeDurableTapscript(raw)
		require.NoError(t, err)
		require.Equal(t, original, restored)
		again, err := encodeDurableTapscript(restored)
		require.NoError(t, err)
		require.Equal(t, raw, again)
	}
	raw, err := encodeDurableTapscript(nil)
	require.NoError(t, err)
	restored, err := decodeDurableTapscript(raw)
	require.NoError(t, err)
	require.Nil(t, restored)
}
