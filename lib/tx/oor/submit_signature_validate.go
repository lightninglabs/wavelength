package oor

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
)

// ValidateSubmitPackageSigned validates a v0 OOR submit package including
// signature correctness and script VM execution for the Ark PSBT.
//
// This performs structural validation and then executes the tapscript VM for
// each Ark input using the witness data embedded in the PSBT.
func ValidateSubmitPackageSigned(ark *psbt.Packet,
	checkpoints []*psbt.Packet) (*ValidatedSubmitPackage, error) {

	validated, err := ValidateSubmitPackage(ark, checkpoints)
	if err != nil {
		return nil, err
	}

	if err := validatePSBTSpends(ark, "ark"); err != nil {
		return nil, err
	}

	return validated, nil
}

// ValidateFinalizePackageSigned validates a v0 OOR finalize package including
// signature correctness and script VM execution for checkpoint PSBTs.
//
// This builds the final witness stacks for the checkpoints and executes the
// script VM to confirm the finalized package is spendable on-chain.
func ValidateFinalizePackageSigned(ark *psbt.Packet,
	finalCheckpoints []*psbt.Packet) error {

	if err := ValidateFinalizePackage(ark, finalCheckpoints); err != nil {
		return err
	}

	for i, checkpoint := range finalCheckpoints {
		if err := validatePSBTSpends(
			checkpoint, fmt.Sprintf("final checkpoint %d", i),
		); err != nil {
			return err
		}
	}

	return nil
}

// validatePSBTSpends constructs witnesses from PSBT metadata and executes the
// script VM for each input, returning an annotated error on the first failure.
func validatePSBTSpends(pkt *psbt.Packet, label string) error {
	switch {
	case pkt == nil || pkt.UnsignedTx == nil:
		return fmt.Errorf("%s psbt must include unsigned tx", label)

	case len(pkt.Inputs) != len(pkt.UnsignedTx.TxIn):
		return fmt.Errorf("%s psbt input count mismatch", label)
	}

	tx := pkt.UnsignedTx.Copy()

	prevOuts := make(map[wire.OutPoint]*wire.TxOut, len(tx.TxIn))
	for i, txIn := range tx.TxIn {
		in := pkt.Inputs[i]
		if in.WitnessUtxo == nil {
			return fmt.Errorf("%s input %d missing witness utxo",
				label, i)
		}

		prevOuts[txIn.PreviousOutPoint] = in.WitnessUtxo

		witness, err := buildTaprootWitness(in)
		if err != nil {
			return fmt.Errorf("%s input %d: %w", label, i, err)
		}

		txIn.Witness = witness
	}

	prevFetcher := txscript.NewMultiPrevOutFetcher(prevOuts)
	sigHashes := txscript.NewTxSigHashes(tx, prevFetcher)

	for i, txIn := range tx.TxIn {
		prevOut := prevFetcher.FetchPrevOutput(
			txIn.PreviousOutPoint,
		)
		if prevOut == nil {
			return fmt.Errorf("%s input %d missing prevout", label,
				i)
		}

		engine, err := txscript.NewEngine(
			prevOut.PkScript, tx, i, txscript.StandardVerifyFlags,
			nil, sigHashes, prevOut.Value, prevFetcher,
		)
		if err != nil {
			return fmt.Errorf("%s input %d: create script "+
				"engine: %w", label, i, err)
		}

		if err := engine.Execute(); err != nil {
			return fmt.Errorf("%s input %d: script validation "+
				"failed: %w", label, i, err)
		}
	}

	return nil
}

