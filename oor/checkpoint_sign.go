package oor

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
	"github.com/lightningnetwork/lnd/input"
)

// ErrCustomCheckpointInputSigning means the prepared checkpoint input passed
// validation, but the local signer failed to produce a signature.
var ErrCustomCheckpointInputSigning = errors.New("custom checkpoint input " +
	"signing failed")

// SignCheckpointPSBTs attaches the client-side collaborative VTXO spend
// signatures to each checkpoint PSBT.
//
// Each checkpoint PSBT is expected to spend exactly one VTXO (input index 0).
// The TransferInput slice is expected to match the checkpoint PSBT slice 1:1.
// Before client signing, the operator signature is verified to preserve
// custody: clients only sign once the operator has committed its half of the
// collaborative spend path.
func SignCheckpointPSBTs(signer input.Signer, inputs []TransferInput,
	checkpoints []*psbt.Packet) error {

	switch {
	case signer == nil:
		return fmt.Errorf("signer must be provided")

	case len(inputs) == 0:
		return fmt.Errorf("transfer inputs must be provided")

	case len(checkpoints) == 0:
		return fmt.Errorf("checkpoint psbts must be provided")

	case len(inputs) != len(checkpoints):
		return fmt.Errorf("input count %d does not match checkpoint "+
			"count %d", len(inputs), len(checkpoints))
	}

	// Before the client adds its own signature material, verify the
	// server/operator signature that was attached at submit acceptance.
	// This preserves custody: we only proceed once the
	// collaborative spend is already cryptographically valid from
	// the operator side.
	err := validateOperatorCheckpointSignatures(inputs, checkpoints)
	if err != nil {
		return err
	}

	for i := range inputs {
		err = signCheckpointPSBT(signer, &inputs[i], checkpoints[i])
		if err != nil {
			return fmt.Errorf("sign checkpoint %d: %w", i, err)
		}
	}

	return nil
}

// validateOperatorCheckpointSignatures verifies each checkpoint includes a
// valid operator collaborative-path signature before client signing begins.
//
// This ensures the checkpoint witness UTXO matches the expected VTXO data and
// that the operator signature correctly spends the collaborative leaf.
func validateOperatorCheckpointSignatures(inputs []TransferInput,
	checkpoints []*psbt.Packet) error {

	inputByOutpoint := make(map[wire.OutPoint]*TransferInput, len(inputs))
	for i := range inputs {
		in := &inputs[i]
		if in == nil || in.VTXO == nil {
			return fmt.Errorf("transfer input must include vtxo")
		}

		err := in.Validate()
		if err != nil {
			return err
		}

		inputByOutpoint[in.VTXO.Outpoint] = in
	}

	for i := range checkpoints {
		checkpoint := checkpoints[i]
		if checkpoint == nil || checkpoint.UnsignedTx == nil {
			return fmt.Errorf("checkpoint psbt must include " +
				"unsigned tx")
		}

		if len(checkpoint.UnsignedTx.TxIn) != 1 ||
			len(checkpoint.Inputs) != 1 {
			return fmt.Errorf("checkpoint must have exactly one " +
				"input")
		}

		prevOutpoint := checkpoint.UnsignedTx.TxIn[0].PreviousOutPoint
		in := inputByOutpoint[prevOutpoint]
		if in == nil {
			return fmt.Errorf("unknown checkpoint input "+
				"outpoint %s", prevOutpoint)
		}

		err := validateSingleOperatorCheckpointSignature(
			in, checkpoint,
		)
		if err != nil {
			return fmt.Errorf("checkpoint %d: %w", i, err)
		}
	}

	return nil
}

