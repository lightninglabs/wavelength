package oor

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

const maxFinalWitnessItems = 128

// validateFinalizeCheckpointSignatures enforces that finalized checkpoints:
//   - correspond to the exact co-signed checkpoint set;
//   - preserve the operator collaborative-leaf signature from co-sign;
//   - add a valid owner signature for the same collaborative leaf; and
//   - execute successfully in the script VM.
func validateFinalizeCheckpointSignatures(operatorKey *btcec.PublicKey,
	coSigned, finalized []*psbt.Packet) error {

	if operatorKey == nil {
		return fmt.Errorf("operator key must be provided")
	}

	if len(coSigned) == 0 {
		return fmt.Errorf("co-signed checkpoint psbts must be provided")
	}

	if len(finalized) == 0 {
		return fmt.Errorf("final checkpoint psbts must be provided")
	}

	coSignedByTxid := make(map[chainhash.Hash]*psbt.Packet, len(coSigned))
	for i := range coSigned {
		checkpoint := coSigned[i]
		if checkpoint == nil || checkpoint.UnsignedTx == nil {
			return fmt.Errorf("co-signed checkpoint psbt must " +
				"include unsigned tx")
		}

		txid := checkpoint.UnsignedTx.TxHash()
		if _, exists := coSignedByTxid[txid]; exists {
			return fmt.Errorf("duplicate co-signed checkpoint "+
				"txid: %s", txid)
		}

		coSignedByTxid[txid] = checkpoint
	}

	seen := make(map[chainhash.Hash]struct{}, len(finalized))
	for i := range finalized {
		checkpoint := finalized[i]
		if checkpoint == nil || checkpoint.UnsignedTx == nil {
			return fmt.Errorf("final checkpoint psbt must " +
				"include unsigned tx")
		}

		txid := checkpoint.UnsignedTx.TxHash()
		coSignedCheckpoint := coSignedByTxid[txid]
		if coSignedCheckpoint == nil {
			return fmt.Errorf("final checkpoint %s missing from "+
				"co-signed set", txid)
		}

		if _, exists := seen[txid]; exists {
			return fmt.Errorf("duplicate final checkpoint txid: %s",
				txid)
		}
		seen[txid] = struct{}{}

		err := validateFinalizedCheckpoint(
			operatorKey, coSignedCheckpoint, checkpoint,
		)
		if err != nil {
			return fmt.Errorf("final checkpoint %s: %w", txid, err)
		}
	}

	if len(seen) != len(coSignedByTxid) {
		return fmt.Errorf("final checkpoint set does not match " +
			"co-signed set")
	}

	return nil
}

