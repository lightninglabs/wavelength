package tapassets

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/tap-sdk/customanchor"
)

// AssetVTXOClaimInput is one asset piece of an asset VTXO, as a claim of the
// VTXO takes it from the VTXO's sealed package. A VTXO holds one piece per
// issuance of its asset.
type AssetVTXOClaimInput struct {
	// Amount is the number of units the piece holds.
	Amount uint64

	// ProofPath ends in the piece. Every step confirms once the VTXO's
	// tree path is on chain.
	ProofPath *customanchor.ProofPath

	// OPTrueWitness spends the piece under its OP_TRUE script key.
	OPTrueWitness [][]byte
}

// DecodeAssetVTXOClaimInputs reads the asset pieces of the asset VTXO at leaf
// from the sealed package of the leaf's transaction.
func DecodeAssetVTXOClaimInputs(sealedPackage []byte,
	leaf wire.OutPoint) ([]AssetVTXOClaimInput, error) {

	committed, err := decodeSealedPackage(sealedPackage)
	if err != nil {
		return nil, err
	}
	inputs, err := claimInputsFromCommit(committed, leaf)
	if err != nil {
		return nil, err
	}
	for idx := range inputs {
		if err := inputs[idx].ProofPath.Validate(); err != nil {
			return nil, fmt.Errorf("asset VTXO proof path %d: %w",
				idx, err)
		}
	}

	return inputs, nil
}

// claimInputsFromCommit extends the proof path of each leaf asset's input
// with the leaf transition.
func claimInputsFromCommit(committed *commitResult,
	leaf wire.OutPoint) ([]AssetVTXOClaimInput, error) {

	var inputs []AssetVTXOClaimInput
	for idx := range committed.outputs {
		output := &committed.outputs[idx]
		if output.anchorOutpoint != sdkOutpoint(leaf) {
			continue
		}

		claimInput, err := claimInputFromOutput(committed, output)
		if err != nil {
			return nil, fmt.Errorf("asset output %d in %v: %w", idx,
				leaf, err)
		}
		inputs = append(inputs, *claimInput)
	}
	if len(inputs) == 0 {
		return nil, fmt.Errorf("sealed package holds no asset in %v",
			leaf)
	}

	return inputs, nil
}

// claimInputFromOutput extends the proof path of one leaf asset's input with
// the leaf transition.
func claimInputFromOutput(committed *commitResult,
	output *commitOutput) (*AssetVTXOClaimInput, error) {

	if output.scriptMode != customanchor.AssetScriptOPTrue ||
		len(output.opTrueWitness) == 0 {
		return nil, fmt.Errorf("asset is not OP_TRUE locked")
	}

	var input *commitInput
	for idx := range committed.inputs {
		candidate := &committed.inputs[idx]
		if candidate.packetRole != output.packetRole ||
			candidate.packetIndex != output.packetIndex {

			continue
		}
		if input != nil {
			return nil, fmt.Errorf("leaf transition merges asset " +
				"inputs")
		}
		input = candidate
	}
	if input == nil {
		return nil, fmt.Errorf("leaf transition has no asset input")
	}

	path, err := inputProofPath(input.proofSource)
	if err != nil {
		return nil, err
	}
	path.Steps = append(path.Steps, customanchor.ProofPathStep{
		TransitionProof: bytes.Clone(output.proofBlob),
	})

	return &AssetVTXOClaimInput{
		Amount:        output.amount,
		ProofPath:     path,
		OPTrueWitness: cloneByteSlices(output.opTrueWitness),
	}, nil
}
