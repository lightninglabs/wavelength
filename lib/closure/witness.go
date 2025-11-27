package closure

import (
	"bytes"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// ReadTxWitness deserializes a witness from a byte slice.
func ReadTxWitness(witnessSerialized []byte) (wire.TxWitness, error) {
	r := bytes.NewReader(witnessSerialized)

	// first we extract the number of witness elements
	witCount, err := wire.ReadVarInt(r, 0)
	if err != nil {
		return nil, err
	}

	// read each witness item
	witness := make(wire.TxWitness, witCount)
	for i := uint64(0); i < witCount; i++ {
		witness[i], err = wire.ReadVarBytes(r, 0, txscript.MaxScriptSize, "witness")
		if err != nil {
			return nil, err
		}
	}

	return witness, nil
}