// validateFinalizedCheckpoint verifies a single co-signed checkpoint against
// its finalized counterpart, ensuring signatures are preserved/added and the
// collaborative leaf executes successfully.
func validateFinalizedCheckpoint(operatorKey *btcec.PublicKey,
	coSigned, finalized *psbt.Packet) error {

	if len(coSigned.UnsignedTx.TxIn) != 1 || len(coSigned.Inputs) != 1 {
		return fmt.Errorf("co-signed checkpoint must have " +
			"exactly one input")
	}

	if len(finalized.UnsignedTx.TxIn) != 1 || len(finalized.Inputs) != 1 {
		return fmt.Errorf("finalized checkpoint must have " +
			"exactly one input")
	}

	coInput := &coSigned.Inputs[0]
	finalInput := &finalized.Inputs[0]

	if coInput.WitnessUtxo == nil {
		return fmt.Errorf("co-signed checkpoint must include witness " +
			"utxo")
	}
	if finalInput.WitnessUtxo == nil {
		return fmt.Errorf("finalized checkpoint must include witness " +
			"utxo")
	}

	if coInput.WitnessUtxo.Value != finalInput.WitnessUtxo.Value {
		return fmt.Errorf("witness utxo value mismatch")
	}
	if !bytes.Equal(coInput.WitnessUtxo.PkScript,
		finalInput.WitnessUtxo.PkScript) {

		return fmt.Errorf("witness utxo script mismatch")
	}

	operatorSig, err := findSignatureByPubKey(
		coInput, operatorKey,
	)
	if err != nil {
		return err
	}

	tapLeaf, err := findTapLeafByHash(coInput, operatorSig.LeafHash)
	if err != nil {
		return err
	}

	err = validateTapLeafControlBlockBinding(tapLeaf,
		finalInput.WitnessUtxo)
	if err != nil {
		return err
	}

	prevFetcher := txscript.NewCannedPrevOutputFetcher(
		finalInput.WitnessUtxo.PkScript, finalInput.WitnessUtxo.Value,
	)

	// Finalized custom spends are serialized with the complete
	// FinalScriptWitness, while taproot partial signature fields may be
	// stripped during PSBT round-trip serialization. Validate the
	// embedded operator signature against the co-signed checkpoint and
	// execute the final witness directly.
	if len(finalInput.FinalScriptWitness) > 0 {
		err = validateCustomFinalWitness(
			finalized.UnsignedTx, finalInput.WitnessUtxo,
			finalInput.FinalScriptWitness, tapLeaf,
			operatorSig, prevFetcher,
		)
		if err != nil {
			return err
		}

		return nil
	}

	finalOperatorSig, err := findSignatureByPubKeyAndLeafHash(
		finalInput, operatorKey, operatorSig.LeafHash,
	)
	if err != nil {
		return err
	}

	if finalOperatorSig.SigHash != operatorSig.SigHash {
		return fmt.Errorf("operator sighash type changed")
	}
	if !bytes.Equal(finalOperatorSig.Signature, operatorSig.Signature) {
		return fmt.Errorf("operator signature changed")
	}

	// Standard collaborative leaf: verify both operator and owner
	// signatures, then execute the script VM.
	ownerSig, err := findSingleNonOperatorSignatureForLeaf(
		finalInput, operatorKey, operatorSig.LeafHash,
	)
	if err != nil {
		return err
	}

	err = verifyTaprootScriptSpendSig(
		finalized.UnsignedTx, 0, prevFetcher, tapLeaf,
		finalOperatorSig,
	)
	if err != nil {
		return fmt.Errorf("operator signature invalid: %w", err)
	}

	err = verifyTaprootScriptSpendSig(
		finalized.UnsignedTx, 0, prevFetcher, tapLeaf, ownerSig,
	)
	if err != nil {
		return fmt.Errorf("owner signature invalid: %w", err)
	}

	return executeCollaborativeLeaf(
		finalized.UnsignedTx, finalInput.WitnessUtxo, tapLeaf,
		finalOperatorSig, ownerSig,
	)
}

// validateCustomFinalWitness validates a finalized custom witness spend using
// the operator signature and tapleaf from the co-signed checkpoint.
func validateCustomFinalWitness(unsignedTx *wire.MsgTx, prevOut *wire.TxOut,
	finalScriptWitness []byte, tapLeaf *psbt.TaprootTapLeafScript,
	operatorSig *psbt.TaprootScriptSpendSig,
	prevFetcher txscript.PrevOutputFetcher) error {

	witness, err := parseFinalScriptWitness(finalScriptWitness)
	if err != nil {
		return fmt.Errorf("parse final script witness: %w", err)
	}

	if len(witness) < 4 {
		return fmt.Errorf("custom witness must have at least 4 "+
			"elements (got %d)", len(witness))
	}

	wantOperatorSig := appendTaprootSigHash(
		operatorSig.Signature, operatorSig.SigHash,
	)
	if !bytes.Equal(witness[0], wantOperatorSig) {
		return fmt.Errorf("operator signature changed")
	}

	if !bytes.Equal(witness[len(witness)-2], tapLeaf.Script) {
		return fmt.Errorf("custom witness script changed")
	}
	if !bytes.Equal(witness[len(witness)-1], tapLeaf.ControlBlock) {
		return fmt.Errorf("custom witness control block changed")
	}

	err = verifyTaprootScriptSpendSig(
		unsignedTx, 0, prevFetcher, tapLeaf, operatorSig,
	)
	if err != nil {
		return fmt.Errorf("operator signature invalid: %w", err)
	}

	return executeCustomWitness(unsignedTx, prevOut, finalScriptWitness)
}

