package db

import (
	"fmt"

	"github.com/btcsuite/btcd/txscript"
)

const (
	compressedEvenYPrefix = 0x02
	taprootScriptLen      = 34
	taprootKeyLen         = 32
)

// cosignerFromPkScript derives compressed pubkey bytes from a P2TR script.
//
// VTXOs in this system are taproot outputs. The script commits to a 32-byte
// x-only key. BIP-340 uses even-Y pubkeys for x-only encoding, so we can
// reconstruct the compressed form by prefixing 0x02.
func cosignerFromPkScript(pkScript []byte) ([]byte, error) {
	if len(pkScript) != taprootScriptLen {
		return nil, fmt.Errorf(
			"pkScript must be %d bytes", taprootScriptLen,
		)
	}

	if pkScript[0] != txscript.OP_1 || pkScript[1] != txscript.OP_DATA_32 {
		return nil, fmt.Errorf("pkScript must be p2tr")
	}

	pubkey := make([]byte, 1+taprootKeyLen)
	pubkey[0] = compressedEvenYPrefix
	copy(pubkey[1:], pkScript[2:])

	return pubkey, nil
}
