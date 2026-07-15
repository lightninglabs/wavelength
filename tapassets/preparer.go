// Package tapassets adapts tap-sdk custom-anchor transactions to Wavelength's
// durable out-of-round transfer boundary.
package tapassets

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
	"github.com/lightninglabs/wavelength/oor"
)

const (
	preparationStateVersion = uint16(0)
	attemptCheckpoint       = "checkpoint"
	attemptArk              = "ark"
	opTrueKeyDomain         = "wavelength/taproot-assets-oor/optrue/v0/"
	witnessBackendSigner    = tapsdk.CustomAssetWitnessBackendSigner
	witnessCallerProvided   = tapsdk.CustomAssetWitnessCallerProvided
	scriptExternal          = tapsdk.CustomAssetScriptExternal
)

// ErrReconciliationRequired reports a commit attempt whose durable outcome is
// unknown. Retrying it could create a competing Taproot Asset transition.
var ErrReconciliationRequired = errors.New("taproot asset commit requires " +
	"reconciliation")

// PreparerConfig contains the dependencies of the concrete tap-sdk adapter.
type PreparerConfig struct {
	Wallet *tapsdk.Wallet
	Store  Store
}

// Preparer commits the checkpoint and Ark asset transitions before handing a
// sealed, immutable package to Wavelength's outgoing OOR actor.
type Preparer struct {
	driver    customAnchorDriver
	inventory proofInventoryClient
	store     Store
	mu        sync.Mutex
}

type preparationState struct {
	Version           uint16      `json:"version"`
	RequestDigest     tapsdk.Hash `json:"request_digest"`
	Attempt           string      `json:"attempt,omitempty"`
	CheckpointPackage []byte      `json:"checkpoint_package,omitempty"`
	ArkPackage        []byte      `json:"ark_package,omitempty"`
}

// NewPreparer constructs a production tap-sdk-backed OOR preparer.
func NewPreparer(cfg PreparerConfig) (*Preparer, error) {
	if cfg.Wallet == nil {
		return nil, fmt.Errorf("tap-sdk wallet is required")
	}
	if cfg.Store == nil {
		return nil, fmt.Errorf("taproot asset preparation store is " +
			"required")
	}

	return &Preparer{
		driver: &sdkDriver{
			wallet: cfg.Wallet,
		},
		inventory: cfg.Wallet.Client(),
		store:     cfg.Store,
	}, nil
}

