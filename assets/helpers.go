package assets

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/proof"
)

// GenTaprootAssetRootFromProof generates the taproot asset commitment root
// from a proof. This returns ONLY the asset commitment hash, not including any
// tapscript siblings. Use GenTaprootRootFromProof if you need the full taproot
// merkle root including siblings.
//
// This function is used by buildScriptSpendPlans which combines the asset
// commitment with the script tree separately.
//
// When DeriveByAssetInclusion returns multiple commitments (e.g., when there
// are alt leaves or multiple assets), this function selects the commitment that
// produces an output key matching the proof's on-chain anchor output.
func GenTaprootAssetRootFromProof(prf *proof.Proof) ([]byte, error) {
	// Use DeriveByAssetInclusion to reconstruct the exact TapCommitment
	// that was used when creating this output. This preserves the exact
	// MS-SMT tree structure and produces the correct V2 TapLeaf hash.
	keys, err := prf.InclusionProof.DeriveByAssetInclusion(&prf.Asset, nil)
	if err != nil {
		return nil, fmt.Errorf("derive by asset inclusion: %w", err)
	}

	if len(keys) == 0 {
		return nil, fmt.Errorf("no commitment keys derived")
	}

	// Get the actual on-chain output key from the proof's anchor tx.
	outputIdx := prf.InclusionProof.OutputIndex
	if int(outputIdx) >= len(prf.AnchorTx.TxOut) {
		return nil, fmt.Errorf(
			"output index %d out of range", outputIdx,
		)
	}
	anchorOutput := prf.AnchorTx.TxOut[outputIdx]
	if len(anchorOutput.PkScript) != 34 {
		return nil, fmt.Errorf("anchor output not taproot (len=%d)",
			len(anchorOutput.PkScript))
	}
	actualOutputKey := anchorOutput.PkScript[2:] // Skip OP_1 and push byte

	// Get the internal key and sibling hash from the proof.
	internalKey := prf.InclusionProof.InternalKey
	if internalKey == nil {
		return nil, fmt.Errorf("proof missing internal key")
	}

	var siblingHash *chainhash.Hash
	if prf.InclusionProof.CommitmentProof != nil &&
		prf.InclusionProof.CommitmentProof.TapSiblingPreimage != nil {

		h, err := prf.InclusionProof.CommitmentProof.
			TapSiblingPreimage.TapHash()
		if err != nil {
			return nil, fmt.Errorf("compute sibling hash: %w", err)
		}
		siblingHash = h
	}

	// Find the commitment that produces an output key matching the anchor.
	var matchingCommitment *commitment.TapCommitment
	for _, tc := range keys {
		// Compute the tapscript root with this commitment.
		taprootRoot := tc.TapscriptRoot(siblingHash)

		// Compute the output key.
		outputKey := txscript.ComputeTaprootOutputKey(
			internalKey, taprootRoot[:],
		)

		// Compare with actual output (X-coordinate only).
		outputKeyX := outputKey.SerializeCompressed()[1:]
		if bytes.Equal(outputKeyX, actualOutputKey) {
			matchingCommitment = tc
			break
		}
	}

	if matchingCommitment == nil {
		// Fall back to first commitment if no match found.
		for _, c := range keys {
			matchingCommitment = c
			break
		}
	}

	// Return just the asset commitment root (no sibling). The sibling
	// (e.g. script tree) is combined separately in buildScriptSpendPlans.
	taprootAssetRoot := matchingCommitment.TapscriptRoot(nil)

	return taprootAssetRoot[:], nil
}

// GenTaprootRootFromProof generates the full taproot merkle root from a proof.
// This includes both the asset commitment and any tapscript siblings (such as
// OP_TRUE leaves for DirectWalletScript, or script closures for onboarding).
// The result is the exact tweak tapd applied to derive the on-chain output key.
//
// Use this function when you need the full taproot root for keyspend signing,
// where the MuSig2 aggregate must be tweaked with the complete tapscript tree.
func GenTaprootRootFromProof(prf *proof.Proof) ([]byte, error) {
	// Use DeriveByAssetInclusion to reconstruct the exact TapCommitment
	// that was used when creating this output. This traverses the merkle
	// proof in CommitmentProof to ensure we get the same commitment.
	keys, err := prf.InclusionProof.DeriveByAssetInclusion(&prf.Asset, nil)
	if err != nil {
		return nil, fmt.Errorf("derive by asset inclusion: %w", err)
	}

	// There should be exactly one commitment key.
	if len(keys) == 0 {
		return nil, fmt.Errorf("no commitment keys derived")
	}

	// Get the first (and should be only) commitment.
	var tapCommitment *commitment.TapCommitment
	for _, c := range keys {
		tapCommitment = c
		break
	}

	// Check if there's a tapscript sibling.
	var siblingHash *chainhash.Hash
	if prf.InclusionProof.CommitmentProof != nil &&
		prf.InclusionProof.CommitmentProof.TapSiblingPreimage != nil {

		sibling := prf.InclusionProof.CommitmentProof.TapSiblingPreimage
		h, err := sibling.TapHash()
		if err != nil {
			return nil, fmt.Errorf("compute sibling hash: %w", err)
		}
		siblingHash = h
	}

	// Compute the tapscript root. If there's a sibling, this combines the
	// asset commitment leaf with the sibling hash via TapBranchHash.
	taprootRoot := tapCommitment.TapscriptRoot(siblingHash)

	return taprootRoot[:], nil
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
