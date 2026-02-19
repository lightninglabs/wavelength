package oor

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/scripts"
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
		if err := validatePSBTSpends(checkpoint, fmt.Sprintf(
			"final checkpoint %d", i,
		)); err != nil {
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

		tapTreeEncoded, err := GetTapTreePSBTInput(in)
		if err != nil {
			return fmt.Errorf("%s input %d: get tap tree: %w",
				label, i, err)
		}

		witness, err := buildTaprootWitness(in, tapTreeEncoded)
		if err != nil {
			return fmt.Errorf("%s input %d: %w", label, i,
				err)
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
			return fmt.Errorf("%s input %d missing prevout",
				label, i)
		}

		engine, err := txscript.NewEngine(
			prevOut.PkScript, tx, i,
			txscript.StandardVerifyFlags, nil, sigHashes,
			prevOut.Value, prevFetcher,
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

// buildTaprootWitness constructs the witness stack for a taproot input using
// any finalized witness, key spend signature, or script spend signature data
// available in the PSBT.
func buildTaprootWitness(in psbt.PInput,
	tapTreeEncoded []byte) (wire.TxWitness, error) {

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
				return nil, fmt.Errorf(
					"nil taproot signature",
				)
			}

			if !bytes.Equal(sig.LeafHash, targetLeafHash) {
				return nil, fmt.Errorf("taproot script " +
					"signatures reference multiple " +
					"leaf hashes")
			}
		}

		leafScript, err := findTaprootLeafScript(
			in, targetLeafHash, tapTreeEncoded,
		)
		if err != nil {
			return nil, err
		}

		witness := make(wire.TxWitness, 0,
			len(in.TaprootScriptSpendSig)+2,
		)
		for i := range in.TaprootScriptSpendSig {
			sig := in.TaprootScriptSpendSig[i]
			witness = append(
				witness,
				appendTaprootSigHash(
					sig.Signature, sig.SigHash,
				),
			)
		}

		witness = append(witness, leafScript.Script)
		witness = append(witness, leafScript.ControlBlock)

		return witness, nil
	}

	if len(in.TaprootLeafScript) == 1 {
		leaf := in.TaprootLeafScript[0]
		if leaf == nil {
			return nil, fmt.Errorf("taproot leaf script " +
				"missing")
		}

		return wire.TxWitness{
			leaf.Script,
			leaf.ControlBlock,
		}, nil
	}

	return nil, fmt.Errorf("missing taproot signature or " +
		"leaf script")
}

// findTaprootLeafScript locates the tapleaf script and control block for the
// given leaf hash, searching PSBT metadata first and falling back to encoded
// tap tree data if needed.
func findTaprootLeafScript(in psbt.PInput, leafHash []byte,
	tapTreeEncoded []byte) (*psbt.TaprootTapLeafScript, error) {

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

	if len(tapTreeEncoded) == 0 {
		return nil, fmt.Errorf("taproot leaf script not found")
	}

	target := chainhash.Hash{}
	copy(target[:], leafHash)

	leafScript, err := tapLeafFromEncodedTree(
		tapTreeEncoded, target,
	)
	if err != nil {
		return nil, err
	}

	return leafScript, nil
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
	control := proof.ToControlBlock(&scripts.ARKNUMSKey)
	controlBytes, err := control.ToBytes()
	if err != nil {
		return nil, fmt.Errorf("encode control block: %w",
			err)
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
func appendTaprootSigHash(sig []byte,
	sigHash txscript.SigHashType) []byte {

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
			witnessReader, 0, txscript.MaxScriptSize,
			"witness",
		)
		if err != nil {
			return nil, fmt.Errorf("read witness item: %w",
				err)
		}

		witness[i] = item
	}

	return witness, nil
}