// BuildTaprootWitness constructs the witness stack for a taproot input using
// any finalized witness, key spend signature, or script spend signature data
// available in the PSBT. It also understands Ark condition-witness metadata,
// so callers that need the exact wire transaction for an OOR package should use
// this instead of generic PSBT finalization.
func BuildTaprootWitness(in psbt.PInput) (wire.TxWitness, error) {
	if len(in.FinalScriptWitness) > 0 {
		return parseFinalWitness(in.FinalScriptWitness)
	}

	if len(in.TaprootKeySpendSig) > 0 {
		return wire.TxWitness{
			appendTaprootSigHash(
				in.TaprootKeySpendSig, in.SighashType,
			),
		}, nil
	}

	if len(in.TaprootScriptSpendSig) > 0 {
		targetLeafHash := in.TaprootScriptSpendSig[0].LeafHash
		for i := range in.TaprootScriptSpendSig {
			sig := in.TaprootScriptSpendSig[i]
			if sig == nil {
				return nil, fmt.Errorf("nil taproot signature")
			}

			if !bytes.Equal(sig.LeafHash, targetLeafHash) {
				return nil, fmt.Errorf("taproot script " +
					"signatures reference multiple leaf " +
					"hashes")
			}
		}

		leafScript, err := findTaprootLeafScript(in, targetLeafHash)
		if err != nil {
			return nil, err
		}

		sigItems, err := orderTaprootScriptSpendSignatures(
			in.TaprootScriptSpendSig, leafScript.Script,
		)
		if err != nil {
			return nil, err
		}

		witness := make(wire.TxWitness, 0,
			len(sigItems)+3,
		)
		for i := range sigItems {
			witness = append(witness, sigItems[i])
		}

		// Condition witness items are appended after the signatures
		// and before the leaf script. This layout matches Ark policy
		// leaves where the tapscript consumes signatures from the top
		// of the stack and then consumes condition data pushed below
		// them (e.g. vHTLC claim: <sig> <preimage> OP_SHA256 <hash>
		// OP_EQUALVERIFY <k> CHECKSIG). Any future policy whose script
		// consumes condition stack items above the signatures would
		// need a different ordering; add an explicit position hint
		// next to the condition witness when that case appears.
		conditionWitness, err := arkscript.GetConditionWitnessPSBTInput(
			in,
		)
		switch {
		case err == nil:
			witness = append(witness, conditionWitness...)

		case errors.Is(err, arkscript.ErrConditionWitnessNotFound):
			// Plain collaborative leaves do not carry extra
			// condition witness material, so missing Ark
			// condition metadata is fine.

		default:
			return nil, fmt.Errorf("decode condition witness: %w",
				err)
		}

		witness = append(witness, leafScript.Script)
		witness = append(witness, leafScript.ControlBlock)

		return witness, nil
	}

	if len(in.TaprootLeafScript) == 1 {
		leaf := in.TaprootLeafScript[0]
		if leaf == nil {
			return nil, fmt.Errorf("taproot leaf script missing")
		}

		return wire.TxWitness{
			leaf.Script,
			leaf.ControlBlock,
		}, nil
	}

	return nil, fmt.Errorf("missing taproot signature or leaf script")
}

// buildTaprootWitness constructs the witness stack for a taproot input using
// the exported OOR-aware witness builder.
func buildTaprootWitness(in psbt.PInput) (wire.TxWitness, error) {
	return BuildTaprootWitness(in)
}

// orderTaprootScriptSpendSignatures reorders script-spend signatures into the
// witness order required by CHECKSIG/CHECKSIGVERIFY chains. Arkscript
// multisig leaves compile as <k0> CHECKSIGVERIFY <k1> CHECKSIG..., which means
// witness signatures must appear in reverse key order on the stack.
func orderTaprootScriptSpendSignatures(sigs []*psbt.TaprootScriptSpendSig,
	leafScript []byte) ([][]byte, error) {

	if len(sigs) == 0 {
		return nil, fmt.Errorf("missing taproot script signatures")
	}

	keys, err := extractChecksigPubKeys(leafScript)
	if err != nil {
		return nil, err
	}

	if len(keys) == 0 {
		ordered := make([][]byte, 0, len(sigs))
		for i := range sigs {
			sig := sigs[i]
			ordered = append(
				ordered, appendTaprootSigHash(
					sig.Signature, sig.SigHash,
				),
			)
		}

		return ordered, nil
	}

	sigByPubKey := make(map[string][]byte, len(sigs))
	for i := range sigs {
		sig := sigs[i]
		if sig == nil {
			return nil, fmt.Errorf("nil taproot signature")
		}

		key := string(sig.XOnlyPubKey)
		if _, ok := sigByPubKey[key]; ok {
			return nil, fmt.Errorf("duplicate taproot signature " +
				"for pubkey")
		}

		sigByPubKey[key] = appendTaprootSigHash(
			sig.Signature, sig.SigHash,
		)
	}

	ordered := make([][]byte, 0, len(keys))
	for i := len(keys) - 1; i >= 0; i-- {
		sig, ok := sigByPubKey[string(keys[i])]
		if !ok {
			ordered = append(ordered, nil)
			continue
		}

		ordered = append(ordered, sig)
		delete(sigByPubKey, string(keys[i]))
	}

	if len(sigByPubKey) != 0 {
		return nil, fmt.Errorf("could not match %d taproot signatures "+
			"to %d leaf checksig keys", len(sigs), len(keys))
	}

	return ordered, nil
}

