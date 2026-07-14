package oor

import (
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/tx/arktx"
)

// ValidateFinalizePackage validates a v0 OOR finalize package.
//
// The finalize package is the set of checkpoint PSBTs returned by the client
// with the client's signatures applied. This function validates that:
//
//   - the Ark tx is present and canonical (needed for deterministic mapping),
//   - each Ark input spends checkpoint output (txid:vout=0),
//   - the provided checkpoint PSBTs correspond exactly to the set of checkpoint
//     txids referenced by the Ark inputs, and
//   - each checkpoint PSBT includes some finalized signature material.
//
// ValidateFinalizePackage is intentionally structural: it does not verify
// cryptographic signature correctness (that depends on VTXO scripts/policy).
// The caller is expected to perform full cryptographic validation with access
// to the correct tapscripts and key material.
func ValidateFinalizePackage(ark *psbt.Packet,
	finalCheckpoints []*psbt.Packet) error {

	switch {
	case ark == nil || ark.UnsignedTx == nil:
		return fmt.Errorf("ark psbt must include unsigned tx")

	case len(finalCheckpoints) == 0:
		return fmt.Errorf("final checkpoint psbts must be provided")
	}

	err := arktx.ValidateCanonicalPSBT(ark)
	if err != nil {
		return err
	}

	if len(ark.Inputs) != len(ark.UnsignedTx.TxIn) {
		return fmt.Errorf("ark psbt input count mismatch")
	}

	// Index the provided checkpoint PSBTs by txid so we can validate:
	//   - the set is unique (no duplicates); and
	//   - the set matches the Ark inputs exactly (no missing/extra txs).
	checkpointByTxid := make(
		map[chainhash.Hash]*psbt.Packet, len(finalCheckpoints),
	)

	for _, checkpoint := range finalCheckpoints {
		if checkpoint == nil || checkpoint.UnsignedTx == nil {
			return fmt.Errorf("checkpoint psbt must include " +
				"unsigned tx")
		}

		txid := checkpoint.UnsignedTx.TxHash()
		if _, exists := checkpointByTxid[txid]; exists {
			return fmt.Errorf("duplicate checkpoint txid: %s", txid)
		}

		if err := validateCheckpointTx(
			checkpoint.UnsignedTx,
		); err != nil {
			return fmt.Errorf("checkpoint %s invalid: %w", txid,
				err)
		}

		if len(checkpoint.Inputs) != len(checkpoint.UnsignedTx.TxIn) {
			return fmt.Errorf("checkpoint psbt input count " +
				"mismatch")
		}

		if len(checkpoint.Inputs) == 0 {
			return fmt.Errorf("checkpoint psbt has no inputs")
		}

		input := checkpoint.Inputs[0]
		hasFinalWitness := len(input.FinalScriptWitness) > 0 ||
			len(input.FinalScriptSig) > 0

		hasTaprootSigs := len(input.TaprootKeySpendSig) > 0 ||
			len(input.TaprootScriptSpendSig) > 0

		if !hasFinalWitness && !hasTaprootSigs {
			return fmt.Errorf("checkpoint %s missing finalize "+
				"signature material", txid)
		}

		checkpointByTxid[txid] = checkpoint
	}

	seen := make(map[wire.OutPoint]struct{}, len(ark.UnsignedTx.TxIn))
	for i, txIn := range ark.UnsignedTx.TxIn {
		prevOut := txIn.PreviousOutPoint

		if prevOut.Index != 0 {
			return fmt.Errorf("ark input %d spends checkpoint "+
				"output index %d, want 0", i, prevOut.Index)
		}

		_, ok := checkpointByTxid[prevOut.Hash]
		if !ok {
			return fmt.Errorf("ark input %d references unknown "+
				"checkpoint txid %s", i, prevOut.Hash)
		}

		if _, exists := seen[prevOut]; exists {
			return fmt.Errorf("duplicate checkpoint outpoint in "+
				"ark inputs: %s", prevOut)
		}

		seen[prevOut] = struct{}{}
	}

	if len(seen) != len(checkpointByTxid) {
		return fmt.Errorf("final checkpoint set does not match ark " +
			"inputs")
	}

	return nil
}
