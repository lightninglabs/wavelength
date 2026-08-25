package tapassets

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
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
)

type expectedUnconfirmedAnchor struct {
	previousOutpoint  tapsdk.Outpoint
	previousOutpoints []tapsdk.Outpoint
	anchorOutpoint    tapsdk.Outpoint
	transaction       []byte
}

type assetSpendSource struct {
	proofFile      []byte
	proofPath      *tapsdk.AssetProofPath
	witness        tapsdk.CustomAssetWitnessPlan
	verifier       tapsdk.ConfirmedProofVerifier
	amount         uint64
	issuanceID     tapsdk.AssetID
	issuanceKnown  bool
	anchorOutpoint tapsdk.Outpoint
}

type assetSourceVerifier struct {
	sources []*assetSpendSource
}

func (v *assetSourceVerifier) VerifyConfirmedProof(ctx context.Context,
	proofFile []byte) (*tapsdk.ConfirmedProofVerification, error) {

	var errs []error
	for _, source := range v.sources {
		verification, err := source.verifier.VerifyConfirmedProof(
			ctx, proofFile,
		)
		if err == nil {
			return verification, nil
		}
		errs = append(errs, err)
	}

	return nil, fmt.Errorf("asset proof was not accepted: %w",
		errors.Join(errs...))
}

func (v *assetSourceVerifier) VerifyUnconfirmedAnchor(ctx context.Context,
	transition tapsdk.UnconfirmedAnchorVerification) error {

	var errs []error
	for _, source := range v.sources {
		verifier, ok :=
			source.verifier.(tapsdk.UnconfirmedAnchorVerifier)
		if !ok {
			continue
		}
		if err := verifier.VerifyUnconfirmedAnchor(
			ctx, transition,
		); err == nil {
			return nil
		} else {
			errs = append(errs, err)
		}
	}

	return fmt.Errorf("unconfirmed anchor was not accepted: %w",
		errors.Join(errs...))
}

func (s *assetSpendSource) customInput(id string, assetRef tapsdk.AssetRef) (
	tapsdk.CustomAssetInput, error) {

	if s == nil {
		return tapsdk.CustomAssetInput{}, fmt.Errorf("asset proof " +
			"source is required")
	}

	input := tapsdk.CustomAssetInput{
		ID:       id,
		AssetRef: assetRef,
		Amount:   s.amount,
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

func (s *assetSpendSource) appendTransition(output commitOutput,
	anchorTx []byte) (*assetSpendSource, error) {

	if s == nil || len(output.proofBlob) == 0 {
		return nil, fmt.Errorf("asset transition proof is required")
	}

	var path *tapsdk.AssetProofPath
	if s.proofPath != nil {
		path = s.proofPath.Clone()
	} else {
		path = &tapsdk.AssetProofPath{
			ConfirmedBaseProof: append(
				[]byte(nil), s.proofFile...,
			),
		}
	}

	if len(path.Steps) >= tapsdk.AssetProofPathMaxDepth {
		return nil, fmt.Errorf("asset proof path has no remaining " +
			"transition slot")
	}
	baseDepth := uint16(len(path.Steps))
	path.Steps = append(path.Steps, tapsdk.AssetProofPathStep{
		TransitionProof: append([]byte(nil), output.proofBlob...),
	})

	expected := &expectedUnconfirmedAnchor{
		previousOutpoint: s.anchorOutpoint,
		previousOutpoints: []tapsdk.Outpoint{
			s.anchorOutpoint,
		},
		anchorOutpoint: output.anchorOutpoint,
		transaction:    append([]byte(nil), anchorTx...),
	}

	return &assetSpendSource{
		proofPath: path,
		witness: tapsdk.CustomAssetWitnessPlan{
			Mode:  witnessCallerProvided,
			Stack: cloneByteSlices(output.opTrueWitness),
		},
		verifier: &treePathVerifier{
			base:      s.verifier,
			baseDepth: baseDepth,
			steps: []*expectedUnconfirmedAnchor{
				expected,
			},
		},
		amount:         output.amount,
		issuanceID:     output.issuanceID,
		issuanceKnown:  true,
		anchorOutpoint: output.anchorOutpoint,
	}, nil
}

func validateTreeProofCapacity(root TreeRootAssetSource,
	additionalSteps int) error {

	if additionalSteps <= 0 {
		return fmt.Errorf("asset tree depth must be positive")
	}

	for idx := range root.Inputs {
		input := root.Inputs[idx]
		var currentDepth, currentSize int
		if len(input.ProofPath) != 0 {
			var path tapsdk.AssetProofPath
			if err := path.UnmarshalBinary(
				input.ProofPath,
			); err != nil {
				return fmt.Errorf("root asset input %d proof "+
					"path: %w", idx, err)
			}
			currentDepth = len(path.Steps)
			currentSize = len(input.ProofPath)
		} else {
			maxSize := tapsdk.AssetProofPathMaxConfirmedProofSize
			if len(input.ProofFile) > maxSize {
				return fmt.Errorf("root asset input %d proof "+
					"exceeds %d bytes", idx, maxSize)
			}
			currentSize = len(input.ProofFile)
		}

		if currentDepth+
			additionalSteps > tapsdk.AssetProofPathMaxDepth {
			return fmt.Errorf("root asset input %d proof depth %d "+
				"cannot add %d tree transitions", idx,
				currentDepth, additionalSteps)
		}

		stepBudget := uint64(additionalSteps) *
			uint64(4+tapsdk.AssetProofPathMaxStepSize)
		if uint64(currentSize)+
			stepBudget > tapsdk.AssetProofPathMaxSize {
			return fmt.Errorf("root asset input %d proof lacks "+
				"space for %d tree transitions", idx,
				additionalSteps)
		}
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