// PrepareTaprootAssetOOR implements oor.TaprootAssetOORPreparer. The first PoC
// intentionally accepts one standard Wavelength input, one two-leaf recipient
// policy, and one isolated asset allocation.
func (p *Preparer) PrepareTaprootAssetOOR(ctx context.Context,
	request *oor.TaprootAssetOORPrepareRequest) (
	*oor.TaprootAssetOORPreparation, error) {

	if p == nil || p.driver == nil || p.inventory == nil || p.store == nil {
		return nil, fmt.Errorf("taproot asset preparer is not " +
			"configured")
	}
	if err := request.Validate(); err != nil {
		return nil, err
	}
	if request.Inputs[0].CustomSpend != nil ||
		len(request.Inputs[0].ExternalSignatures) != 0 {
		return nil, fmt.Errorf("Taproot Asset OOR PoC requires a " +
			"standard VTXO input")
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	digest, err := preparationRequestDigest(request)
	if err != nil {
		return nil, err
	}
	state, err := p.loadState(ctx, request.RequestID, digest)
	if err != nil {
		return nil, err
	}
	if state.Attempt != "" {
		return nil, fmt.Errorf("%w: %s commit for request %q",
			ErrReconciliationRequired, state.Attempt,
			request.RequestID)
	}

	assetRef, err := tapsdk.ParseAssetRef(request.Intent.AssetRef)
	if err != nil {
		return nil, fmt.Errorf("parse Taproot Asset ref: %w", err)
	}
	input := &request.Inputs[0]
	verifier := &proofInventoryVerifier{
		client:    p.inventory,
		assetRef:  assetRef,
		amount:    request.Intent.AssetAmount,
		anchor:    sdkOutpoint(input.VTXO.Outpoint),
		assetRoot: tapsdk.Hash(*input.TaprootAssetRoot),
	}
	// A fully committed journal is self-contained. Restoring it must not
	// depend on tapd availability because no external effect remains.
	if len(state.CheckpointPackage) == 0 {
		verification, err := verifier.VerifyConfirmedProof(
			ctx, request.Intent.ProofFile,
		)
		if err != nil {
			return nil, err
		}
		if verification.PassiveAssetCount != 0 {
			return nil, fmt.Errorf("Taproot Asset OOR PoC "+
				"requires an isolated anchor, found %d "+
				"passive assets",
				verification.PassiveAssetCount)
		}
	}

	checkpoint, checkpointResult, err := p.prepareCheckpoint(
		ctx, request, assetRef, digest, state,
	)
	if err != nil {
		return nil, err
	}

	ark, arkResult, recipients, err := p.prepareArk(
		ctx, request, assetRef, checkpoint, checkpointResult, verifier,
		state,
	)
	if err != nil {
		return nil, err
	}

	checkpointPackage := append(
		[]byte(nil), checkpointResult.packageBytes...,
	)
	prepared := &oor.TaprootAssetOORPreparation{
		PreparedSubmit: &oor.PreparedSubmitPackage{
			ArkPSBT: ark,
			CheckpointPSBTs: []*psbt.Packet{
				checkpoint,
			},
			TaprootAssetTransfer: &oortx.TaprootAssetTransfer{
				Version: oortx.TaprootAssetTransferVersion,
				CheckpointPackages: [][]byte{
					checkpointPackage,
				},
				ArkPackage: append(
					[]byte(nil), arkResult.packageBytes...,
				),
			},
		},
		Recipients: recipients,
	}
	if err := prepared.Validate(request); err != nil {
		return nil, fmt.Errorf("validate prepared Taproot "+
			"Asset OOR: %w", err)
	}

	return prepared, nil
}

// prepareCheckpoint commits the confirmed asset input into the checkpoint
// policy output, using a unique OP_TRUE asset script for the next transition.
func (p *Preparer) prepareCheckpoint(ctx context.Context,
	request *oor.TaprootAssetOORPrepareRequest, assetRef tapsdk.AssetRef,
	digest tapsdk.Hash, state *preparationState) (*psbt.Packet,
	*commitResult, error) {

	input := &request.Inputs[0]
	checkpointInput, err := input.CheckpointInput()
	if err != nil {
		return nil, nil, err
	}
	artifact, err := oortx.BuildCheckpointPSBT(
		request.Policy, checkpointInput,
	)
	if err != nil {
		return nil, nil, err
	}
	spendPath, err := input.EffectiveSpendPath()
	if err != nil {
		return nil, nil, err
	}
	if err := psbtutil.AddTapLeafScript(
		&artifact.PSBT.Inputs[0], spendPath.SpendInfo,
	); err != nil {
		return nil, nil, err
	}

	var committed *commitResult
	if len(state.CheckpointPackage) != 0 {
		committed, err = p.driver.DecodePackage(state.CheckpointPackage)
		if err != nil {
			return nil, nil, fmt.Errorf("restore checkpoint "+
				"package: %w", err)
		}
	} else {
		anchorPlan, err := checkpointAnchorPlan(
			request.Policy, input.OwnerLeafScript,
		)
		if err != nil {
			return nil, nil, err
		}
		anchorBytes, err := psbtutil.Serialize(artifact.PSBT)
		if err != nil {
			return nil, nil, err
		}
		opTrueKey := deterministicKey(digest, attemptCheckpoint)
		opTrueScript := &tapsdk.CustomAssetOPTrueScriptPlan{
			InternalKey: tapsdk.KeyDescriptor{
				RawKeyBytes: opTrueKey,
			},
		}
		sdkRequest := &tapsdk.CustomAnchorRequest{
			Inputs: []tapsdk.CustomAssetInput{{
				ID:       "wavelength-input-0",
				AssetRef: assetRef,
				Amount:   request.Intent.AssetAmount,
				ProofFile: append(
					[]byte(nil),
					request.Intent.ProofFile...,
				),
				Witness: tapsdk.CustomAssetWitnessPlan{
					Mode: witnessBackendSigner,
				},
			}},
			Outputs: []tapsdk.CustomAssetOutput{{
				ID:                "wavelength-checkpoint-0",
				AssetRef:          assetRef,
				Amount:            request.Intent.AssetAmount,
				AnchorOutputIndex: 0,
				AnchorValueSat:    uint64(input.VTXO.Amount),
				Script: tapsdk.CustomAssetScriptPlan{
					Mode:   tapsdk.CustomAssetScriptOPTrue,
					OPTrue: opTrueScript,
				},
				Anchor: anchorPlan,
			}},
			AnchorPSBT: anchorBytes,
			Funding:    callerFundedExact(),
			PassiveAssets: tapsdk.CustomAnchorPassiveAssets{
				Policy: tapsdk.CustomAnchorPassiveReject,
			},
			LossPolicy: tapsdk.CustomAnchorLossPolicy{
				Mode: tapsdk.CustomAnchorLossReject,
			},
			SigningPlans: []tapsdk.CustomAnchorInputSigningPlan{
				scriptSigningPlan(
					0, spendPath.WitnessScript,
					input.VTXO.ClientKey.PubKey,
					input.VTXO.OperatorKey,
				),
			},
		}

		committed, err = p.commit(
			ctx, request.RequestID, state, attemptCheckpoint,
			sdkRequest, nil,
		)
		if err != nil {
			return nil, nil, err
		}
		state.CheckpointPackage = append(
			[]byte(nil), committed.packageBytes...,
		)
		if err := p.storeState(
			ctx, request.RequestID, state,
		); err != nil {
			return nil, nil, fmt.Errorf("persist checkpoint "+
				"package: %w", err)
		}
	}

	checkpoint, err := psbtutil.Parse(committed.anchorPSBT)
	if err != nil {
		return nil, nil, err
	}
	if err := validateCheckpointResult(
		request, assetRef, checkpoint, committed,
	); err != nil {
		return nil, nil, err
	}

	return checkpoint, committed, nil
}

// prepareArk commits the unconfirmed checkpoint proof tip into the final
// Wavelength recipient output.
func (p *Preparer) prepareArk(ctx context.Context,
	request *oor.TaprootAssetOORPrepareRequest, assetRef tapsdk.AssetRef,
	checkpoint *psbt.Packet, checkpointResult *commitResult,
	verifier *proofInventoryVerifier, state *preparationState) (
	*psbt.Packet, *commitResult, []oortx.RecipientOutput, error) {

	checkpointOutput := checkpointResult.outputs[0]
	checkpointTxID := checkpoint.UnsignedTx.TxHash()
	checkpointOut := oortx.CheckpointOutput{
		Txid:   checkpointTxID,
		Output: checkpoint.UnsignedTx.TxOut[0],
	}
	checkpointArtifactInput, err := request.Inputs[0].CheckpointInput()
	if err != nil {
		return nil, nil, nil, err
	}
	artifact, err := oortx.BuildCheckpointPSBT(
		request.Policy, checkpointArtifactInput,
	)
	if err != nil {
		return nil, nil, nil, err
	}
	checkpointOut.TapTreeEncoded = artifact.TapTreeEncoded
	checkpointOut.OwnerLeafScript = artifact.OwnerLeafScript
	checkpointOut.OwnerLeafPolicy = artifact.OwnerLeafPolicy

	recipients := cloneRecipients(request.Recipients)
	arkTemplate, err := oortx.BuildArkPSBT(
		[]oortx.CheckpointOutput{checkpointOut}, recipients,
	)
	if err != nil {
		return nil, nil, nil, err
	}
	leaf, err := oortx.BuildTaprootTapLeafScript(
		artifact.TapTreeEncoded, artifact.OwnerLeafScript,
	)
	if err != nil {
		return nil, nil, nil, err
	}
	leaf, err = composeTapLeaf(leaf, checkpointOutput.taprootAssetRoot)
	if err != nil {
		return nil, nil, nil, err
	}
	arkTemplate.Inputs[0].TaprootLeafScript =
		[]*psbt.TaprootTapLeafScript{leaf}

	var committed *commitResult
	if len(state.ArkPackage) != 0 {
		committed, err = p.driver.DecodePackage(state.ArkPackage)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("restore Ark "+
				"package: %w", err)
		}
	} else {
		anchorPlan, policy, err := recipientAnchorPlan(recipients[0])
		if err != nil {
			return nil, nil, nil, err
		}
		if len(policy.Leaves) != 2 {
			return nil, nil, nil, fmt.Errorf("Taproot Asset OOR " +
				"PoC requires a two-leaf recipient policy")
		}
		anchorBytes, err := psbtutil.Serialize(arkTemplate)
		if err != nil {
			return nil, nil, nil, err
		}
		scriptKey, err := tapsdk.ParseScriptKey(
			request.Intent.RecipientScriptKey,
		)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("parse recipient "+
				"asset script key: %w", err)
		}
		verifier.unconfirmed = &expectedUnconfirmedAnchor{
			previousOutpoint: sdkOutpoint(
				request.Inputs[0].VTXO.Outpoint,
			),
			anchorOutpoint: checkpointOutput.anchorOutpoint,
			transaction:    serializeTx(checkpoint.UnsignedTx),
		}
		transitionProof := append(
			[]byte(nil), checkpointOutput.proofBlob...,
		)
		proofStep := tapsdk.AssetProofPathStep{
			TransitionProof: transitionProof,
		}
		externalScript := &tapsdk.CustomAssetExternalScriptPlan{
			ScriptKey: tapsdk.ScriptKey{
				PubKey: scriptKey,
			},
		}
		proofMetadata := append(
			[]byte(nil), request.Intent.ProofDeliveryMetadata...,
		)
		sdkRequest := &tapsdk.CustomAnchorRequest{
			Inputs: []tapsdk.CustomAssetInput{{
				ID:       "wavelength-checkpoint-0",
				AssetRef: assetRef,
				Amount:   request.Intent.AssetAmount,
				ProofPath: &tapsdk.AssetProofPath{
					Version: tapsdk.AssetProofPathVersionV0,
					ConfirmedBaseProof: append(
						[]byte(nil),
						request.Intent.ProofFile...,
					),
					Steps: []tapsdk.AssetProofPathStep{
						proofStep,
					},
				},
				Witness: tapsdk.CustomAssetWitnessPlan{
					Mode: witnessCallerProvided,
					Stack: cloneByteSlices(
						checkpointOutput.opTrueWitness,
					),
				},
			}},
			Outputs: []tapsdk.CustomAssetOutput{{
				ID:                "wavelength-recipient-0",
				AssetRef:          assetRef,
				Amount:            request.Intent.AssetAmount,
				AnchorOutputIndex: 0,
				AnchorValueSat:    uint64(recipients[0].Value),
				Script: tapsdk.CustomAssetScriptPlan{
					Mode:     scriptExternal,
					External: externalScript,
				},
				Anchor: anchorPlan,
				ProofDelivery: tapsdk.CustomAssetProofDelivery{
					RecipientID: request.RequestID,
					CourierAddress: request.
						Intent.
						ProofCourierAddress,
					OpaqueMetadata: proofMetadata,
				},
			}},
			AnchorPSBT: anchorBytes,
			Funding:    callerFundedExact(),
			PassiveAssets: tapsdk.CustomAnchorPassiveAssets{
				Policy: tapsdk.CustomAnchorPassiveReject,
			},
			LossPolicy: tapsdk.CustomAnchorLossPolicy{
				Mode: tapsdk.CustomAnchorLossReject,
			},
			SigningPlans: []tapsdk.CustomAnchorInputSigningPlan{
				scriptSigningPlan(
					0, artifact.OwnerLeafScript,
					request.Inputs[0].VTXO.ClientKey.PubKey,
					request.Inputs[0].VTXO.OperatorKey,
				),
			},
		}

		committed, err = p.commit(
			ctx, request.RequestID, state, attemptArk, sdkRequest,
			verifier,
		)
		if err != nil {
			return nil, nil, nil, err
		}
		state.ArkPackage = append(
			[]byte(nil), committed.packageBytes...,
		)
		if err := p.storeState(
			ctx, request.RequestID, state,
		); err != nil {
			return nil, nil, nil, fmt.Errorf("persist Ark "+
				"package: %w", err)
		}
	}

	ark, err := psbtutil.Parse(committed.anchorPSBT)
	if err != nil {
		return nil, nil, nil, err
	}
	recipients, err = validateArkResult(
		request, assetRef, ark, checkpointResult, committed,
	)
	if err != nil {
		return nil, nil, nil, err
	}

	return ark, committed, recipients, nil
}