// findSignatureByPubKey locates a taproot script spend signature for the
// given pubkey within a PSBT input.
func findSignatureByPubKey(in *psbt.PInput,
	pubKey *btcec.PublicKey) (*psbt.TaprootScriptSpendSig, error) {

	if in == nil {
		return nil, fmt.Errorf("psbt input must be provided")
	}
	if pubKey == nil {
		return nil, fmt.Errorf("pubkey must be provided")
	}

	want := schnorr.SerializePubKey(pubKey)

	var match *psbt.TaprootScriptSpendSig
	for i := range in.TaprootScriptSpendSig {
		sigRec := in.TaprootScriptSpendSig[i]
		if sigRec == nil {
			continue
		}

		if !bytes.Equal(sigRec.XOnlyPubKey, want) {
			continue
		}

		if match != nil {
			return nil, fmt.Errorf("multiple signatures found " +
				"for same pubkey")
		}
		match = sigRec
	}

	if match == nil {
		return nil, fmt.Errorf("missing signature for operator pubkey")
	}

	return match, nil
}

// findSignatureByPubKeyAndLeafHash locates a taproot script spend signature for
// the given pubkey and leaf hash within a PSBT input.
func findSignatureByPubKeyAndLeafHash(in *psbt.PInput,
	pubKey *btcec.PublicKey,
	leafHash []byte) (*psbt.TaprootScriptSpendSig, error) {

	wantPub := schnorr.SerializePubKey(pubKey)

	for i := range in.TaprootScriptSpendSig {
		sigRec := in.TaprootScriptSpendSig[i]
		if sigRec == nil {
			continue
		}

		if !bytes.Equal(sigRec.XOnlyPubKey, wantPub) {
			continue
		}
		if !bytes.Equal(sigRec.LeafHash, leafHash) {
			continue
		}

		return sigRec, nil
	}

	return nil, fmt.Errorf("missing operator signature for " +
		"collaborative leaf")
}

// findSingleNonOperatorSignatureForLeaf returns the sole non-operator
// signature for the specified leaf hash, rejecting zero or multiple matches.
func findSingleNonOperatorSignatureForLeaf(in *psbt.PInput,
	operatorKey *btcec.PublicKey,
	leafHash []byte) (*psbt.TaprootScriptSpendSig, error) {

	if in == nil {
		return nil, fmt.Errorf("psbt input must be provided")
	}

	opPub := schnorr.SerializePubKey(operatorKey)

	var ownerSig *psbt.TaprootScriptSpendSig
	for i := range in.TaprootScriptSpendSig {
		sigRec := in.TaprootScriptSpendSig[i]
		if sigRec == nil {
			continue
		}

		if !bytes.Equal(sigRec.LeafHash, leafHash) {
			continue
		}
		if bytes.Equal(sigRec.XOnlyPubKey, opPub) {
			continue
		}

		if ownerSig != nil {
			return nil, fmt.Errorf("multiple owner signatures " +
				"found for collaborative leaf")
		}
		ownerSig = sigRec
	}

	if ownerSig == nil {
		return nil, fmt.Errorf("missing owner signature for " +
			"collaborative leaf")
	}

	return ownerSig, nil
}

// findTapLeafByHash locates the tapleaf script/control block for the specified
// leaf hash within a PSBT input.
func findTapLeafByHash(in *psbt.PInput,
	leafHash []byte) (*psbt.TaprootTapLeafScript, error) {

	if in == nil {
		return nil, fmt.Errorf("psbt input must be provided")
	}

	for i := range in.TaprootLeafScript {
		leaf := in.TaprootLeafScript[i]
		if leaf == nil {
			continue
		}

		hash := txscript.NewTapLeaf(
			leaf.LeafVersion, leaf.Script,
		).TapHash()
		if !bytes.Equal(hash[:], leafHash) {
			continue
		}

		return leaf, nil
	}

	return nil, fmt.Errorf("tap leaf missing for collaborative leaf hash")
}

