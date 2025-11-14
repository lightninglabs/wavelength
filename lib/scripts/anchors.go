package scripts

import (
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

var (
	// AnchorPkScript a standard P2A script. This is a keyless output.
	// This corresponds to address: bc1pfeessrawgf.
	// The script path for this output requires no witness elements, meaning
	// that it can be spent by anyone who provides the script and control
	// block.
	AnchorPkScript = []byte{
		txscript.OP_1,
		txscript.OP_DATA_2,
		// The P2A identifier.
		0x4e, 0x73,
	}
)

// AnchorOutput returns a zero value P2A output.
func AnchorOutput() *wire.TxOut {
	return &wire.TxOut{
		PkScript: AnchorPkScript,
		Value:    0,
	}
}