// commit journals the intent before making the external tapd call. A
// response-unknown error deliberately leaves the attempt marker in place.
func (p *Preparer) commit(ctx context.Context, requestID string,
	state *preparationState, attempt string,
	request *tapsdk.CustomAnchorRequest,
	verifier tapsdk.ConfirmedProofVerifier) (*commitResult, error) {

	state.Attempt = attempt
	if err := p.storeState(ctx, requestID, state); err != nil {
		return nil, fmt.Errorf("persist %s commit intent: %w", attempt,
			err)
	}
	result, err := p.driver.Commit(ctx, request, verifier)
	if err != nil {
		if commitOutcomeKnown(err) {
			state.Attempt = ""
			if storeErr := p.storeState(
				ctx, requestID, state,
			); storeErr != nil {
				return nil, fmt.Errorf("%v; clear commit "+
					"intent: %w", err, storeErr)
			}
		}

		return nil, err
	}
	state.Attempt = ""

	return result, nil
}

// loadState restores and validates the durable state for one request.
func (p *Preparer) loadState(ctx context.Context, requestID string,
	digest tapsdk.Hash) (*preparationState, error) {

	encoded, err := p.store.Load(ctx, requestID)
	if errors.Is(err, ErrStoreNotFound) {
		state := &preparationState{
			Version:       preparationStateVersion,
			RequestDigest: digest,
		}
		if err := p.storeState(ctx, requestID, state); err != nil {
			return nil, err
		}

		return state, nil
	}
	if err != nil {
		return nil, err
	}

	var state preparationState
	if err := json.Unmarshal(encoded, &state); err != nil {
		return nil, fmt.Errorf("decode taproot asset preparation "+
			"state: %w", err)
	}
	if state.Version != preparationStateVersion {
		return nil, fmt.Errorf("unsupported taproot asset preparation "+
			"state version %d", state.Version)
	}
	if state.RequestDigest != digest {
		return nil, fmt.Errorf("Taproot Asset OOR idempotency key " +
			"reused with different request")
	}
	if state.Attempt != "" && state.Attempt != attemptCheckpoint &&
		state.Attempt != attemptArk {
		return nil, fmt.Errorf("invalid taproot asset commit "+
			"attempt %q", state.Attempt)
	}

	return &state, nil
}

