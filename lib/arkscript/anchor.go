package arkscript

import (
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

var (
	// AnchorPkScript is a standard P2A script. This is a keyless output.
	// This corresponds to address: bc1pfeessrawgf.
	// The script path for this output requires no witness elements,
	// meaning that it can be spent by anyone who provides the script and
	// control block.
	AnchorPkScript = []byte{
		txscript.OP_TRUE,
		txscript.OP_DATA_2,
		// The P2A identifier.
		0x4e, 0x73,
	}
)

// AnchorOutput returns a zero value P2A output. The returned PkScript is
// a defensive copy so callers cannot mutate the package-level global.
func AnchorOutput() *wire.TxOut {
	script := make([]byte, len(AnchorPkScript))
	copy(script, AnchorPkScript)

	return &wire.TxOut{
		PkScript: script,
		Value:    0,
	}
}