// extractChecksigPubKeys returns the x-only pubkeys that are immediately
// consumed by CHECKSIG/CHECKSIGVERIFY opcodes in the given tapscript.
func extractChecksigPubKeys(script []byte) ([][]byte, error) {
	tokenizer := txscript.MakeScriptTokenizer(0, script)
	keys := make([][]byte, 0, 4)

	var prevData []byte
	for tokenizer.Next() {
		op := tokenizer.Opcode()
		data := tokenizer.Data()

		switch op {
		case txscript.OP_CHECKSIG, txscript.OP_CHECKSIGVERIFY:
			if len(prevData) == schnorr.PubKeyBytesLen {
				keys = append(keys, bytes.Clone(prevData))
			}
		}

		if len(data) > 0 {
			prevData = bytes.Clone(data)
		}
	}

	if err := tokenizer.Err(); err != nil {
		return nil, fmt.Errorf("tokenize leaf script: %w", err)
	}

	return keys, nil
}

// findTaprootLeafScript locates the tapleaf script and control block for the
// given leaf hash from explicit PSBT leaf-script metadata.
func findTaprootLeafScript(in psbt.PInput,
	leafHash []byte) (*psbt.TaprootTapLeafScript, error) {

	if len(leafHash) != chainhash.HashSize {
		return nil, fmt.Errorf("invalid leaf hash size")
	}

	for i := range in.TaprootLeafScript {
		leaf := in.TaprootLeafScript[i]
		if leaf == nil {
			continue
		}

		hash := txscript.NewTapLeaf(
			leaf.LeafVersion, leaf.Script,
		).TapHash()
		if bytes.Equal(hash[:], leafHash) {
			return leaf, nil
		}
	}

	return nil, fmt.Errorf("taproot leaf script not found")
}

// tapLeafFromEncodedTree reconstructs a TaprootTapLeafScript from encoded tap
// tree data by locating the target leaf and deriving its control block.
func tapLeafFromEncodedTree(encoded []byte,
	target chainhash.Hash) (*psbt.TaprootTapLeafScript, error) {

	leafScripts, err := DecodeTapTree(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode tap tree: %w", err)
	}

	tapLeaves := make([]txscript.TapLeaf, 0, len(leafScripts))
	for _, script := range leafScripts {
		tapLeaves = append(
			tapLeaves, txscript.NewBaseTapLeaf(script),
		)
	}

	tree := txscript.AssembleTaprootScriptTree(tapLeaves...)
	index, ok := tree.LeafProofIndex[target]
	if !ok {
		return nil, fmt.Errorf("leaf not found in tap tree")
	}

	proof := tree.LeafMerkleProofs[index]
	control := proof.ToControlBlock(&arkscript.ARKNUMSKey)
	controlBytes, err := control.ToBytes()
	if err != nil {
		return nil, fmt.Errorf("encode control block: %w", err)
	}

	return &psbt.TaprootTapLeafScript{
		ControlBlock: controlBytes,
		Script:       proof.TapLeaf.Script,
		LeafVersion:  proof.TapLeaf.LeafVersion,
	}, nil
}

// BuildTaprootTapLeafScript constructs a TaprootTapLeafScript for the target
// leaf script using the encoded tap tree data.
//
// This is used by v0 OOR to attach the owner leaf path to Ark inputs so submit
// validation and finalize signing can bind to the correct tapscript leaf.
func BuildTaprootTapLeafScript(encoded []byte,
	leafScript []byte) (*psbt.TaprootTapLeafScript, error) {

	if len(leafScript) == 0 {
		return nil, fmt.Errorf("leaf script must be provided")
	}

	target := txscript.NewBaseTapLeaf(leafScript).TapHash()

	return tapLeafFromEncodedTree(encoded, target)
}

// appendTaprootSigHash appends a non-default sighash byte to a schnorr
// signature for witness construction.
func appendTaprootSigHash(sig []byte, sigHash txscript.SigHashType) []byte {
	out := append([]byte(nil), sig...)
	if sigHash == txscript.SigHashDefault {
		return out
	}

	return append(out, byte(sigHash))
}

// parseFinalWitness deserializes a raw final witness stack from PSBT metadata.
func parseFinalWitness(raw []byte) (wire.TxWitness, error) {
	witnessReader := bytes.NewReader(raw)
	witCount, err := wire.ReadVarInt(witnessReader, 0)
	if err != nil {
		return nil, fmt.Errorf("read witness count: %w", err)
	}

	witness := make(wire.TxWitness, witCount)
	for i := uint64(0); i < witCount; i++ {
		item, err := wire.ReadVarBytes(
			witnessReader, 0, txscript.MaxScriptSize, "witness",
		)
		if err != nil {
			return nil, fmt.Errorf("read witness item: %w", err)
		}

		witness[i] = item
	}

	return witness, nil
}
