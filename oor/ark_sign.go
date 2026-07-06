package oor

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/lib/tx/psbtutil"
	"github.com/lightningnetwork/lnd/input"
)

// SignArkPSBT signs the Ark PSBT inputs using the client key on the
// checkpoint collab leaf path.
//
// Each Ark input spends a checkpoint output. The checkpoint output is a
// taproot output with a 2-of-2 collaborative multisig leaf (owner +
// operator) and an operator CSV leaf. This function provides the client's
// half of the 2-of-2 signature. The operator provides their half during
// the submit phase.
//
// The signing pattern follows SignCheckpointPSBTs: for each input, build a
// taproot script-spend SignDescriptor using the TaprootLeafScript metadata
// already attached to the Ark PSBT input, then sign and attach the signature.
func SignArkPSBT(signer input.Signer, arkPSBT *psbt.Packet,
	checkpointPSBTs []*psbt.Packet, transferInputs []TransferInput) error {

	switch {
	case signer == nil:
		return fmt.Errorf("signer must be provided")

	case arkPSBT == nil || arkPSBT.UnsignedTx == nil:
		return fmt.Errorf("ark psbt must include unsigned tx")

	case len(checkpointPSBTs) == 0:
		return fmt.Errorf("checkpoint psbts must be provided")

	case len(transferInputs) == 0:
		return fmt.Errorf("transfer inputs must be provided")
	}

	// Build a map from checkpoint txid → transfer input index. Each
	// checkpoint spends exactly one VTXO input (index 0), so we match
	// via the checkpoint's input prevout.
	inputByCheckpointTxid, err := buildCheckpointInputMap(
		checkpointPSBTs, transferInputs,
	)
	if err != nil {
		return err
	}

	// Build a complete prevout map from all Ark PSBT inputs.
	// BIP-341 SigHashDefault commits to ALL prevouts (sha_prevouts,
	// sha_amounts, sha_scriptpubkeys), so we need a MultiPrevOutFetcher
	// that returns the correct TxOut for each input. Using
	// CannedPrevOutputFetcher would produce incorrect sighashes for
	// multi-input Ark transactions.
	prevOuts := make(
		map[wire.OutPoint]*wire.TxOut, len(arkPSBT.UnsignedTx.TxIn),
	)
	for i, txIn := range arkPSBT.UnsignedTx.TxIn {
		wu := arkPSBT.Inputs[i].WitnessUtxo
		if wu == nil {
			return fmt.Errorf("ark input %d: missing witness utxo",
				i)
		}

		prevOuts[txIn.PreviousOutPoint] = wu
	}

	prevFetcher := txscript.NewMultiPrevOutFetcher(prevOuts)
	sigHashes := txscript.NewTxSigHashes(
		arkPSBT.UnsignedTx, prevFetcher,
	)

	for i := range arkPSBT.UnsignedTx.TxIn {
		err := signArkPSBTInput(
			signer, arkPSBT, i, inputByCheckpointTxid, prevFetcher,
			sigHashes,
		)
		if err != nil {
			return fmt.Errorf("sign ark input %d: %w", i, err)
		}
	}

	return nil
}

// buildCheckpointInputMap creates a lookup from checkpoint txid to the
// corresponding transfer input.
func buildCheckpointInputMap(checkpointPSBTs []*psbt.Packet,
	transferInputs []TransferInput) (map[chainhash.Hash]*TransferInput,
	error) {

	if len(checkpointPSBTs) != len(transferInputs) {
		return nil, fmt.Errorf("checkpoint count %d does not match "+
			"transfer input count %d", len(checkpointPSBTs),
			len(transferInputs))
	}

	result := make(
		map[chainhash.Hash]*TransferInput, len(checkpointPSBTs),
	)

	for i, cp := range checkpointPSBTs {
		if cp == nil || cp.UnsignedTx == nil {
			return nil, fmt.Errorf("checkpoint %d: missing "+
				"unsigned tx", i)
		}

		cpTxid := cp.UnsignedTx.TxHash()
		result[cpTxid] = &transferInputs[i]
	}

	return result, nil
}

