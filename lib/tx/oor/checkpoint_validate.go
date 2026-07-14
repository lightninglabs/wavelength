package oor

import (
	"fmt"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/tx/arktx"
)

// validateCheckpointTx enforces the shared v0 checkpoint tx shape:
// output 0 is the spendable checkpoint output and the unique anchor output is
// last.
func validateCheckpointTx(tx *wire.MsgTx) error {
	if tx == nil {
		return fmt.Errorf("checkpoint tx must be provided")
	}

	if len(tx.TxOut) == 0 {
		return fmt.Errorf("checkpoint tx has no outputs")
	}

	anchorCount := 0
	anchorIndex := -1
	for i, out := range tx.TxOut {
		if !arktx.IsAnchorOutput(out) {
			continue
		}

		anchorCount++
		anchorIndex = i
	}

	if anchorCount != 1 {
		return fmt.Errorf("checkpoint tx must have exactly one anchor "+
			"output, got %d", anchorCount)
	}

	if len(tx.TxOut) < 2 {
		return fmt.Errorf("checkpoint tx must have at least two " +
			"outputs")
	}

	if arktx.IsAnchorOutput(tx.TxOut[0]) {
		return fmt.Errorf("checkpoint tx output 0 cannot be an " +
			"anchor output")
	}

	if anchorIndex != len(tx.TxOut)-1 {
		return fmt.Errorf("checkpoint tx anchor must be the last " +
			"output")
	}

	return nil
}
