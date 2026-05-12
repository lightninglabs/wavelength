package oor

import (
	"fmt"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/wire"
)

// CollectCheckpointInputs extracts the set of VTXO outpoints spent by the
// provided checkpoint PSBTs.
//
// In v0, each checkpoint transaction must spend exactly one VTXO input.
// The coordinator uses these outpoints as the session lock set.
func CollectCheckpointInputs(checkpoints []*psbt.Packet) ([]wire.OutPoint,
	error) {

	if len(checkpoints) == 0 {
		return nil, fmt.Errorf("checkpoint psbts must be provided")
	}

	seen := make(map[wire.OutPoint]struct{}, len(checkpoints))
	inputs := make([]wire.OutPoint, 0, len(checkpoints))

	for i, checkpoint := range checkpoints {
		if checkpoint == nil || checkpoint.UnsignedTx == nil {
			return nil, fmt.Errorf("checkpoint %d missing tx", i)
		}

		tx := checkpoint.UnsignedTx
		if len(tx.TxIn) != 1 {
			return nil, fmt.Errorf("checkpoint %d has %d "+
				"inputs, want 1", i, len(tx.TxIn))
		}

		outpoint := tx.TxIn[0].PreviousOutPoint

		_, exists := seen[outpoint]
		if exists {
			return nil, fmt.Errorf("duplicate checkpoint input: %s",
				outpoint)
		}

		seen[outpoint] = struct{}{}
		inputs = append(inputs, outpoint)
	}

	return inputs, nil
}
