package oor

import (
	"fmt"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/tx/psbtutil"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

// SignArkPSBT signs the Ark PSBT inputs using the client key on the owner
// leaf path.
//
// Each Ark input spends a checkpoint output. The checkpoint output is a
// taproot output with an owner leaf (single checksig) and an operator CSV
// leaf. This function maps each Ark input back to its checkpoint and then to
// the transfer input that holds the client signing key.
//
// The signing pattern follows SignCheckpointPSBTs: for each input, build a
// taproot script-spend SignDescriptor using the TaprootLeafScript metadata
// already attached to the Ark PSBT input, then sign and attach the signature.
func SignArkPSBT(signer input.Signer, arkPSBT *psbt.Packet,
	checkpointPSBTs []*psbt.Packet,
	transferInputs []TransferInput) error {

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

	for i := range arkPSBT.UnsignedTx.TxIn {
		err := signArkPSBTInput(
			signer, arkPSBT, i, inputByCheckpointTxid,
		)
		if err != nil {
			return fmt.Errorf("sign ark input %d: %w", i, err)
		}
	}

	return nil
}

// checkpointInputEntry pairs a transfer input with its checkpoint txid for
// Ark input signing.
type checkpointInputEntry struct {
	transferInput *TransferInput
}

// buildCheckpointInputMap creates a lookup from checkpoint txid to the
// corresponding transfer input.
func buildCheckpointInputMap(checkpointPSBTs []*psbt.Packet,
	transferInputs []TransferInput) (
	map[chainhash.Hash]*checkpointInputEntry, error) {

	if len(checkpointPSBTs) != len(transferInputs) {
		return nil, fmt.Errorf("checkpoint count %d does not match "+
			"transfer input count %d",
			len(checkpointPSBTs), len(transferInputs))
	}

	result := make(
		map[chainhash.Hash]*checkpointInputEntry,
		len(checkpointPSBTs),
	)

	for i, cp := range checkpointPSBTs {
		if cp == nil || cp.UnsignedTx == nil {
			return nil, fmt.Errorf(
				"checkpoint %d: missing unsigned tx", i,
			)
		}

		cpTxid := cp.UnsignedTx.TxHash()
		result[cpTxid] = &checkpointInputEntry{
			transferInput: &transferInputs[i],
		}
	}

	return result, nil
}

// signArkPSBTInput signs a single Ark PSBT input using the owner leaf.
func signArkPSBTInput(signer input.Signer, arkPSBT *psbt.Packet,
	inputIndex int,
	inputMap map[chainhash.Hash]*checkpointInputEntry) error {

	prevOut := arkPSBT.UnsignedTx.TxIn[inputIndex].PreviousOutPoint
	entry, ok := inputMap[prevOut.Hash]
	if !ok {
		return fmt.Errorf("no transfer input for checkpoint "+
			"txid %s", prevOut.Hash)
	}

	in := entry.transferInput
	if err := in.Validate(); err != nil {
		return err
	}

	pInput := &arkPSBT.Inputs[inputIndex]

	// The TaprootLeafScript metadata was attached during package
	// construction. Extract the owner leaf script and control block.
	if len(pInput.TaprootLeafScript) == 0 {
		return fmt.Errorf("ark input %d: missing tapleaf script",
			inputIndex)
	}

	leaf := pInput.TaprootLeafScript[0]
	ownerLeafScript := leaf.Script
	controlBlock := leaf.ControlBlock

	// Build the prevout for the checkpoint output being spent.
	witnessUtxo := pInput.WitnessUtxo
	if witnessUtxo == nil {
		return fmt.Errorf("ark input %d: missing witness utxo",
			inputIndex)
	}

	prevFetcher := txscript.NewCannedPrevOutputFetcher(
		witnessUtxo.PkScript, witnessUtxo.Value,
	)
	sigHashes := txscript.NewTxSigHashes(
		arkPSBT.UnsignedTx, prevFetcher,
	)

	signDesc := &input.SignDescriptor{
		KeyDesc: keychain.KeyDescriptor{
			PubKey: in.VTXO.ClientKey.PubKey,
		},
		SignMethod: input.TaprootScriptSpendSignMethod,
		Output: &wire.TxOut{
			Value:    witnessUtxo.Value,
			PkScript: witnessUtxo.PkScript,
		},
		HashType:          txscript.SigHashDefault,
		SigHashes:         sigHashes,
		PrevOutputFetcher: prevFetcher,
		InputIndex:        inputIndex,
		WitnessScript:     ownerLeafScript,
		ControlBlock:      controlBlock,
	}

	sig, err := signer.SignOutputRaw(arkPSBT.UnsignedTx, signDesc)
	if err != nil {
		return fmt.Errorf("sign output: %w", err)
	}

	sigBytes := sig.Serialize()
	if len(sigBytes) == 0 {
		return fmt.Errorf("signer returned empty signature")
	}

	return psbtutil.AddTaprootScriptSpendSig(
		pInput, in.VTXO.ClientKey.PubKey,
		ownerLeafScript, sigBytes, signDesc.HashType,
	)
}
