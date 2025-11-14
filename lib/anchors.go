package lib

import "github.com/btcsuite/btcd/wire"

var (
	// ANCHOR_PKSCRIPT a standard P2A script. This is a keyless output.
	// This corresponds to address: bc1pfeessrawgf.
	// The witness for spending this output must be empty meaning that it
	// can be spent by anyone.
	ANCHOR_PKSCRIPT = []byte{
		0x51,       // OP_1
		0x02,       // Len: 2 bytes
		0x4e, 0x73, // The P2A identifier.
	}
)

// AnchorOutput returns a zero value P2A output.
func AnchorOutput() *wire.TxOut {
	return &wire.TxOut{
		PkScript: ANCHOR_PKSCRIPT,
		Value:    0,
	}
}
