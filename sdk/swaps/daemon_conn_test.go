package swaps

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/stretchr/testify/require"
)

// TestParseSessionTxidUsesChainhashOrder verifies that human-readable txids
// are converted back into the raw hash bytes expected on the RPC boundary.
func TestParseSessionTxidUsesChainhashOrder(t *testing.T) {
	t.Parallel()

	displayTxid := "34fb90915aabbbc4884d6076824ad809099dd41d" +
		"6085af74f7234c9f7243ea73"

	got, err := parseSessionTxid(displayTxid)
	require.NoError(t, err)

	hash, err := chainhash.NewHashFromStr(displayTxid)
	require.NoError(t, err)

	require.Equal(t, hash[:], got)
	require.NotEqual(t,
		[]byte{
			0x34, 0xfb, 0x90, 0x91, 0x5a, 0xab, 0xbb, 0xc4,
			0x88, 0x4d, 0x60, 0x76, 0x82, 0x4a, 0xd8, 0x09,
			0x09, 0x9d, 0xd4, 0x1d, 0x60, 0x85, 0xaf, 0x74,
			0xf7, 0x23, 0x4c, 0x9f, 0x72, 0x43, 0xea, 0x73,
		},
		got,
	)
}
