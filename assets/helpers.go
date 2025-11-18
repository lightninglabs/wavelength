package assets

import (
	"fmt"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/proof"
)

// GenTaprootAssetRootFromProof generates the taproot asset root hash from a
// proof. This root hash is used as the inclusion proof in the taproot control
// block for timeout sweep transactions. It proves the asset commitment is part
// of the taproot tree.
func GenTaprootAssetRootFromProof(prf *proof.Proof) ([]byte, error) {
	// Copy the asset template for commitment calculation.
	assetCopy := prf.Asset.CopySpendTemplate()

	// Create asset commitment using V2 format.
	version := commitment.TapCommitmentV2
	assetCommitment, err := commitment.FromAssets(&version, assetCopy)
	if err != nil {
		return nil, fmt.Errorf("create asset commitment: %w", err)
	}

	// Trim split witnesses from the commitment.
	assetCommitment, err = commitment.TrimSplitWitnesses(
		&version, assetCommitment,
	)
	if err != nil {
		return nil, fmt.Errorf("trim split witnesses: %w", err)
	}

	// Compute the tapscript root (the asset commitment merkle root).
	taprootAssetRoot := assetCommitment.TapscriptRoot(nil)

	return taprootAssetRoot[:], nil
}

// GetSigHash computes the sighash for a PSBT input.
func GetSigHash(pkt *psbt.Packet, inputIndex int) ([32]byte, error) {
	if inputIndex >= len(pkt.Inputs) {
		return [32]byte{}, fmt.Errorf("input index %d out of range",
			inputIndex)
	}

	prevOuts := make(map[wire.OutPoint]*wire.TxOut)
	for idx, input := range pkt.Inputs {
		if input.WitnessUtxo == nil {
			return [32]byte{}, fmt.Errorf("input %d missing "+
				"witness utxo", idx)
		}

		prevOuts[pkt.UnsignedTx.TxIn[idx].PreviousOutPoint] =
			input.WitnessUtxo
	}

	prevOutFetcher := txscript.NewMultiPrevOutFetcher(prevOuts)
	sigHashes := txscript.NewTxSigHashes(pkt.UnsignedTx, prevOutFetcher)

	// For taproot, we use SigHashDefault
	sigHash, err := txscript.CalcTaprootSignatureHash(
		sigHashes, txscript.SigHashDefault, pkt.UnsignedTx,
		inputIndex, prevOutFetcher,
	)
	if err != nil {
		return [32]byte{}, fmt.Errorf("calc sighash: %w", err)
	}

	var sigHashArray [32]byte
	copy(sigHashArray[:], sigHash)

	return sigHashArray, nil
}
