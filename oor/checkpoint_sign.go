package oor

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/tx/psbtutil"
	"github.com/lightningnetwork/lnd/input"
)

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
			return fmt.Errorf(
				"checkpoint psbt must include unsigned tx",
			)
		}

		if len(checkpoint.UnsignedTx.TxIn) != 1 ||
			len(checkpoint.Inputs) != 1 {

			return fmt.Errorf(
				"checkpoint must have exactly one input",
			)
		}

		prevOutpoint := checkpoint.UnsignedTx.TxIn[0].PreviousOutPoint
		in := inputByOutpoint[prevOutpoint]
		if in == nil {
			return fmt.Errorf(
				"unknown checkpoint input outpoint %s",
				prevOutpoint,
			)
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
// valid operator script-spend signature for the spend path described by
// in.SpendInfo.
func validateSingleOperatorCheckpointSignature(in *TransferInput,
	checkpoint *psbt.Packet) error {

	vtxo := in.VTXO
	if vtxo == nil || vtxo.OperatorKey == nil {
		return fmt.Errorf("operator key must be provided")
	}

	// SpendInfo is required; the caller must supply it via the arkscript
	// system so we know which leaf script and control block to verify
	// against.
	if in.SpendInfo == nil {
		return fmt.Errorf("spend info must be provided")
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

	// Use the caller-supplied spend path bytes directly; no tapscript
	// reconstruction needed.
	leafScript := in.SpendInfo.WitnessScript
	ctrlBlock := in.SpendInfo.ControlBlock

	err := requireTapLeafScript(
		&checkpoint.Inputs[0], leafScript, ctrlBlock,
	)
	if err != nil {
		return err
	}

	sigRec, err := findTaprootScriptSpendSig(
		&checkpoint.Inputs[0], vtxo.OperatorKey, leafScript,
	)
	if err != nil {
		return err
	}

	return verifyTaprootScriptSpendSig(
		checkpoint.UnsignedTx, 0, prevFetcher, leafScript, sigRec,
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
		return nil, fmt.Errorf(
			"invalid schnorr signature size: %d", len(raw),
		)
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
		sigHashes,
		sigRec.SigHash,
		tx,
		inputIndex,
		prevFetcher,
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
// spend path described by in.SpendInfo. The spend path is generic: it may be
// the standard VTXO collab leaf, the vHTLC Claim leaf, or any other
// arkscript-compiled leaf; no script reconstruction is performed here.
//
// When in.ConditionWitness is non-empty (e.g. a preimage for a HashLock-
// gated leaf), the function assembles FinalScriptWitness explicitly after
// both the operator and client signatures are present.
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

	if len(checkpoint.UnsignedTx.TxIn) != 1 ||
		len(checkpoint.Inputs) != 1 {

		return fmt.Errorf("checkpoint psbt must have exactly one "+
			"input, got tx=%d psbt=%d",
			len(checkpoint.UnsignedTx.TxIn),
			len(checkpoint.Inputs))
	}

	// SpendInfo is required at signing time; callers must derive it via
	// the arkscript system before calling this function.
	if in.SpendInfo == nil {
		return fmt.Errorf("spend info required for checkpoint " +
			"signing")
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

	// Build the sign descriptor directly from the caller-supplied spend
	// path bytes; no tapscript reconstruction is performed.
	leafScript := in.SpendInfo.WitnessScript
	signDesc := &input.SignDescriptor{
		KeyDesc:           in.VTXO.ClientKey,
		SignMethod:        input.TaprootScriptSpendSignMethod,
		Output:            prevOut,
		HashType:          txscript.SigHashDefault,
		SigHashes:         sigHashes,
		PrevOutputFetcher: prevFetcher,
		InputIndex:        0,
		WitnessScript:     leafScript,
	}

	sig, err := signer.SignOutputRaw(checkpoint.UnsignedTx, signDesc)
	if err != nil {
		return fmt.Errorf("sign output: %w", err)
	}

	sigBytes := sig.Serialize()
	if len(sigBytes) == 0 {
		return fmt.Errorf("signer returned empty signature")
	}

	err = addTapLeafScriptRaw(
		&checkpoint.Inputs[0],
		in.SpendInfo.WitnessScript,
		in.SpendInfo.ControlBlock,
	)
	if err != nil {
		return err
	}

	err = psbtutil.AddTaprootScriptSpendSig(
		&checkpoint.Inputs[0], in.VTXO.ClientKey.PubKey,
		in.SpendInfo.WitnessScript, sigBytes, signDesc.HashType,
	)
	if err != nil {
		return err
	}

	// For script paths with condition witness items (e.g. a HashLock
	// preimage), assemble the final segwit witness explicitly so the PSBT
	// finalizer can broadcast without further signing.
	if len(in.ConditionWitness) > 0 {
		return assembleFinalScriptWitness(checkpoint, in)
	}

	return nil
}

// addTapLeafScriptRaw adds the given leaf script and control block to the
// PSBT input's TaprootLeafScript list. If the leaf is already present the
// function is a no-op.
func addTapLeafScriptRaw(in *psbt.PInput, leafScript,
	controlBlock []byte) error {

	if in == nil {
		return fmt.Errorf("psbt input must be provided")
	}

	if len(leafScript) == 0 {
		return fmt.Errorf("leaf script must be provided")
	}

	if len(controlBlock) == 0 {
		return fmt.Errorf("control block must be provided")
	}

	needle := &psbt.TaprootTapLeafScript{
		ControlBlock: controlBlock,
		Script:       leafScript,
		LeafVersion:  txscript.BaseLeafVersion,
	}

	for i := range in.TaprootLeafScript {
		existing := in.TaprootLeafScript[i]
		if existing == nil {
			continue
		}

		if bytes.Equal(existing.ControlBlock, needle.ControlBlock) &&
			bytes.Equal(existing.Script, needle.Script) &&
			existing.LeafVersion == needle.LeafVersion {

			return nil
		}
	}

	in.TaprootLeafScript = append(in.TaprootLeafScript, needle)

	return nil
}

// assembleFinalScriptWitness constructs the full segwit witness for a script-
// path spend that requires condition witness items (e.g. a preimage for a
// HashLock leaf) between the signatures and the leaf script.
//
// It locates the operator and client signatures in TaprootScriptSpendSig by
// x-only public key, then assembles the witness as:
//
//	[op_sig, client_sig, cond_item_0, ..., cond_item_N, leaf_script, ctrl]
//
// The result is stored as FinalScriptWitness on the checkpoint input so the
// PSBT finalizer can broadcast without further signing.
func assembleFinalScriptWitness(checkpoint *psbt.Packet,
	in *TransferInput) error {

	if checkpoint == nil || len(checkpoint.Inputs) == 0 {
		return fmt.Errorf("checkpoint psbt must have inputs")
	}

	vtxo := in.VTXO
	if vtxo == nil || vtxo.OperatorKey == nil ||
		vtxo.ClientKey.PubKey == nil {

		return fmt.Errorf("vtxo operator and client keys must " +
			"be provided")
	}

	// Locate each party's signature by their x-only public key.
	opXOnly := schnorr.SerializePubKey(vtxo.OperatorKey)
	clientXOnly := schnorr.SerializePubKey(vtxo.ClientKey.PubKey)

	var (
		opSig     []byte
		clientSig []byte
	)

	pInput := &checkpoint.Inputs[0]
	for _, sigRec := range pInput.TaprootScriptSpendSig {
		if sigRec == nil {
			continue
		}

		switch {
		case bytes.Equal(sigRec.XOnlyPubKey, opXOnly):
			opSig = sigRec.Signature

		case bytes.Equal(sigRec.XOnlyPubKey, clientXOnly):
			clientSig = sigRec.Signature
		}
	}

	if len(opSig) == 0 {
		return fmt.Errorf("operator signature not found in " +
			"checkpoint psbt")
	}

	if len(clientSig) == 0 {
		return fmt.Errorf("client signature not found in " +
			"checkpoint psbt")
	}

	// Build the witness stack: signatures first (deepest on stack),
	// then condition items (e.g. preimage), then the spend script
	// and its control block.
	witness := make(wire.TxWitness, 0,
		2+len(in.ConditionWitness)+2)
	witness = append(witness, opSig, clientSig)
	witness = append(witness, in.ConditionWitness...)
	witness = append(
		witness,
		in.SpendInfo.WitnessScript,
		in.SpendInfo.ControlBlock,
	)

	pInput.FinalScriptWitness = serializeWitness(witness)

	return nil
}

// serializeWitness encodes a wire.TxWitness into the PSBT FinalScriptWitness
// format: <varint count> (<varint len><bytes>)...
func serializeWitness(w wire.TxWitness) []byte {
	var buf bytes.Buffer
	_ = wire.WriteVarInt(&buf, 0, uint64(len(w)))
	for _, item := range w {
		_ = wire.WriteVarBytes(&buf, 0, item)
	}

	return buf.Bytes()
}
