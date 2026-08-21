package tapassets

import (
	"bytes"
	"crypto/sha256"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
)

const (
	opTrueKeyDomain       = "wavelength/assets/optrue/v0/"
	witnessBackendSigner  = tapsdk.CustomAssetWitnessBackendSigner
	witnessCallerProvided = tapsdk.CustomAssetWitnessCallerProvided
	scriptExternal        = tapsdk.CustomAssetScriptExternal
)

type expectedUnconfirmedAnchor struct {
	previousOutpoint tapsdk.Outpoint
	anchorOutpoint   tapsdk.Outpoint
	transaction      []byte
}

type assetSpendSource struct {
	proofFile []byte
	proofPath *tapsdk.AssetProofPath
	witness   tapsdk.CustomAssetWitnessPlan
	verifier  tapsdk.ConfirmedProofVerifier
}

func (s *assetSpendSource) customInput(id string, assetRef tapsdk.AssetRef,
	amount uint64) (tapsdk.CustomAssetInput, error) {

	if s == nil {
		return tapsdk.CustomAssetInput{}, fmt.Errorf("asset proof " +
			"source is required")
	}

	input := tapsdk.CustomAssetInput{
		ID:       id,
		AssetRef: assetRef,
		Amount:   amount,
		Witness: tapsdk.CustomAssetWitnessPlan{
			Mode:  s.witness.Mode,
			Stack: cloneByteSlices(s.witness.Stack),
		},
	}

	switch {
	case len(s.proofFile) != 0:
		input.ProofFile = append([]byte(nil), s.proofFile...)

	case s.proofPath != nil:
		input.ProofPath = s.proofPath.Clone()

	default:
		return tapsdk.CustomAssetInput{}, fmt.Errorf("asset proof " +
			"source is empty")
	}

	return input, nil
}

func (s *assetSpendSource) appendTransition(proofBlob []byte,
	witness [][]byte) (tapsdk.CustomAssetInput, error) {

	if s == nil || len(proofBlob) == 0 {
		return tapsdk.CustomAssetInput{}, fmt.Errorf("asset " +
			"transition proof is required")
	}

	var path *tapsdk.AssetProofPath
	if s.proofPath != nil {
		path = s.proofPath.Clone()
	} else {
		path = &tapsdk.AssetProofPath{
			Version: tapsdk.AssetProofPathVersionV0,
			ConfirmedBaseProof: append(
				[]byte(nil), s.proofFile...,
			),
		}
	}

	if len(path.Steps) >= tapsdk.AssetProofPathMaxDepth {
		return tapsdk.CustomAssetInput{}, fmt.Errorf("asset proof " +
			"path has no remaining transition slot")
	}
	path.Steps = append(path.Steps, tapsdk.AssetProofPathStep{
		TransitionProof: append([]byte(nil), proofBlob...),
	})

	return tapsdk.CustomAssetInput{
		ProofPath: path,
		Witness: tapsdk.CustomAssetWitnessPlan{
			Mode:  witnessCallerProvided,
			Stack: cloneByteSlices(witness),
		},
	}, nil
}

func validateTreeProofCapacity(root TreeRootAssetSource,
	additionalSteps int) error {

	if additionalSteps <= 0 {
		return fmt.Errorf("asset tree depth must be positive")
	}

	var (
		currentDepth int
		currentSize  int
	)

	switch {
	case root.proofPath != nil:
		currentDepth = len(root.proofPath.Steps)
		if currentDepth+additionalSteps >
			tapsdk.AssetProofPathMaxDepth {
			return fmt.Errorf("asset proof path depth %d cannot "+
				"add %d tree transitions", currentDepth,
				additionalSteps)
		}

		encoded, err := root.proofPath.MarshalBinary()
		if err != nil {
			return fmt.Errorf("encode root proof path: %w", err)
		}
		currentSize = len(encoded)

	case len(root.ProofPath) != 0:
		var path tapsdk.AssetProofPath
		if err := path.UnmarshalBinary(root.ProofPath); err != nil {
			return fmt.Errorf("decode root proof path: %w", err)
		}
		currentDepth = len(path.Steps)
		currentSize = len(root.ProofPath)

	case len(root.ProofFile) != 0:
		maxProofSize := tapsdk.AssetProofPathMaxConfirmedProofSize
		if len(root.ProofFile) > maxProofSize {
			return fmt.Errorf("confirmed asset proof exceeds "+
				"%d bytes", maxProofSize)
		}
		currentSize = len(root.ProofFile)

	default:
		return fmt.Errorf("root proof source is empty")
	}

	if currentDepth+additionalSteps > tapsdk.AssetProofPathMaxDepth {
		return fmt.Errorf("asset proof path depth %d cannot add %d "+
			"tree transitions", currentDepth, additionalSteps)
	}

	stepBudget := uint64(additionalSteps) *
		uint64(4+tapsdk.AssetProofPathMaxStepSize)
	if uint64(currentSize)+stepBudget > tapsdk.AssetProofPathMaxSize {
		return fmt.Errorf("asset proof path lacks space for %d tree "+
			"transitions", additionalSteps)
	}

	return nil
}

func callerFundedExact() tapsdk.CustomAnchorFundingPlan {
	return tapsdk.CustomAnchorFundingPlan{
		Mode:              tapsdk.CustomAnchorFundingCallerFundedExact,
		CallerFundedExact: &tapsdk.CustomAnchorCallerFundedExact{},
	}
}

func anchorPlan(internalKey *btcec.PublicKey,
	leaves []txscript.TapLeaf) tapsdk.CustomAnchorOutputPlan {

	pubKey, _ := tapsdk.ParsePubKey(internalKey.SerializeCompressed())
	sdkLeaves := make([]tapsdk.TapLeaf, len(leaves))
	for idx := range leaves {
		sdkLeaves[idx] = tapsdk.TapLeaf{
			Script: append([]byte(nil), leaves[idx].Script...),
		}
	}

	return tapsdk.CustomAnchorOutputPlan{
		InternalKey: tapsdk.InternalKey{
			PubKey: pubKey,
		},
		Tapscript: tapsdk.CustomAnchorTapscriptPlan{
			TapLeaves: sdkLeaves,
		},
	}
}

func deterministicKey(digest tapsdk.Hash, domain string) tapsdk.PubKey {
	seed := sha256.Sum256(
		append(
			append(
				[]byte(opTrueKeyDomain), domain...,
			),
			digest[:]...,
		),
	)
	_, publicKey := btcec.PrivKeyFromBytes(seed[:])
	key, _ := tapsdk.ParsePubKey(publicKey.SerializeCompressed())

	return key
}

func sdkOutpoint(outpoint wire.OutPoint) tapsdk.Outpoint {
	return tapsdk.Outpoint{
		Txid:  outpoint.Hash,
		Index: outpoint.Index,
	}
}

func serializeTx(tx *wire.MsgTx) []byte {
	if tx == nil {
		return nil
	}

	var encoded bytes.Buffer
	_ = tx.Serialize(&encoded)

	return encoded.Bytes()
}

func cloneByteSlices(values [][]byte) [][]byte {
	result := make([][]byte, len(values))
	for idx := range values {
		result[idx] = append([]byte(nil), values[idx]...)
	}

	return result
}