// validateSingleOperatorCheckpointSignature verifies one checkpoint contains a
// valid operator collaborative-path script-spend signature.
func validateSingleOperatorCheckpointSignature(in *TransferInput,
	checkpoint *psbt.Packet) error {

	vtxo := in.VTXO
	if vtxo == nil || vtxo.OperatorKey == nil {
		return fmt.Errorf("operator key must be provided")
	}

	prevOut := checkpoint.Inputs[0].WitnessUtxo
	if prevOut == nil {
		return fmt.Errorf("checkpoint must include witness utxo")
	}

	if prevOut.Value != int64(vtxo.Amount) {
		return fmt.Errorf("checkpoint witness utxo value mismatch")
	}

	if !bytes.Equal(prevOut.PkScript, vtxo.PkScript) {
		return fmt.Errorf("checkpoint witness utxo script mismatch")
	}

	prevFetcher := txscript.NewCannedPrevOutputFetcher(
		prevOut.PkScript, prevOut.Value,
	)

	spendPath, err := in.EffectiveSpendPath()
	if err != nil {
		return err
	}

	err = requireTapLeafScript(
		&checkpoint.Inputs[0], spendPath.WitnessScript,
		spendPath.ControlBlock,
	)
	if err != nil {
		return err
	}

	sigRec, err := findTaprootScriptSpendSig(
		&checkpoint.Inputs[0], vtxo.OperatorKey,
		spendPath.WitnessScript,
	)
	if err != nil {
		return err
	}

	return verifyTaprootScriptSpendSig(
		checkpoint.UnsignedTx, 0, prevFetcher, spendPath.WitnessScript,
		sigRec,
	)
}

// requireTapLeafScript asserts the PSBT input includes the expected tapleaf
// script and control block for the collaborative spend path.
func requireTapLeafScript(in *psbt.PInput, leafScript,
	controlBlock []byte) error {

	if in == nil {
		return fmt.Errorf("psbt input must be provided")
	}

	for i := range in.TaprootLeafScript {
		leaf := in.TaprootLeafScript[i]
		if leaf == nil {
			continue
		}

		if bytes.Equal(leaf.Script, leafScript) &&
			bytes.Equal(leaf.ControlBlock, controlBlock) {
			return nil
		}
	}

	return fmt.Errorf("checkpoint missing collaborative tap leaf")
}

