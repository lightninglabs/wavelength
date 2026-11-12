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
	"github.com/lightninglabs/tap-sdk/customanchor"
	"github.com/lightninglabs/wavelength/lib/arkscript"
)

const (
	opTrueKeyDomain       = "wavelength/assets/optrue/v0/"
	witnessBackendSigner  = customanchor.AssetWitnessBackendSigner
	witnessCallerProvided = customanchor.AssetWitnessCallerProvided
	scriptExternal        = customanchor.AssetScriptExternal
)

type expectedUnconfirmedAnchor struct {
	previousOutpoint  tapsdk.Outpoint
	previousOutpoints []tapsdk.Outpoint
	anchorOutpoint    tapsdk.Outpoint
	transaction       []byte
}

type assetSpendSource struct {
	proofFile      []byte
	proofPath      *customanchor.ProofPath
	witness        customanchor.AssetWitnessPlan
	verifier       customanchor.ConfirmedProofVerifier
	amount         uint64
	issuanceID     tapsdk.AssetID
	issuanceKnown  bool
	anchorOutpoint tapsdk.Outpoint
}

type assetSourceVerifier struct {
	sources []*assetSpendSource
}

func (v *assetSourceVerifier) VerifyConfirmedProof(ctx context.Context,
	proofFile []byte) (*customanchor.ConfirmedProofVerification, error) {

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
	transition customanchor.UnconfirmedAnchorVerification) error {

	var errs []error
	for _, source := range v.sources {
		verifier, ok :=
			source.verifier.(customanchor.UnconfirmedAnchorVerifier)
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
	customanchor.AssetInput, error) {

	if s == nil {
		return customanchor.AssetInput{}, fmt.Errorf("asset proof " +
			"source is required")
	}

	input := customanchor.AssetInput{
		ID:       id,
		AssetRef: assetRef,
		Amount:   s.amount,
		Witness: customanchor.AssetWitnessPlan{
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
		return customanchor.AssetInput{}, fmt.Errorf("asset proof " +
			"source is empty")
	}

	return input, nil
}

func (s *assetSpendSource) appendTransition(output commitOutput,
	anchorTx []byte) (*assetSpendSource, error) {

	if s == nil || len(output.proofBlob) == 0 {
		return nil, fmt.Errorf("asset transition proof is required")
	}

	var path *customanchor.ProofPath
	if s.proofPath != nil {
		path = s.proofPath.Clone()
	} else {
		path = &customanchor.ProofPath{
			ConfirmedBaseProof: append(
				[]byte(nil), s.proofFile...,
			),
		}
	}

	if len(path.Steps) >= customanchor.ProofPathMaxDepth {
		return nil, fmt.Errorf("asset proof path has no remaining " +
			"transition slot")
	}
	baseDepth := uint16(len(path.Steps))
	path.Steps = append(path.Steps, customanchor.ProofPathStep{
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
		witness: customanchor.AssetWitnessPlan{
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
			var path customanchor.ProofPath
			if err := path.UnmarshalBinary(
				input.ProofPath,
			); err != nil {
				return fmt.Errorf("root asset input %d proof "+
					"path: %w", idx, err)
			}
			currentDepth = len(path.Steps)
			currentSize = len(input.ProofPath)
		} else {
			maxSize := customanchor.ProofPathMaxConfirmedProofSize
			if len(input.ProofFile) > maxSize {
				return fmt.Errorf("root asset input %d proof "+
					"exceeds %d bytes", idx, maxSize)
			}
			currentSize = len(input.ProofFile)
		}

		if currentDepth+
			additionalSteps > customanchor.ProofPathMaxDepth {
			return fmt.Errorf("root asset input %d proof depth %d "+
				"cannot add %d tree transitions", idx,
				currentDepth, additionalSteps)
		}

		stepBudget := uint64(additionalSteps) *
			uint64(4+customanchor.ProofPathMaxStepSize)
		if uint64(currentSize)+
			stepBudget > customanchor.ProofPathMaxSize {
			return fmt.Errorf("root asset input %d proof lacks "+
				"space for %d tree transitions", idx,
				additionalSteps)
		}
	}

	return nil
}

func callerFundedExact() customanchor.FundingPlan {
	return customanchor.FundingPlan{
		Mode:              customanchor.FundingCallerFundedExact,
		CallerFundedExact: &customanchor.CallerFundedExact{},
	}
}

// anchorPlan describes a composed anchor output for the given policy leaves.
// tap-sdk assembles TapLeaves by pairing adjacent leaves, while arkscript
// splits a policy's leaves in halves. Leaves are accepted only where both
// layouts give the same root, so the composed output stays spendable through
// the policy's control blocks.
func anchorPlan(internalKey *btcec.PublicKey,
	leaves []txscript.TapLeaf) (customanchor.OutputPlan, error) {

	if len(leaves) > 1 {
		policyLeaves := make([]arkscript.PolicyLeaf, len(leaves))
		for idx := range leaves {
			policyLeaves[idx] = arkscript.PolicyLeaf{
				Leaf: leaves[idx],
			}
		}
		policy, err := arkscript.BuildTree(
			policyLeaves, &arkscript.ARKNUMSKey,
		)
		if err != nil {
			return customanchor.OutputPlan{}, fmt.Errorf("build "+
				"anchor policy tree: %w", err)
		}

		assembled := txscript.AssembleTaprootScriptTree(leaves...)
		root := assembled.RootNode.TapHash()
		if !bytes.Equal(root[:], policy.RootHash) {
			return customanchor.OutputPlan{}, fmt.Errorf("%d "+
				"anchor leaves do not assemble into their "+
				"policy tree", len(leaves))
		}
	}

	pubKey, _ := tapsdk.ParsePubKey(internalKey.SerializeCompressed())
	sdkLeaves := make([]tapsdk.TapLeaf, len(leaves))
	for idx := range leaves {
		sdkLeaves[idx] = tapsdk.TapLeaf{
			Script: append([]byte(nil), leaves[idx].Script...),
		}
	}

	return customanchor.OutputPlan{
		InternalKey: tapsdk.InternalKey{
			PubKey: pubKey,
		},
		Tapscript: customanchor.TapscriptPlan{
			TapLeaves: sdkLeaves,
		},
	}, nil
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
