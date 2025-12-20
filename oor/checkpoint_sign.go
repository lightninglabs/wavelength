package oor

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	"github.com/lightninglabs/darepo-client/lib/tx"
	"github.com/lightningnetwork/lnd/input"
)

// SignCheckpointPSBTs attaches the client-side collaborative VTXO spend
// signatures to each checkpoint PSBT.
//
// Each checkpoint PSBT is expected to spend exactly one VTXO (input index 0).
// The TransferInput slice is expected to match the checkpoint PSBT slice
// 1:1.
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

	for i := range inputs {
		err := signCheckpointPSBT(signer, &inputs[i], checkpoints[i])
		if err != nil {
			return fmt.Errorf("sign checkpoint %d: %w", i, err)
		}
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

	if len(checkpoint.UnsignedTx.TxIn) != 1 ||
		len(checkpoint.Inputs) != 1 {

		return fmt.Errorf("checkpoint psbt must have exactly one "+
			"input, got tx=%d psbt=%d",
			len(checkpoint.UnsignedTx.TxIn),
			len(checkpoint.Inputs))
	}

	prevOut := &wire.TxOut{
		Value:    int64(in.VTXO.Amount),
		PkScript: in.VTXO.PkScript,
	}

	prevFetcher := txscript.NewCannedPrevOutputFetcher(
		prevOut.PkScript, prevOut.Value,
	)

	sigHashes := txscript.NewTxSigHashes(checkpoint.UnsignedTx, prevFetcher)

	signDesc, spendInfo, err := tx.NewVTXOCollabSignDescriptor(
		&tx.VTXOSpendContext{
			Outpoint:  in.VTXO.Outpoint,
			Output:    prevOut,
			TapScript: in.VTXO.TapScript,
		},
		in.VTXO.ClientKey,
		0,
		sigHashes,
		prevFetcher,
	)
	if err != nil {
		return err
	}

	sig, err := signer.SignOutputRaw(checkpoint.UnsignedTx, signDesc)
	if err != nil {
		return fmt.Errorf("sign output: %w", err)
	}

	sigBytes := sig.Serialize()
	if len(sigBytes) == 0 {
		return fmt.Errorf("signer returned empty signature")
	}

	err = addTapLeafScript(&checkpoint.Inputs[0], spendInfo)
	if err != nil {
		return err
	}

	return addTaprootScriptSpendSig(
		&checkpoint.Inputs[0], in.VTXO.ClientKey.PubKey,
		spendInfo.WitnessScript, sigBytes, signDesc.HashType,
	)
}

// addTapLeafScript ensures the checkpoint PSBT input includes the leaf script
// and control block for the collaborative VTXO leaf.
func addTapLeafScript(in *psbt.PInput, spendInfo *scripts.VTXOSpendData) error {
	if in == nil {
		return fmt.Errorf("psbt input must be provided")
	}

	if spendInfo == nil {
		return fmt.Errorf("spend info must be provided")
	}

	needle := &psbt.TaprootTapLeafScript{
		ControlBlock: spendInfo.ControlBlock,
		Script:       spendInfo.WitnessScript,
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