// storeState atomically persists one preparation state.
func (p *Preparer) storeState(ctx context.Context, requestID string,
	state *preparationState) error {

	encoded, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode taproot asset preparation state: %w",
			err)
	}

	return p.store.Store(ctx, requestID, encoded)
}

// commitOutcomeKnown reports whether retrying after err is known not to repeat
// a successful tapd side effect.
func commitOutcomeKnown(err error) bool {
	var attemptErr *tapsdk.CustomAnchorCommitAttemptError
	if errors.As(err, &attemptErr) {
		return !attemptErr.OutcomeUnknown
	}
	var responseErr *tapsdk.CustomAnchorCommitResponseError
	if errors.As(err, &responseErr) {
		return false
	}
	var localResponseErr *commitResponseError
	if errors.As(err, &localResponseErr) {
		return false
	}

	return true
}

// callerFundedExact returns the funding mode used by fee-less Wavelength OOR
// parent transactions.
func callerFundedExact() tapsdk.CustomAnchorFundingPlan {
	return tapsdk.CustomAnchorFundingPlan{
		Mode:              tapsdk.CustomAnchorFundingCallerFundedExact,
		CallerFundedExact: &tapsdk.CustomAnchorCallerFundedExact{},
	}
}

// checkpointAnchorPlan maps Wavelength's two-leaf checkpoint policy into the
// SDK-owned custom-anchor output DTO.
func checkpointAnchorPlan(policy arkscript.CheckpointPolicy,
	ownerLeaf []byte) (tapsdk.CustomAnchorOutputPlan, error) {

	tree, err := arkscript.CheckpointTapScript(policy, ownerLeaf)
	if err != nil {
		return tapsdk.CustomAnchorOutputPlan{}, err
	}

	return anchorPlan(&arkscript.ARKNUMSKey, tree.Leaves), nil
}

