package oor

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/wire"
)

// ApplyFinalizeData attaches per-input tap tree metadata from the Ark tx PSBT
// to the corresponding checkpoint PSBT output.
//
// In OOR transfers, finalization should not mutate the unsigned transactions;
// it should only attach output metadata needed for later auditing/tracing. This
// function therefore:
//
//   - maps Ark inputs to checkpoint txs by matching Ark input prevouts to
//     (checkpoint_txid, vout=0), and
//   - sets `checkpoint.Outputs[0].TaprootTapTree` to the encoded tap tree blob
//     extracted from the corresponding Ark PSBT input unknown field.
//
// The caller should treat the resulting checkpoint PSBTs as the canonical
// checkpoint representation to persist.
func ApplyFinalizeData(ark *psbt.Packet,
	checkpoints []*psbt.Packet) error {

	switch {
	case ark == nil || ark.UnsignedTx == nil:
		return fmt.Errorf("ark psbt must include unsigned tx")

	case len(ark.Inputs) != len(ark.UnsignedTx.TxIn):
		return fmt.Errorf("ark psbt input count mismatch")

	case len(checkpoints) == 0:
		return fmt.Errorf("checkpoint psbts must be provided")
	}

	checkpointByOutpoint := make(
		map[wire.OutPoint]*psbt.Packet, len(checkpoints),
	)

	for _, checkpoint := range checkpoints {
		if checkpoint == nil || checkpoint.UnsignedTx == nil {
			return fmt.Errorf("checkpoint psbt must include " +
				"unsigned tx")
		}

		if len(checkpoint.UnsignedTx.TxOut) == 0 {
			return fmt.Errorf("checkpoint tx has no outputs")
		}

		if len(checkpoint.Outputs) != len(checkpoint.UnsignedTx.TxOut) {
			return fmt.Errorf("checkpoint psbt output count " +
				"mismatch")
		}

		if len(checkpoint.Outputs) == 0 {
			return fmt.Errorf("checkpoint psbt has no outputs")
		}

		checkpointTxid := checkpoint.UnsignedTx.TxHash()
		outpoint := wire.OutPoint{
			Hash:  checkpointTxid,
			Index: 0,
		}

		_, exists := checkpointByOutpoint[outpoint]
		if exists {
			return fmt.Errorf("duplicate checkpoint txid: %s",
				checkpointTxid)
		}

		checkpointByOutpoint[outpoint] = checkpoint
	}

	for i, txIn := range ark.UnsignedTx.TxIn {
		prevOut := txIn.PreviousOutPoint

		checkpoint, ok := checkpointByOutpoint[prevOut]
		if !ok {
			return fmt.Errorf("ark input %d references "+
				"unknown checkpoint outpoint %s", i,
				prevOut.String())
		}

		encodedTapTree, err := GetTapTreePSBTInput(ark.Inputs[i])
		if err != nil {
			return fmt.Errorf("ark input %d missing tap tree "+
				"metadata: %w", i, err)
		}

		if len(checkpoint.Outputs) == 0 {
			return fmt.Errorf("checkpoint psbt has no outputs")
		}

		if len(checkpoint.Outputs[0].TaprootTapTree) != 0 &&
			!bytes.Equal(
				checkpoint.Outputs[0].TaprootTapTree,
				encodedTapTree,
			) {

			return fmt.Errorf("checkpoint already has a " +
				"different tap tree")
		}

		checkpoint.Outputs[0].TaprootTapTree = encodedTapTree
	}

	return nil
}
