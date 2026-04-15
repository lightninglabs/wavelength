package oor

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

// CoSignCheckpointPSBTs attaches the operator signature to each checkpoint
// PSBT.
//
// Each checkpoint PSBT is expected to spend exactly one VTXO (input index 0),
// using the standard collaborative VTXO leaf path.
func CoSignCheckpointPSBTs(signer input.Signer,
	operatorKey keychain.KeyDescriptor,
	descs []VTXOSigningDescriptor,
	checkpoints []*psbt.Packet) error {

	switch {
	case signer == nil:
		return fmt.Errorf("signer must be provided")

	case operatorKey.PubKey == nil:
		return fmt.Errorf("operator pubkey must be provided")

	case len(checkpoints) == 0:
		return fmt.Errorf("checkpoint psbts must be provided")
	}

	descByOutpoint := make(
		map[wire.OutPoint]*VTXOSigningDescriptor, len(descs),
	)
	for i := range descs {
		descByOutpoint[descs[i].Outpoint] = &descs[i]
	}

	for i := range checkpoints {
		err := coSignCheckpointPSBT(
			signer, operatorKey, descByOutpoint, checkpoints[i],
		)
		if err != nil {
			return fmt.Errorf("co-sign checkpoint %d: %w", i, err)
		}
	}

	return nil
}

// coSignCheckpointPSBT signs checkpoint input 0 with the operator key.
func coSignCheckpointPSBT(signer input.Signer,
	operatorKey keychain.KeyDescriptor,
	descByOutpoint map[wire.OutPoint]*VTXOSigningDescriptor,
	checkpoint *psbt.Packet) error {

	switch {
	case signer == nil:
		return fmt.Errorf("signer must be provided")

	case operatorKey.PubKey == nil:
		return fmt.Errorf("operator pubkey must be provided")

	case checkpoint == nil || checkpoint.UnsignedTx == nil:
		return fmt.Errorf("checkpoint psbt must include unsigned tx")

	case descByOutpoint == nil:
		return fmt.Errorf("descriptor map must be provided")
	}

	if len(checkpoint.Inputs) != 1 ||
		len(checkpoint.UnsignedTx.TxIn) != 1 {

		return fmt.Errorf("checkpoint must have exactly one input")
	}

	if checkpoint.Inputs[0].WitnessUtxo == nil {
		return fmt.Errorf("checkpoint must include witness utxo")
	}

	prevOutpoint := checkpoint.UnsignedTx.TxIn[0].PreviousOutPoint

	desc := descByOutpoint[prevOutpoint]
	if desc == nil {
		return fmt.Errorf("missing signing descriptor for input %s",
			prevOutpoint)
	}

	prevOut := checkpoint.Inputs[0].WitnessUtxo

	prevFetcher := txscript.NewCannedPrevOutputFetcher(
		prevOut.PkScript, prevOut.Value,
	)

	sigHashes := txscript.NewTxSigHashes(
		checkpoint.UnsignedTx, prevFetcher,
	)

	template, err := decodeDescriptorPolicyTemplate(*desc)
	if err != nil {
		return err
	}

	spendPath, err := decodeDescriptorSpendPath(*desc)
	if err != nil {
		return err
	}

	// Resolve the AST leaf that the client's spend path targets. The
	// lookup both confirms the spend path matches one of the policy's
	// compiled leaves and returns the semantic AST node so we can
	// gate co-signing on AST-level key membership rather than a
	// byte-level substring scan over the compiled witness script.
	leafNode, err := resolveSpendPathLeaf(template, spendPath)
	if err != nil {
		return err
	}

	if !arkscript.ContainsKey(leafNode, operatorKey.PubKey) {
		return fmt.Errorf(
			"spend path leaf does not contain operator key",
		)
	}

	witnessScript := spendPath.WitnessScript
	controlBlock := spendPath.ControlBlock

	signDesc := &input.SignDescriptor{
		KeyDesc:           operatorKey,
		SignMethod:        input.TaprootScriptSpendSignMethod,
		Output:            prevOut,
		HashType:          txscript.SigHashDefault,
		SigHashes:         sigHashes,
		PrevOutputFetcher: prevFetcher,
		InputIndex:        0,
		WitnessScript:     witnessScript,
		ControlBlock:      controlBlock,
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
		&checkpoint.Inputs[0], witnessScript, controlBlock,
	)
	if err != nil {
		return err
	}

	return addTaprootScriptSpendSig(
		&checkpoint.Inputs[0], operatorKey.PubKey,
		witnessScript, sigBytes, signDesc.HashType,
	)
}

// addTapLeafScriptRaw ensures the checkpoint PSBT input includes the leaf
// script and control block.
func addTapLeafScriptRaw(in *psbt.PInput, witnessScript,
	controlBlock []byte) error {

	if in == nil {
		return fmt.Errorf("psbt input must be provided")
	}

	if len(witnessScript) == 0 || len(controlBlock) == 0 {
		return fmt.Errorf("witness script and control block must " +
			"be provided")
	}

	needle := &psbt.TaprootTapLeafScript{
		ControlBlock: controlBlock,
		Script:       witnessScript,
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

// addTaprootScriptSpendSig adds/replaces a taproot script spend signature in
// the PSBT input.
func addTaprootScriptSpendSig(in *psbt.PInput, pubKey *btcec.PublicKey,
	leafScript []byte, sig []byte, sigHash txscript.SigHashType) error {

	switch {
	case in == nil:
		return fmt.Errorf("psbt input must be provided")

	case pubKey == nil:
		return fmt.Errorf("pubkey must be provided")

	case len(leafScript) == 0:
		return fmt.Errorf("leaf script must be provided")

	case len(sig) == 0:
		return fmt.Errorf("signature must be provided")
	}

	leafHash := txscript.NewBaseTapLeaf(leafScript).TapHash()
	leafHashBytes := make([]byte, 0, len(leafHash))
	leafHashBytes = append(leafHashBytes, leafHash[:]...)

	needle := &psbt.TaprootScriptSpendSig{
		XOnlyPubKey: schnorr.SerializePubKey(pubKey),
		LeafHash:    leafHashBytes,
		Signature:   sig,
		SigHash:     sigHash,
	}

	for i := range in.TaprootScriptSpendSig {
		existing := in.TaprootScriptSpendSig[i]
		if existing == nil {
			continue
		}

		if existing.EqualKey(needle) {
			in.TaprootScriptSpendSig[i] = needle
			return nil
		}
	}

	in.TaprootScriptSpendSig = append(in.TaprootScriptSpendSig, needle)

	return nil
}