// recipientAnchorPlan decodes the recipient's semantic Wavelength policy and
// maps it into the SDK-owned custom-anchor output DTO.
func recipientAnchorPlan(recipient oortx.RecipientOutput) (
	tapsdk.CustomAnchorOutputPlan, *arkscript.CompiledPolicy, error) {

	template, err := arkscript.DecodePolicyTemplate(
		recipient.VTXOPolicyTemplate,
	)
	if err != nil {
		return tapsdk.CustomAnchorOutputPlan{}, nil, err
	}
	policy, err := template.Compile()
	if err != nil {
		return tapsdk.CustomAnchorOutputPlan{}, nil, err
	}
	leaves := make([]txscript.TapLeaf, len(policy.Leaves))
	for idx := range policy.Leaves {
		leaves[idx] = policy.Leaves[idx].Leaf
	}

	return anchorPlan(policy.InternalKey, leaves), policy, nil
}

// anchorPlan converts an internal key and complete policy leaves to tap-sdk
// primitive DTOs.
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

// scriptSigningPlan binds one anchor input to an exact Wavelength tapscript
// leaf and its required client/operator keys.
func scriptSigningPlan(index uint32, script []byte,
	signers ...*btcec.PublicKey) tapsdk.CustomAnchorInputSigningPlan {

	leafHash := txscript.NewBaseTapLeaf(script).TapHash()
	required := make([]tapsdk.XOnlyPubKey, 0, len(signers))
	for _, signer := range signers {
		if signer == nil {
			continue
		}
		key, _ := tapsdk.ParseXOnlyPubKey(
			schnorr.SerializePubKey(signer),
		)
		required = append(required, key)
	}

	return tapsdk.CustomAnchorInputSigningPlan{
		InputIndex: index,
		ScriptPath: &tapsdk.CustomAnchorScriptPathSigningPlan{
			LeafHash:        tapsdk.Hash(leafHash),
			RequiredSigners: required,
		},
	}
}

