package oor

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/tx/arktx"
)

// ValidatedSubmitPackage contains derived facts from a submit package that are
// useful to higher layers (server coordinator, client FSM, tests).
type ValidatedSubmitPackage struct {
	// ArkTxid is the txid of the unsigned Ark tx.
	//
	// This is also the v0 session identifier for submit/finalize.
	ArkTxid chainhash.Hash

	// CheckpointOutpoints are the checkpoint outputs (txid:vout=0) that the
	// Ark tx spends, in the same order as Ark tx inputs.
	CheckpointOutpoints []wire.OutPoint
}

// ValidateSubmitPackage validates a v0 OOR submit package.
//
// This is a structural validator. It validates that:
//
//   - the Ark PSBT is present and canonical (anchor last, one anchor, canonical
//     input/output ordering),
//   - each Ark tx input spends a checkpoint tx output (txid:vout=0),
//   - the provided checkpoint txs cover all Ark inputs (no missing or extra),
//   - each Ark PSBT input has a witness UTXO that matches the referenced
//     checkpoint tx output 0 (script + value), and
//   - each checkpoint PSBT already carries standard output tap tree metadata.
//
// ValidateSubmitPackage does not validate:
//
// - client or operator signatures,
// - whether the checkpoint txs correctly spend live VTXOs,
// - whether checkpoint scripts match operator policy, or
// - VTXO set state / locking.
//
// Those checks belong to higher layers that have access to policy and state.
func ValidateSubmitPackage(ark *psbt.Packet,
	checkpoints []*psbt.Packet) (*ValidatedSubmitPackage, error) {

	switch {
	case ark == nil || ark.UnsignedTx == nil:
		return nil, fmt.Errorf("ark psbt must include unsigned tx")

	case len(checkpoints) == 0:
		return nil, fmt.Errorf("checkpoint psbts must be provided")
	}

	err := arktx.ValidateCanonicalPSBT(ark)
	if err != nil {
		return nil, err
	}

	if len(ark.Inputs) != len(ark.UnsignedTx.TxIn) {
		return nil, fmt.Errorf("ark psbt input count mismatch")
	}

	// Index checkpoint PSBTs by txid so we can:
	//   - check for duplicates early; and
	//   - validate that the checkpoint set is exactly the set referenced
	//     by the Ark tx inputs (no missing or extra checkpoints).
	checkpointByTxid := make(
		map[chainhash.Hash]*psbt.Packet, len(checkpoints),
	)
	for _, checkpoint := range checkpoints {
		if checkpoint == nil || checkpoint.UnsignedTx == nil {
			return nil, fmt.Errorf("checkpoint psbt must include " +
				"unsigned tx")
		}

		checkpointTxid := checkpoint.UnsignedTx.TxHash()
		if _, exists := checkpointByTxid[checkpointTxid]; exists {
			return nil, fmt.Errorf("duplicate checkpoint txid: %s",
				checkpointTxid)
		}

		if err := validateCheckpointTx(
			checkpoint.UnsignedTx,
		); err != nil {
			return nil, fmt.Errorf("checkpoint %s invalid: %w",
				checkpointTxid, err)
		}

		checkpointByTxid[checkpointTxid] = checkpoint
	}

	seenCheckpoint := make(
		map[wire.OutPoint]struct{}, len(ark.UnsignedTx.TxIn),
	)
	outpoints := make([]wire.OutPoint, 0, len(ark.UnsignedTx.TxIn))

	for i, txIn := range ark.UnsignedTx.TxIn {
		prevOut := txIn.PreviousOutPoint

		// v0 assumes each Ark input spends vout=0 of the checkpoint tx.
		//
		// This gives a canonical mapping between checkpoint txs and Ark
		// inputs, without needing per-input metadata.
		if prevOut.Index != 0 {
			return nil, fmt.Errorf("ark input %d spends "+
				"checkpoint output index %d, want 0", i,
				prevOut.Index)
		}

		checkpointPkt, ok := checkpointByTxid[prevOut.Hash]
		if !ok {
			return nil, fmt.Errorf("ark input %d references "+
				"unknown checkpoint txid %s", i, prevOut.Hash)
		}

		_, exists := seenCheckpoint[prevOut]
		if exists {
			return nil, fmt.Errorf("duplicate checkpoint outpoint "+
				"in ark inputs: %s", prevOut)
		}

		seenCheckpoint[prevOut] = struct{}{}
		outpoints = append(outpoints, prevOut)

		// Require witness UTXOs so the package is self-contained.
		// The receiver should not need to fetch prevouts from chain to
		// validate the PSBT structure.
		witnessUtxo := ark.Inputs[i].WitnessUtxo
		if witnessUtxo == nil {
			return nil, fmt.Errorf("ark input %d missing "+
				"witness utxo", i)
		}

		checkpointOut := checkpointPkt.UnsignedTx.TxOut[0]
		if witnessUtxo.Value != checkpointOut.Value {
			return nil, fmt.Errorf("ark input %d witness utxo "+
				"value mismatch", i)
		}

		if !bytes.Equal(witnessUtxo.PkScript, checkpointOut.PkScript) {
			return nil, fmt.Errorf("ark input %d witness utxo "+
				"script mismatch", i)
		}

		if len(checkpointPkt.Outputs) == 0 {
			return nil, fmt.Errorf("checkpoint psbt has no outputs")
		}

		if len(checkpointPkt.Outputs[0].TaprootTapTree) == 0 {
			return nil, fmt.Errorf("checkpoint %s missing output "+
				"tap tree metadata", prevOut.Hash)
		}
	}

	// Ensure the checkpoint set is exactly the set referenced by Ark
	// inputs.
	//
	// We allow extra checkpoint PSBTs to be rejected here so callers can
	// rely on "checkpoint list is session-complete" semantics.
	if len(seenCheckpoint) != len(checkpointByTxid) {
		return nil, fmt.Errorf("checkpoint set does not match ark " +
			"inputs")
	}

	arkTxid := ark.UnsignedTx.TxHash()

	return &ValidatedSubmitPackage{
		ArkTxid:             arkTxid,
		CheckpointOutpoints: outpoints,
	}, nil
}