// validateTapLeafControlBlockBinding verifies the tapleaf control block binds
// to the provided prevout pkScript (taproot output key).
func validateTapLeafControlBlockBinding(
	leaf *psbt.TaprootTapLeafScript, prevOut *wire.TxOut) error {

	if leaf == nil {
		return fmt.Errorf("tap leaf must be provided")
	}
	if prevOut == nil {
		return fmt.Errorf("prevout must be provided")
	}

	controlBlock, err := txscript.ParseControlBlock(leaf.ControlBlock)
	if err != nil {
		return fmt.Errorf("parse control block: %w", err)
	}

	rootHash := controlBlock.RootHash(leaf.Script)
	tapKey := txscript.ComputeTaprootOutputKey(
		controlBlock.InternalKey, rootHash,
	)
	pkScript, err := txscript.PayToTaprootScript(tapKey)
	if err != nil {
		return fmt.Errorf("derive taproot pkscript: %w", err)
	}

	if !bytes.Equal(pkScript, prevOut.PkScript) {
		return fmt.Errorf("control block/script do not match prevout " +
			"taproot key")
	}

	return nil
}

// parseTaprootScriptSpendSigBytes parses a schnorr signature that may include
// an appended sighash byte, enforcing the expected sighash type.
func parseTaprootScriptSpendSigBytes(raw []byte,
	sigHash txscript.SigHashType) (*schnorr.Signature, error) {

	switch {
	case len(raw) == schnorr.SignatureSize:
		return schnorr.ParseSignature(raw)

	case len(raw) == schnorr.SignatureSize+1 &&
		raw[len(raw)-1] == byte(sigHash):

		return schnorr.ParseSignature(raw[:schnorr.SignatureSize])

	default:
		return nil, fmt.Errorf("invalid schnorr signature size: %d",
			len(raw))
	}
}

// verifyTaprootScriptSpendSig verifies a single taproot script spend signature
// against the provided transaction, prevout data, and leaf script.
func verifyTaprootScriptSpendSig(tx *wire.MsgTx, inputIndex int,
	prevFetcher txscript.PrevOutputFetcher,
	leaf *psbt.TaprootTapLeafScript,
	sigRec *psbt.TaprootScriptSpendSig) error {

	if tx == nil {
		return fmt.Errorf("tx must be provided")
	}
	if leaf == nil {
		return fmt.Errorf("leaf must be provided")
	}
	if sigRec == nil {
		return fmt.Errorf("signature record must be provided")
	}

	sig, err := parseTaprootScriptSpendSigBytes(
		sigRec.Signature, sigRec.SigHash,
	)
	if err != nil {
		return err
	}

	pubKey, err := schnorr.ParsePubKey(sigRec.XOnlyPubKey)
	if err != nil {
		return fmt.Errorf("parse pubkey: %w", err)
	}

	tapLeaf := txscript.NewTapLeaf(leaf.LeafVersion, leaf.Script)
	sigHashes := txscript.NewTxSigHashes(tx, prevFetcher)
	sigHash, err := txscript.CalcTapscriptSignaturehash(
		sigHashes, sigRec.SigHash, tx, inputIndex, prevFetcher, tapLeaf,
	)
	if err != nil {
		return fmt.Errorf("calculate tapscript sighash: %w", err)
	}

	if !sig.Verify(sigHash, pubKey) {
		return fmt.Errorf("invalid taproot script signature")
	}

	return nil
}

// appendTaprootSigHash appends a non-default sighash byte to a schnorr
// signature for witness construction.
func appendTaprootSigHash(sig []byte, sigHash txscript.SigHashType) []byte {
	if sigHash == txscript.SigHashDefault {
		return append([]byte(nil), sig...)
	}

	out := make([]byte, 0, len(sig)+1)
	out = append(out, sig...)
	out = append(out, byte(sigHash))

	return out
}