// deterministicKey derives a public, unique OP_TRUE internal key from the
// request digest. No secret is required because the asset spend is script path.
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

// composeTapLeaf extends a policy-only control block with the Taproot Asset
// root as its final sibling and recalculates the output-key parity bit.
func composeTapLeaf(leaf *psbt.TaprootTapLeafScript,
	assetRoot tapsdk.Hash) (*psbt.TaprootTapLeafScript, error) {

	if leaf == nil {
		return nil, fmt.Errorf("checkpoint tap leaf is required")
	}
	controlBlock, err := txscript.ParseControlBlock(leaf.ControlBlock)
	if err != nil {
		return nil, fmt.Errorf("parse checkpoint control block: %w",
			err)
	}
	policyRoot := controlBlock.RootHash(leaf.Script)
	combined := tapBranchHash(policyRoot, assetRoot[:])
	outputKey := txscript.ComputeTaprootOutputKey(
		controlBlock.InternalKey, combined[:],
	)
	outputKeyIsOdd := outputKey.SerializeCompressed()[0] == 0x03
	controlBlock.OutputKeyYIsOdd = outputKeyIsOdd
	controlBlock.InclusionProof = append(
		append(
			[]byte(nil), controlBlock.InclusionProof...,
		),
		assetRoot[:]...,
	)
	encoded, err := controlBlock.ToBytes()
	if err != nil {
		return nil, fmt.Errorf("encode composed checkpoint control "+
			"block: %w", err)
	}

	return &psbt.TaprootTapLeafScript{
		ControlBlock: encoded,
		Script:       append([]byte(nil), leaf.Script...),
		LeafVersion:  leaf.LeafVersion,
	}, nil
}

// tapBranchHash computes the BIP-341 branch hash of two child roots.
func tapBranchHash(left, right []byte) chainhash.Hash {
	if bytes.Compare(left, right) > 0 {
		left, right = right, left
	}

	return *chainhash.TaggedHash(chainhash.TagTapBranch, left, right)
}

