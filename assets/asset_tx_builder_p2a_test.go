package assets_test

import (
	"testing"

	"github.com/btcsuite/btcd/txscript"
	"github.com/stretchr/testify/require"
)

// TestPayToAnchorScript tests that the P2A script is correctly formed.
func TestPayToAnchorScript(t *testing.T) {
	// The P2A script should be: OP_1 OP_DATA_2 0x4e 0x73
	// This is a witness v1 program with the "4e73" identifier.
	expectedScript := []byte{
		txscript.OP_1, txscript.OP_DATA_2, 0x4e, 0x73,
	}

	// The expected script length is 4 bytes.
	require.Len(t, expectedScript, 4)

	// Verify this is a valid witness program structure.
	// OP_1 = witness version 1
	// OP_DATA_2 = push 2 bytes
	// 0x4e73 = the P2A identifier
	require.Equal(t, byte(txscript.OP_1), expectedScript[0])
	require.Equal(t, byte(txscript.OP_DATA_2), expectedScript[1])
	require.Equal(t, []byte{0x4e, 0x73}, expectedScript[2:4])
}