// executeCollaborativeLeaf runs the script VM for the collaborative leaf using
// the operator and owner signatures to ensure the finalized checkpoint is
// spendable on-chain.
func executeCollaborativeLeaf(unsignedTx *wire.MsgTx, prevOut *wire.TxOut,
	leaf *psbt.TaprootTapLeafScript,
	operatorSig, ownerSig *psbt.TaprootScriptSpendSig) error {

	if unsignedTx == nil {
		return fmt.Errorf("unsigned tx must be provided")
	}

	tx := unsignedTx.Copy()
	tx.TxIn[0].Witness = wire.TxWitness{
		appendTaprootSigHash(
			operatorSig.Signature, operatorSig.SigHash,
		),
		appendTaprootSigHash(ownerSig.Signature, ownerSig.SigHash),
		leaf.Script,
		leaf.ControlBlock,
	}

	prevFetcher := txscript.NewCannedPrevOutputFetcher(
		prevOut.PkScript, prevOut.Value,
	)
	sigHashes := txscript.NewTxSigHashes(tx, prevFetcher)

	engine, err := txscript.NewEngine(
		prevOut.PkScript, tx, 0, txscript.StandardVerifyFlags, nil,
		sigHashes, prevOut.Value, prevFetcher,
	)
	if err != nil {
		return err
	}

	if err := engine.Execute(); err != nil {
		return fmt.Errorf("script vm execution failed: %w", err)
	}

	return nil
}

// executeCustomWitness validates a finalized checkpoint that uses a custom
// witness (e.g., vHTLC Claim with preimage). It parses the serialized
// witness, assigns it to the transaction input, and runs the script VM.
func executeCustomWitness(unsignedTx *wire.MsgTx, prevOut *wire.TxOut,
	finalScriptWitness []byte) error {

	if unsignedTx == nil {
		return fmt.Errorf("unsigned tx must be provided")
	}

	if prevOut == nil {
		return fmt.Errorf("prevout must be provided")
	}

	witness, err := parseFinalScriptWitness(finalScriptWitness)
	if err != nil {
		return fmt.Errorf("parse final script witness: %w", err)
	}

	if len(witness) < 3 {
		return fmt.Errorf("custom witness must have at least 3 "+
			"elements (got %d)", len(witness))
	}

	tx := unsignedTx.Copy()
	tx.TxIn[0].Witness = witness

	prevFetcher := txscript.NewCannedPrevOutputFetcher(
		prevOut.PkScript, prevOut.Value,
	)
	sigHashes := txscript.NewTxSigHashes(tx, prevFetcher)

	engine, err := txscript.NewEngine(
		prevOut.PkScript, tx, 0, txscript.StandardVerifyFlags, nil,
		sigHashes, prevOut.Value, prevFetcher,
	)
	if err != nil {
		return fmt.Errorf("create script engine: %w", err)
	}

	if err := engine.Execute(); err != nil {
		return fmt.Errorf("custom witness script vm execution "+
			"failed: %w", err)
	}

	return nil
}

// parseFinalScriptWitness deserializes a BIP-174 FinalScriptWitness field
// into a wire.TxWitness.
func parseFinalScriptWitness(raw []byte) (wire.TxWitness, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty final script witness")
	}

	// FinalScriptWitness is encoded as a count-prefixed vector of
	// length-prefixed byte strings, using Bitcoin's standard witness
	// serialization.
	r := bytes.NewReader(raw)

	// Read the number of witness items.
	count, err := wire.ReadVarInt(r, 0)
	if err != nil {
		return nil, fmt.Errorf("read witness count: %w", err)
	}
	if count > maxFinalWitnessItems {
		return nil, fmt.Errorf("witness item count %d exceeds max %d",
			count, maxFinalWitnessItems)
	}

	witness := make(wire.TxWitness, count)
	for i := uint64(0); i < count; i++ {
		item, err := wire.ReadVarBytes(r, 0, 10000, "witness item")
		if err != nil {
			return nil, fmt.Errorf("read witness item %d: %w",
				i, err)
		}

		witness[i] = item
	}

	if r.Len() != 0 {
		return nil, fmt.Errorf(
			"final script witness has %d trailing bytes", r.Len(),
		)
	}

	return witness, nil
}
