package oor

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
)

// extractCheckpointTx returns the broadcastable transaction from a finalized
// OOR checkpoint PSBT. The OOR finalize path validates signatures but, for
// the standard collaborative leaf, does not assemble the witness into
// FinalScriptWitness — that field is only set for custom spends (e.g. vHTLC
// with condition witness items). This helper handles both cases:
//
//   - If FinalScriptWitness is present, parse it as a wire.TxWitness and
//     attach it to a copy of the unsigned transaction.
//   - Otherwise, reconstruct the witness from TaprootScriptSpendSig and
//     TaprootLeafScript using the leaf script's pubkey order.
//
// The returned transaction is suitable for broadcast through txconfirm.
func extractCheckpointTx(pkt *psbt.Packet) (*wire.MsgTx, error) {
	if pkt == nil || pkt.UnsignedTx == nil {
		return nil, fmt.Errorf("checkpoint psbt missing unsigned tx")
	}
	if len(pkt.Inputs) != 1 || len(pkt.UnsignedTx.TxIn) != 1 {
		return nil, fmt.Errorf("checkpoint psbt must have one input")
	}

	tx := pkt.UnsignedTx.Copy()
	in := &pkt.Inputs[0]

	// Custom spends (e.g. vHTLC claim) carry the fully assembled witness
	// in FinalScriptWitness. The OOR finalize path persists the PSBT
	// as-is for these, so we can use it directly.
	if len(in.FinalScriptWitness) > 0 {
		witness, err := parseFinalScriptWitness(in.FinalScriptWitness)
		if err != nil {
			return nil, fmt.Errorf("parse final script witness: %w",
				err)
		}

		tx.TxIn[0].Witness = witness

		return tx, nil
	}

	// Standard collaborative spends only carry partial taproot signatures
	// and the tap leaf script + control block. Before assembling the
	// witness, bind the persisted tap leaf to the on-chain pkScript so a
	// row that pairs a valid signature set with a leaf unrelated to the
	// prevout fails fast here rather than at mempool admission. Mirrors
	// the symmetric VerifyBindsToPkScript call on the sweep build path
	// (fraud/sweep.go).
	if in.WitnessUtxo == nil ||
		len(in.WitnessUtxo.PkScript) == 0 {
		return nil, fmt.Errorf("checkpoint psbt missing WitnessUtxo " +
			"for binding check")
	}
	if len(in.TaprootLeafScript) != 1 {
		return nil, fmt.Errorf("checkpoint psbt has %d tap "+
			"leaves, want 1", len(in.TaprootLeafScript))
	}
	leaf := in.TaprootLeafScript[0]
	if leaf == nil || len(leaf.Script) == 0 ||
		len(leaf.ControlBlock) == 0 {
		return nil, fmt.Errorf("checkpoint psbt has empty tap leaf " +
			"script or control block")
	}
	bindCheck := &arkscript.SpendPath{
		SpendInfo: &arkscript.SpendInfo{
			WitnessScript: leaf.Script,
			ControlBlock:  leaf.ControlBlock,
		},
	}
	if err := bindCheck.VerifyBindsToPkScript(
		in.WitnessUtxo.PkScript,
	); err != nil {
		return nil, fmt.Errorf("checkpoint leaf does not bind to "+
			"prevout pkScript: %w", err)
	}

	witness, err := assembleCollaborativeWitness(in)
	if err != nil {
		return nil, fmt.Errorf("assemble collaborative witness: %w",
			err)
	}

	tx.TxIn[0].Witness = witness

	return tx, nil
}