// validateCheckpointResult binds the first SDK package to the Wavelength
// input and committed checkpoint output.
func validateCheckpointResult(request *oor.TaprootAssetOORPrepareRequest,
	assetRef tapsdk.AssetRef, checkpoint *psbt.Packet,
	result *commitResult) error {

	if checkpoint == nil || checkpoint.UnsignedTx == nil {
		return fmt.Errorf("committed checkpoint PSBT is required")
	}
	if len(result.inputs) != 1 || len(result.outputs) != 1 {
		return fmt.Errorf("checkpoint package must contain one asset " +
			"input and output")
	}
	input := result.inputs[0]
	output := result.outputs[0]
	expectedInput := sdkOutpoint(request.Inputs[0].VTXO.Outpoint)
	if input.anchorOutpoint != expectedInput ||
		!input.assetRef.Equivalent(assetRef) ||
		input.amount != request.Intent.AssetAmount {
		return fmt.Errorf("checkpoint package asset input mismatch")
	}
	if output.anchorOutputIndex != 0 ||
		output.anchorOutpoint != sdkOutpoint(wire.OutPoint{
			Hash: checkpoint.UnsignedTx.TxHash(), Index: 0,
		}) || !output.assetRef.Equivalent(assetRef) ||
		output.amount != request.Intent.AssetAmount ||
		output.anchorValueSat != int64(request.Inputs[0].VTXO.Amount) ||
		len(output.opTrueWitness) == 0 || len(output.proofBlob) == 0 {
		return fmt.Errorf("checkpoint package asset output mismatch")
	}
	if len(checkpoint.UnsignedTx.TxOut) < 2 {
		return fmt.Errorf("committed checkpoint outputs are incomplete")
	}
	tree, err := arkscript.CheckpointTapScript(
		request.Policy, request.Inputs[0].OwnerLeafScript,
	)
	if err != nil {
		return err
	}
	if err := validateOutputCommitment(
		checkpoint.UnsignedTx.TxOut[0], &arkscript.ARKNUMSKey,
		tree.RootHash, output,
	); err != nil {
		return fmt.Errorf("checkpoint output: %w", err)
	}

	return nil
}

// validateArkResult binds the second SDK package to the committed checkpoint
// and returns the composed recipient metadata.
func validateArkResult(request *oor.TaprootAssetOORPrepareRequest,
	assetRef tapsdk.AssetRef, ark *psbt.Packet, checkpoint,
	result *commitResult) ([]oortx.RecipientOutput, error) {

	if ark == nil || ark.UnsignedTx == nil {
		return nil, fmt.Errorf("committed Ark PSBT is required")
	}
	if len(result.inputs) != 1 || len(result.outputs) != 1 {
		return nil, fmt.Errorf("Ark package must contain one asset " +
			"input and output")
	}
	input := result.inputs[0]
	output := result.outputs[0]
	if input.anchorOutpoint != checkpoint.outputs[0].anchorOutpoint ||
		!input.assetRef.Equivalent(assetRef) ||
		input.amount != request.Intent.AssetAmount {
		return nil, fmt.Errorf("Ark package asset input mismatch")
	}
	if output.anchorOutputIndex != 0 ||
		output.anchorOutpoint != sdkOutpoint(wire.OutPoint{
			Hash: ark.UnsignedTx.TxHash(), Index: 0,
		}) || !output.assetRef.Equivalent(assetRef) ||
		output.amount != request.Intent.AssetAmount ||
		output.anchorValueSat != int64(request.Recipients[0].Value) ||
		len(output.proofBlob) == 0 {
		return nil, fmt.Errorf("Ark package asset output mismatch")
	}

	recipients := cloneRecipients(request.Recipients)
	root := chainhash.Hash(output.taprootAssetRoot)
	recipients[0].TaprootAssetRoot = &root
	template, err := arkscript.DecodePolicyTemplate(
		recipients[0].VTXOPolicyTemplate,
	)
	if err != nil {
		return nil, err
	}
	policy, err := template.Compile()
	if err != nil {
		return nil, err
	}
	composed, err := arkscript.ComposeWithSiblingRoot(policy, root)
	if err != nil {
		return nil, err
	}
	recipients[0].PkScript, err = txscript.PayToTaprootScript(
		composed.OutputKey(),
	)
	if err != nil {
		return nil, err
	}
	if err := validateOutputCommitment(
		ark.UnsignedTx.TxOut[0], policy.InternalKey, policy.RootHash,
		output,
	); err != nil {
		return nil, fmt.Errorf("Ark recipient output: %w", err)
	}

	return recipients, nil
}