// findTaprootScriptSpendSig locates a taproot script spend signature for the
// given pubkey and leaf script within a PSBT input.
func findTaprootScriptSpendSig(in *psbt.PInput, pubKey *btcec.PublicKey,
	leafScript []byte) (*psbt.TaprootScriptSpendSig, error) {

	if in == nil {
		return nil, fmt.Errorf("psbt input must be provided")
	}

	if pubKey == nil {
		return nil, fmt.Errorf("pubkey must be provided")
	}

	leafHash := txscript.NewBaseTapLeaf(leafScript).TapHash()
	leafHashBytes := leafHash[:]
	wantPub := schnorr.SerializePubKey(pubKey)

	for i := range in.TaprootScriptSpendSig {
		sigRec := in.TaprootScriptSpendSig[i]
		if sigRec == nil {
			continue
		}

		if !bytes.Equal(sigRec.XOnlyPubKey, wantPub) {
			continue
		}

		if !bytes.Equal(sigRec.LeafHash, leafHashBytes) {
			continue
		}

		return sigRec, nil
	}

	return nil, fmt.Errorf("missing taproot script spend signature")
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

// verifyTaprootScriptSpendSig verifies a taproot script spend signature against
// the provided transaction, prevout data, and leaf script.
func verifyTaprootScriptSpendSig(tx *wire.MsgTx, inputIndex int,
	prevFetcher txscript.PrevOutputFetcher, leafScript []byte,
	sigRec *psbt.TaprootScriptSpendSig) error {

	if tx == nil {
		return fmt.Errorf("tx must be provided")
	}

	if sigRec == nil {
		return fmt.Errorf("signature must be provided")
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

	sigHashes := txscript.NewTxSigHashes(tx, prevFetcher)
	sigHash, err := txscript.CalcTapscriptSignaturehash(
		sigHashes, sigRec.SigHash, tx, inputIndex, prevFetcher,
		txscript.NewBaseTapLeaf(leafScript),
	)
	if err != nil {
		return fmt.Errorf("calculate tapscript sighash: %w", err)
	}

	if !sig.Verify(sigHash, pubKey) {
		return fmt.Errorf("invalid taproot script signature")
	}

	return nil
}

// signCheckpointPSBT signs checkpoint input 0 with the client key for the
// collaborative VTXO leaf path.
func signCheckpointPSBT(signer input.Signer, in *TransferInput,
	checkpoint *psbt.Packet) error {

	switch {
	case signer == nil:
		return fmt.Errorf("signer must be provided")

	case in == nil:
		return fmt.Errorf("transfer input must be provided")

	case checkpoint == nil || checkpoint.UnsignedTx == nil:
		return fmt.Errorf("checkpoint psbt must include unsigned tx")

	case len(checkpoint.Inputs) == 0:
		return fmt.Errorf("checkpoint psbt must have inputs")
	}

	err := in.Validate()
	if err != nil {
		return err
	}

	// For non-standard spend paths (e.g., vHTLC Claim), delegate to
	// the custom signing flow that uses SpendInfo directly.
	if in.IsCustomSpend() {
		return signCustomCheckpointPSBT(signer, in, checkpoint)
	}

	if len(checkpoint.UnsignedTx.TxIn) != 1 ||
		len(checkpoint.Inputs) != 1 {
		return fmt.Errorf("checkpoint psbt must have exactly one "+
			"input, got tx=%d psbt=%d",
			len(checkpoint.UnsignedTx.TxIn), len(checkpoint.Inputs))
	}

	prevOut := &wire.TxOut{
		Value:    int64(in.VTXO.Amount),
		PkScript: in.VTXO.PkScript,
	}

	prevFetcher := txscript.NewCannedPrevOutputFetcher(
		prevOut.PkScript, prevOut.Value,
	)

	spendPath, err := in.EffectiveSpendPath()
	if err != nil {
		return err
	}

	sigHashes := txscript.NewTxSigHashes(
		checkpoint.UnsignedTx, prevFetcher,
	)
	signDesc := spendPath.SpendInfo.BuildSignDescriptor(
		in.VTXO.ClientKey, prevOut, sigHashes, prevFetcher, 0,
	)

	sig, err := signer.SignOutputRaw(checkpoint.UnsignedTx, signDesc)
	if err != nil {
		return fmt.Errorf("sign output: %w", err)
	}

	sigBytes := sig.Serialize()
	if len(sigBytes) == 0 {
		return fmt.Errorf("signer returned empty signature")
	}

	err = psbtutil.AddTapLeafScript(
		&checkpoint.Inputs[0], spendPath.SpendInfo,
	)
	if err != nil {
		return err
	}

	err = psbtutil.AddTaprootScriptSpendSig(
		&checkpoint.Inputs[0], in.VTXO.ClientKey.PubKey,
		spendPath.WitnessScript, sigBytes, signDesc.HashType,
	)
	if err != nil {
		return err
	}

	sigRec, err := findTaprootScriptSpendSig(
		&checkpoint.Inputs[0], in.VTXO.ClientKey.PubKey,
		spendPath.WitnessScript,
	)
	if err != nil {
		return err
	}

	err = verifyTaprootScriptSpendSig(
		checkpoint.UnsignedTx, signDesc.InputIndex, prevFetcher,
		spendPath.WitnessScript, sigRec,
	)
	if err != nil {
		return err
	}

	return nil
}

// signCustomCheckpointPSBT signs a checkpoint input using a custom spend
// path provided by the TransferInput's SpendInfo. This is used for
// non-standard VTXOs such as vHTLC Claim paths where the spend leaf is
// not the default collaborative VTXO leaf.
func signCustomCheckpointPSBT(signer input.Signer, in *TransferInput,
	checkpoint *psbt.Packet) error {

	if len(checkpoint.UnsignedTx.TxIn) != 1 ||
		len(checkpoint.Inputs) != 1 {
		return fmt.Errorf("checkpoint psbt must have exactly one "+
			"input, got tx=%d psbt=%d",
			len(checkpoint.UnsignedTx.TxIn), len(checkpoint.Inputs))
	}

	localSig, err := SignCustomCheckpointInput(signer, in, checkpoint)
	if err != nil {
		return err
	}

	// Attach the custom leaf script and client signature to the PSBT.
	spendData := &arkscript.SpendInfo{
		WitnessScript: in.CustomSpend.SpendInfo.WitnessScript,
		ControlBlock:  in.CustomSpend.SpendInfo.ControlBlock,
	}

	err = psbtutil.AddTapLeafScript(&checkpoint.Inputs[0], spendData)
	if err != nil {
		return err
	}

	err = psbtutil.AddTaprootScriptSpendSig(
		&checkpoint.Inputs[0], in.VTXO.ClientKey.PubKey,
		in.CustomSpend.SpendInfo.WitnessScript, localSig.Signature,
		localSig.SigHash,
	)
	if err != nil {
		return err
	}

	err = attachExternalTaprootScriptSignatures(
		in, &checkpoint.Inputs[0],
	)
	if err != nil {
		return err
	}

	// If condition witness items are provided (e.g., hashlock
	// preimage), or if all custom-spend keys are known, assemble and set
	// the final script witness. This combines all required signatures,
	// condition items, witness script, and control block into BIP-174
	// FinalScriptWitness format.
	if len(in.CustomSpend.Conditions) > 0 {
		err = arkscript.PutConditionWitnessPSBTInput(
			checkpoint, 0, in.CustomSpend.Conditions,
		)
		if err != nil {
			return err
		}
	}

	if len(in.CustomSpendKeys) > 0 || len(in.CustomSpend.Conditions) > 0 {
		err = assembleCustomFinalWitness(
			in, &checkpoint.Inputs[0],
		)
		if err != nil {
			return err
		}
	}

	return nil
}

// SignCustomCheckpointInput signs a checkpoint input using the transfer
// input's custom spend path and returns the raw tapscript signature.
func SignCustomCheckpointInput(signer input.Signer, in *TransferInput,
	checkpoint *psbt.Packet) (*ExternalTaprootScriptSignature, error) {

	if signer == nil {
		return nil, fmt.Errorf("signer must be provided")
	}

	if in == nil || in.VTXO == nil || in.CustomSpend == nil {
		return nil, fmt.Errorf("custom transfer input is required")
	}

	if checkpoint == nil || checkpoint.UnsignedTx == nil {
		return nil, fmt.Errorf("checkpoint psbt must include " +
			"unsigned tx")
	}

	if len(checkpoint.UnsignedTx.TxIn) != 1 ||
		len(checkpoint.Inputs) != 1 {
		return nil, fmt.Errorf("checkpoint psbt must have exactly one "+
			"input, got tx=%d psbt=%d",
			len(checkpoint.UnsignedTx.TxIn), len(checkpoint.Inputs))
	}

	if err := in.Validate(); err != nil {
		return nil, err
	}

	// Defense-in-depth binding check: the caller-supplied witness script
	// and control block must commit to a taproot output whose P2TR script
	// is exactly the VTXO's pkScript. Without this, a malformed control
	// block would coerce the signer into producing a Schnorr signature
	// over an attacker-chosen tapscript.
	if err := in.CustomSpend.VerifyBindsToPkScript(
		in.VTXO.PkScript,
	); err != nil {
		return nil, fmt.Errorf("custom spend path does not bind to "+
			"VTXO pkScript: %w", err)
	}

	prevOut := &wire.TxOut{
		Value:    int64(in.VTXO.Amount),
		PkScript: in.VTXO.PkScript,
	}

	prevFetcher := txscript.NewCannedPrevOutputFetcher(
		prevOut.PkScript, prevOut.Value,
	)
	sigHashes := txscript.NewTxSigHashes(
		checkpoint.UnsignedTx, prevFetcher,
	)

	signDesc := in.CustomSpend.SpendInfo.BuildSignDescriptor(
		in.VTXO.ClientKey, prevOut, sigHashes, prevFetcher, 0,
	)

	sig, err := signer.SignOutputRaw(checkpoint.UnsignedTx, signDesc)
	if err != nil {
		return nil, fmt.Errorf("%w: sign output: %w",
			ErrCustomCheckpointInputSigning, err)
	}

	sigBytes := sig.Serialize()
	if len(sigBytes) == 0 {
		return nil, fmt.Errorf("%w: signer returned empty signature",
			ErrCustomCheckpointInputSigning)
	}

	return &ExternalTaprootScriptSignature{
		PubKey:        in.VTXO.ClientKey.PubKey,
		WitnessScript: in.CustomSpend.SpendInfo.WitnessScript,
		Signature:     sigBytes,
		SigHash:       signDesc.HashType,
	}, nil
}

// attachExternalTaprootScriptSignatures copies externally produced custom
// spend signatures into the checkpoint PSBT input.
func attachExternalTaprootScriptSignatures(in *TransferInput,
	pIn *psbt.PInput) error {

	if len(in.ExternalSignatures) == 0 {
		return nil
	}

	for i := range in.ExternalSignatures {
		externalSig := in.ExternalSignatures[i]
		if externalSig.PubKey == nil {
			return fmt.Errorf("external signature %d pubkey is "+
				"required", i)
		}

		if !bytes.Equal(
			externalSig.WitnessScript,
			in.CustomSpend.SpendInfo.WitnessScript,
		) {
			return fmt.Errorf("external signature %d witness "+
				"script mismatch", i)
		}

		required := false
		for _, spendKey := range in.CustomSpendKeys {
			if spendKey == nil {
				continue
			}

			if bytes.Equal(
				schnorr.SerializePubKey(externalSig.PubKey),
				schnorr.SerializePubKey(spendKey),
			) {

				required = true
				break
			}
		}
		if !required {
			return fmt.Errorf("external signature %d pubkey is "+
				"not required by custom spend path", i)
		}

		err := psbtutil.AddTaprootScriptSpendSig(
			pIn, externalSig.PubKey, externalSig.WitnessScript,
			externalSig.Signature, externalSig.SigHash,
		)
		if err != nil {
			return fmt.Errorf("add external signature %d: %w", i,
				err)
		}
	}

	return nil
}

// assembleCustomFinalWitness builds the FinalScriptWitness for a custom
// spend path that requires condition witness elements beyond signatures.
//
// The witness stack is: [signatures in reverse script-key order,
// ...conditionItems, witnessScript, controlBlock].
func assembleCustomFinalWitness(in *TransferInput, pIn *psbt.PInput) error {
	leafHash := txscript.NewBaseTapLeaf(
		in.CustomSpend.SpendInfo.WitnessScript,
	).TapHash()
	leafHashBytes := leafHash[:]

	if len(in.CustomSpendKeys) == 0 {
		return fmt.Errorf("custom spend key order is required")
	}

	sigsByPubKey := make(map[string][]byte, len(pIn.TaprootScriptSpendSig))

	for _, sigRec := range pIn.TaprootScriptSpendSig {
		if sigRec == nil {
			continue
		}

		if !bytes.Equal(sigRec.LeafHash, leafHashBytes) {
			continue
		}

		sigsByPubKey[string(sigRec.XOnlyPubKey)] = sigRec.Signature
	}

	witnessItems := make(
		[][]byte, 0,
		len(in.CustomSpendKeys)+len(in.CustomSpend.Conditions)+2,
	)
	for i := len(in.CustomSpendKeys) - 1; i >= 0; i-- {
		pubKey := in.CustomSpendKeys[i]
		if pubKey == nil {
			return fmt.Errorf("custom spend key %d is nil", i)
		}

		pubKeyBytes := schnorr.SerializePubKey(pubKey)
		sigBytes := sigsByPubKey[string(pubKeyBytes)]
		if len(sigBytes) == 0 {
			return fmt.Errorf("signature for custom spend key %x "+
				"not found in psbt input", pubKeyBytes)
		}

		witnessItems = append(witnessItems, sigBytes)
	}

	witnessItems = append(witnessItems, in.CustomSpend.Conditions...)
	witnessItems = append(
		witnessItems, in.CustomSpend.SpendInfo.WitnessScript,
	)
	witnessItems = append(
		witnessItems, in.CustomSpend.SpendInfo.ControlBlock,
	)

	// Serialize as BIP-174 FinalScriptWitness: count-prefixed
	// vector of length-prefixed items.
	pIn.FinalScriptWitness = serializeWitness(witnessItems)

	return nil
}

// serializeWitness encodes a witness stack into BIP-174
// FinalScriptWitness format (CompactSize item count followed by
// CompactSize-prefixed items).
func serializeWitness(items [][]byte) []byte {
	var buf bytes.Buffer

	_ = wire.WriteVarInt(&buf, 0, uint64(len(items)))
	for _, item := range items {
		_ = wire.WriteVarInt(&buf, 0, uint64(len(item)))
		buf.Write(item)
	}

	return buf.Bytes()
}