// assembleCollaborativeWitness builds the spend witness for an OOR
// collaborative checkpoint input from its persisted TaprootScriptSpendSig and
// TaprootLeafScript fields.
//
// The leaf script has the canonical CHECKSIGVERIFY-chain shape produced by
// arkscript.Multisig: <k0> CHECKSIGVERIFY <k1> CHECKSIGVERIFY ... <kN-1>
// CHECKSIG. The witness order required by tapscript execution is the reverse:
// the signature for the LAST script key sits at the bottom of the stack and
// the signature for the FIRST script key sits on top.
func assembleCollaborativeWitness(in *psbt.PInput) (wire.TxWitness, error) {
	if len(in.TaprootLeafScript) != 1 {
		return nil, fmt.Errorf("checkpoint psbt has %d tap "+
			"leaves, want 1", len(in.TaprootLeafScript))
	}

	leaf := in.TaprootLeafScript[0]
	if leaf == nil || len(leaf.Script) == 0 {
		return nil, fmt.Errorf("tap leaf script is empty")
	}
	if len(leaf.ControlBlock) == 0 {
		return nil, fmt.Errorf("tap leaf control block is empty")
	}

	leafHash := txscript.NewBaseTapLeaf(leaf.Script).TapHash()
	leafHashBytes := leafHash[:]

	// Extract the ordered list of x-only pubkeys pushed by the leaf
	// script. Tapscript multisig uses 32-byte schnorr keys, so every
	// 32-byte data push is a candidate signer.
	keys, err := extractLeafXOnlyKeys(leaf.Script)
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("tap leaf script has no signer keys")
	}

	// For each key in REVERSE script order, find the matching
	// TaprootScriptSpendSig and append its signature to the witness. The
	// reverse order is what tapscript execution requires: the last key in
	// the script is the first one consumed.
	witness := make(wire.TxWitness, 0, len(keys)+2)
	for i := len(keys) - 1; i >= 0; i-- {
		sig, err := findTaprootSig(in, keys[i], leafHashBytes)
		if err != nil {
			return nil, fmt.Errorf("signature for key %x: %w",
				keys[i], err)
		}

		witness = append(
			witness, appendTaprootSigHashByte(
				sig.Signature, sig.SigHash,
			),
		)
	}

	witness = append(witness, leaf.Script)
	witness = append(witness, leaf.ControlBlock)

	return witness, nil
}

// extractLeafXOnlyKeys returns every 32-byte data push in the leaf script in
// the order they appear. For canonical arkscript.Multisig leaves, this is the
// signer key list passed to Multisig{Keys: ...}.
func extractLeafXOnlyKeys(script []byte) ([][]byte, error) {
	tokenizer := txscript.MakeScriptTokenizer(0, script)

	var keys [][]byte
	for tokenizer.Next() {
		if tokenizer.Opcode() != txscript.OP_DATA_32 {
			continue
		}

		data := tokenizer.Data()
		if len(data) != schnorr.PubKeyBytesLen {
			continue
		}

		keys = append(keys, append([]byte(nil), data...))
	}
	if err := tokenizer.Err(); err != nil {
		return nil, fmt.Errorf("tokenize leaf script: %w", err)
	}

	return keys, nil
}

// findTaprootSig returns the partial taproot signature recorded for keyBytes
// against leafHash, or an error if no matching record exists.
func findTaprootSig(in *psbt.PInput, keyBytes []byte,
	leafHash []byte) (*psbt.TaprootScriptSpendSig, error) {

	for _, sig := range in.TaprootScriptSpendSig {
		if sig == nil {
			continue
		}
		if !bytes.Equal(sig.LeafHash, leafHash) {
			continue
		}
		if !bytes.Equal(sig.XOnlyPubKey, keyBytes) {
			continue
		}

		return sig, nil
	}

	return nil, fmt.Errorf("no signature recorded")
}

// appendTaprootSigHashByte mirrors the OOR finalize witness assembly: schnorr
// signatures with the default sighash type are 64 bytes, while non-default
// types append the 1-byte sighash value to produce a 65-byte witness item.
func appendTaprootSigHashByte(sig []byte, sigHash txscript.SigHashType) []byte {
	if sigHash == txscript.SigHashDefault {
		return append([]byte(nil), sig...)
	}

	out := make([]byte, 0, len(sig)+1)
	out = append(out, sig...)
	out = append(out, byte(sigHash))

	return out
}