// validateOutputCommitment checks both SDK root hints and the actual P2TR
// output key against the Wavelength policy root.
func validateOutputCommitment(txOut *wire.TxOut, internalKey *btcec.PublicKey,
	policyRoot []byte, output commitOutput) error {

	if txOut == nil {
		return fmt.Errorf("transaction output is required")
	}
	combined := tapBranchHash(policyRoot, output.taprootAssetRoot[:])
	if tapsdk.Hash(combined) != output.taprootMerkleRoot {
		return fmt.Errorf("taproot merkle root mismatch")
	}
	outputKey := txscript.ComputeTaprootOutputKey(internalKey, combined[:])
	wantScript, err := txscript.PayToTaprootScript(outputKey)
	if err != nil {
		return err
	}
	if !bytes.Equal(txOut.PkScript, wantScript) {
		return fmt.Errorf("P2TR output key mismatch")
	}

	return nil
}

// preparationRequestDigest binds an idempotency key to all supported PoC
// request fields without relying on unstable Go struct encodings.
func preparationRequestDigest(request *oor.TaprootAssetOORPrepareRequest) (
	tapsdk.Hash, error) {

	var value bytes.Buffer
	writeDigestBytes(
		&value, request.Policy.OperatorKey.SerializeCompressed(),
	)
	_ = binary.Write(&value, binary.BigEndian, request.Policy.CSVDelay)
	input := request.Inputs[0]
	writeDigestBytes(&value, input.VTXO.Outpoint.Hash[:])
	_ = binary.Write(&value, binary.BigEndian, input.VTXO.Outpoint.Index)
	_ = binary.Write(&value, binary.BigEndian, uint64(input.VTXO.Amount))
	writeDigestBytes(&value, input.VTXO.PkScript)
	writeDigestBytes(&value, input.VTXOPolicyTemplate)
	writeDigestBytes(&value, input.OwnerLeafScript)
	writeDigestBytes(&value, input.OwnerLeafPolicy)
	writeDigestBytes(&value, input.TaprootAssetRoot[:])
	recipient := request.Recipients[0]
	_ = binary.Write(&value, binary.BigEndian, uint64(recipient.Value))
	writeDigestBytes(&value, recipient.PkScript)
	writeDigestBytes(&value, recipient.VTXOPolicyTemplate)
	writeDigestBytes(&value, []byte(request.Intent.AssetRef))
	_ = binary.Write(&value, binary.BigEndian, request.Intent.AssetAmount)
	writeDigestBytes(&value, request.Intent.ProofFile)
	writeDigestBytes(&value, request.Intent.RecipientScriptKey)
	writeDigestBytes(&value, []byte(request.Intent.ProofCourierAddress))
	writeDigestBytes(&value, request.Intent.ProofDeliveryMetadata)
	digest := sha256.Sum256(value.Bytes())

	return tapsdk.Hash(digest), nil
}

// writeDigestBytes writes one unambiguous length-prefixed digest field.
func writeDigestBytes(buffer *bytes.Buffer, value []byte) {
	_ = binary.Write(buffer, binary.BigEndian, uint64(len(value)))
	_, _ = buffer.Write(value)
}

// sdkOutpoint converts the shared btcd outpoint into the SDK-owned DTO.
func sdkOutpoint(outpoint wire.OutPoint) tapsdk.Outpoint {
	return tapsdk.Outpoint{
		Txid:  outpoint.Hash,
		Index: outpoint.Index,
	}
}

// serializeTx returns the canonical wire encoding of a transaction.
func serializeTx(tx *wire.MsgTx) []byte {
	if tx == nil {
		return nil
	}
	var encoded bytes.Buffer
	_ = tx.Serialize(&encoded)

	return encoded.Bytes()
}

// cloneRecipients deep-copies mutable recipient fields.
func cloneRecipients(values []oortx.RecipientOutput) []oortx.RecipientOutput {
	result := make([]oortx.RecipientOutput, len(values))
	for idx := range values {
		result[idx] = values[idx]
		result[idx].PkScript = append(
			[]byte(nil), values[idx].PkScript...,
		)
		result[idx].VTXOPolicyTemplate = append(
			[]byte(nil), values[idx].VTXOPolicyTemplate...,
		)
		if values[idx].TaprootAssetRoot != nil {
			root := *values[idx].TaprootAssetRoot
			result[idx].TaprootAssetRoot = &root
		}
	}

	return result
}

// cloneByteSlices deep-copies a witness stack.
func cloneByteSlices(values [][]byte) [][]byte {
	result := make([][]byte, len(values))
	for idx := range values {
		result[idx] = append([]byte(nil), values[idx]...)
	}

	return result
}