// signArkPSBTInput signs a single Ark PSBT input using the checkpoint
// collab leaf (2-of-2 multisig between owner and operator).
func signArkPSBTInput(signer input.Signer, arkPSBT *psbt.Packet, inputIndex int,
	inputMap map[chainhash.Hash]*TransferInput,
	prevFetcher txscript.PrevOutputFetcher,
	sigHashes *txscript.TxSigHashes) error {

	prevOut := arkPSBT.UnsignedTx.TxIn[inputIndex].PreviousOutPoint
	in, ok := inputMap[prevOut.Hash]
	if !ok {
		return fmt.Errorf("no transfer input for checkpoint txid %s",
			prevOut.Hash)
	}
	if err := in.Validate(); err != nil {
		return err
	}

	pInput := &arkPSBT.Inputs[inputIndex]

	// Find the collab leaf by matching against the expected owner
	// leaf script from the transfer input. Do not assume a fixed
	// index since additional leaves may be present or the order
	// may vary.
	collabLeaf, err := findTapLeafByScript(
		pInput, in.OwnerLeafScript,
	)
	if err != nil {
		return fmt.Errorf("ark input %d: %w", inputIndex, err)
	}

	witnessUtxo := pInput.WitnessUtxo
	if witnessUtxo == nil {
		return fmt.Errorf("ark input %d: missing witness utxo",
			inputIndex)
	}

	// Pass the full KeyDescriptor (including KeyLocator) so
	// signers that require the locator can find the private key.
	signDesc := &input.SignDescriptor{
		KeyDesc:    in.VTXO.ClientKey,
		SignMethod: input.TaprootScriptSpendSignMethod,
		Output: &wire.TxOut{
			Value:    witnessUtxo.Value,
			PkScript: witnessUtxo.PkScript,
		},
		HashType:          txscript.SigHashDefault,
		SigHashes:         sigHashes,
		PrevOutputFetcher: prevFetcher,
		InputIndex:        inputIndex,
		WitnessScript:     collabLeaf.Script,
		ControlBlock:      collabLeaf.ControlBlock,
	}

	sig, err := signer.SignOutputRaw(arkPSBT.UnsignedTx, signDesc)
	if err != nil {
		return fmt.Errorf("sign output: %w", err)
	}

	sigBytes := sig.Serialize()
	if len(sigBytes) == 0 {
		return fmt.Errorf("signer returned empty signature")
	}

	err = psbtutil.AddTaprootScriptSpendSig(
		pInput, in.VTXO.ClientKey.PubKey, collabLeaf.Script, sigBytes,
		signDesc.HashType,
	)
	if err != nil {
		return err
	}

	// Preserve condition witness items for custom spends so later submit
	// validation can reconstruct the full tapscript witness after the
	// operator attaches its signature.
	if in.CustomSpend != nil && len(in.CustomSpend.Conditions) > 0 {
		err = arkscript.PutConditionWitnessPSBTInput(
			arkPSBT, inputIndex, in.CustomSpend.Conditions,
		)
		if err != nil {
			return fmt.Errorf("store custom condition witness: %w",
				err)
		}
	}

	return nil
}

// findTapLeafByScript searches the PSBT input's TaprootLeafScript entries
// for one whose script matches the expected leaf bytes.
func findTapLeafByScript(pInput *psbt.PInput,
	expectedScript []byte) (*psbt.TaprootTapLeafScript, error) {

	if len(pInput.TaprootLeafScript) == 0 {
		return nil, fmt.Errorf("missing tapleaf scripts")
	}

	for _, leaf := range pInput.TaprootLeafScript {
		if leaf == nil {
			continue
		}

		if bytes.Equal(leaf.Script, expectedScript) {
			return leaf, nil
		}
	}

	return nil, fmt.Errorf("collab leaf script not found in tapleaf " +
		"entries")
}
