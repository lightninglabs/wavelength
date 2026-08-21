package tapassets

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightningnetwork/lnd/keychain"
)

const (
	onboardingStateVersion  = uint16(0)
	onboardingAttemptCommit = "commit"
	onboardingStorePrefix   = "onboarding/"
	onboardingLockIDDomain  = "wavelength/taproot-assets/onboarding-lock/v1"
	onboardingDustFloorSat  = uint64(330)

	// onboardingChangeValueSat is the Bitcoin value carried by the asset
	// change anchor output. The change is ordinary tapd wallet inventory,
	// so it takes tapd's own asset-anchor value rather than the
	// operator-mandated carrier value of the boarded output.
	onboardingChangeValueSat = int64(1_000)
)

// Logical identifiers naming the transition's asset outputs across the
// commit and the sealed package. The boarding identifier is part of the
// single-output transition's committed shape and must not change.
const (
	onboardingOutputID = "wavelength-onboarding-output-0"
	onboardingChangeID = "wavelength-onboarding-change"
)

// onboardingChangeOutputIndex is the asset change anchor output's position.
// The boarded output holds index 0 and wallet funding appends its Bitcoin
// change after both asset outputs, so the position is fixed at build time.
const onboardingChangeOutputIndex = uint32(1)

// OnboardingRequest selects the complete Taproot Asset proofs funding one
// boarding and the standard Wavelength policy that will own its new
// on-chain anchor. The selected proofs must cover AssetAmount; any surplus
// returns to the daemon's own tapd wallet as asset change.
type OnboardingRequest struct {
	RequestID   string
	AssetRef    string
	AssetAmount uint64

	// ProofFile is one complete confirmed proof file. Exactly one of
	// ProofFile and ProofFiles must be set.
	ProofFile []byte

	// ProofFiles are the complete confirmed proof files of every funding
	// UTXO the transition spends.
	ProofFiles [][]byte

	CarrierValueSat    uint64
	FeeRateSatPerVByte uint64
	TargetConf         uint32
	MaxFeeSat          uint64
	OperatorKey        *btcec.PublicKey
	ExitDelay          uint32
}

// onboardingProofFiles returns the request's funding proofs with the
// singular field folded in, so every caller sees one shape.
func onboardingProofFiles(request *OnboardingRequest) [][]byte {
	if request == nil {
		return nil
	}
	if len(request.ProofFiles) != 0 {
		return request.ProofFiles
	}
	if len(request.ProofFile) != 0 {
		return [][]byte{request.ProofFile}
	}

	return nil
}

// OnboardingKeyDeriver returns the next wallet-owned standard VTXO key.
type OnboardingKeyDeriver func(context.Context) (*keychain.KeyDescriptor, error)

// OnboardingResult contains the local descriptor material for the admitted
// direct-on-chain VTXO. The final asset proof remains managed by tapd.
type OnboardingResult struct {
	Outpoint         wire.OutPoint
	ValueSat         int64
	AssetRef         string
	AssetAmount      uint64
	ActualFeeSat     uint64
	PolicyTemplate   []byte
	PkScript         []byte
	TaprootAssetRoot chainhash.Hash
	OwnerKey         keychain.KeyDescriptor
	OperatorKey      *btcec.PublicKey
	ExitDelay        uint32

	// Digest scopes the output's deterministic asset script key.
	Digest tapsdk.Hash

	// ScriptKey is the committed asset script key. An OP_TRUE boarding
	// output is absent from tapd's wallet inventory, so this is the
	// only handle for exporting its proof.
	ScriptKey tapsdk.PubKey

	// OPTrueWitness is the asset-level OP_TRUE witness stack a round's
	// commitment transition spends the boarded output with.
	OPTrueWitness [][]byte
}

type onboardingDriver interface {
	CommitOnboarding(context.Context, *tapsdk.CustomAnchorRequest,
		tapsdk.ConfirmedProofVerifier) (*commitResult, error)

	DecodePackage([]byte) (*commitResult, error)

	VerifyFinalOnboarding([]byte, []byte) error

	PublishOnboarding(context.Context, []byte, []byte) error
}

// OnboarderConfig contains the external boundaries of the durable workflow.
type OnboarderConfig struct {
	Wallet         *tapsdk.Wallet
	Store          Store
	Signer         tapsdk.AnchorSigner
	DeriveOwnerKey OnboardingKeyDeriver
}

// Onboarder moves a tapd-managed confirmed asset anchor into a standard
// Wavelength policy and registers the resulting direct-on-chain VTXO.
type Onboarder struct {
	driver         onboardingDriver
	inventory      proofInventoryClient
	keys           onboardingKeyClient
	store          Store
	signer         tapsdk.AnchorSigner
	deriveOwnerKey OnboardingKeyDeriver
	mu             sync.Mutex
}

// onboardingKeyClient derives the tapd wallet keys owning asset change.
type onboardingKeyClient interface {
	DeriveScriptKey(context.Context) (*tapsdk.ScriptKey, error)

	DeriveInternalKey(context.Context) (*tapsdk.InternalKey, error)
}

// onboardingChange is the pinned asset change of one onboarding: the units
// the selected funding UTXOs carry beyond the boarded amount, returned to
// the daemon's own tapd wallet on its own anchor output. Both keys are
// derived once and persisted, because the split commitment binds the change
// script key and a replay must rebuild the identical transition.
type onboardingChange struct {
	Amount            uint64             `json:"amount"`
	OutputValueSat    int64              `json:"output_value_sat"`
	ScriptKey         tapsdk.ScriptKey   `json:"script_key"`
	AnchorInternalKey tapsdk.InternalKey `json:"anchor_internal_key"`
}

type onboardingState struct {
	Version         uint16            `json:"version"`
	RequestDigest   tapsdk.Hash       `json:"request_digest"`
	Attempt         string            `json:"attempt,omitempty"`
	OwnerPubKey     []byte            `json:"owner_pub_key"`
	OwnerKeyFamily  int32             `json:"owner_key_family"`
	OwnerKeyIndex   uint32            `json:"owner_key_index"`
	PolicyTemplate  []byte            `json:"policy_template"`
	Change          *onboardingChange `json:"change,omitempty"`
	TransferPackage []byte            `json:"transfer_package,omitempty"`
	FinalAnchorPSBT []byte            `json:"final_anchor_psbt,omitempty"`
	Published       bool              `json:"published"`
}

// NewOnboarder constructs the tap-sdk-backed onboarding workflow.
func NewOnboarder(cfg OnboarderConfig) (*Onboarder, error) {
	if cfg.Wallet == nil {
		return nil, fmt.Errorf("tap-sdk wallet is required")
	}
	if cfg.Store == nil {
		return nil, fmt.Errorf("taproot asset onboarding store is " +
			"required")
	}
	if cfg.Signer == nil {
		return nil, fmt.Errorf("taproot asset anchor signer is " +
			"required")
	}
	if cfg.DeriveOwnerKey == nil {
		return nil, fmt.Errorf("taproot asset owner key deriver is " +
			"required")
	}

	return &Onboarder{
		driver: &sdkDriver{
			wallet: cfg.Wallet,
		},
		inventory:      cfg.Wallet.Client(),
		keys:           cfg.Wallet.Client(),
		store:          cfg.Store,
		signer:         cfg.Signer,
		deriveOwnerKey: cfg.DeriveOwnerKey,
	}, nil
}

// Onboard performs or resumes one idempotent onboarding request. Once the
// commit succeeds, every retry reuses the exact package and final PSBT bytes.
func (o *Onboarder) Onboard(ctx context.Context, request *OnboardingRequest) (
	*OnboardingResult, error) {

	if o == nil || o.driver == nil || o.inventory == nil || o.keys == nil ||
		o.store == nil ||
		o.signer == nil || o.deriveOwnerKey == nil {
		return nil, fmt.Errorf("taproot asset onboarder is not " +
			"configured")
	}
	if err := validateOnboardingRequest(request); err != nil {
		return nil, err
	}

	o.mu.Lock()
	defer o.mu.Unlock()

	digest := onboardingRequestDigest(request)
	state, err := o.loadState(ctx, request, digest)
	if err != nil {
		return nil, err
	}
	if state.Attempt != "" {
		return nil, fmt.Errorf("%w: onboarding %s for request %q",
			ErrReconciliationRequired, state.Attempt,
			request.RequestID)
	}

	ownerKey, err := ownerKeyFromState(state)
	if err != nil {
		return nil, err
	}
	policy, err := arkscript.NewVTXOPolicy(
		ownerKey.PubKey, request.OperatorKey, request.ExitDelay,
	)
	if err != nil {
		return nil, fmt.Errorf("build onboarding VTXO policy: %w", err)
	}
	if len(state.PolicyTemplate) == 0 {
		state.PolicyTemplate, err = policy.Template.Encode()
		if err != nil {
			return nil, fmt.Errorf("encode onboarding VTXO "+
				"policy: %w", err)
		}
		if err := o.storeState(
			ctx, request.RequestID, state,
		); err != nil {
			return nil, err
		}
	} else {
		expected, encodeErr := policy.Template.Encode()
		if encodeErr != nil {
			return nil, encodeErr
		}
		if !bytes.Equal(state.PolicyTemplate, expected) {
			return nil, fmt.Errorf("stored onboarding policy " +
				"mismatch")
		}
	}

	committed, err := o.commit(ctx, request, policy, state)
	if err != nil {
		return nil, err
	}
	result, err := onboardingResultFromCommit(
		request, state, ownerKey, policy, digest, committed,
	)
	if err != nil {
		return nil, err
	}

	if len(state.FinalAnchorPSBT) == 0 {
		state.FinalAnchorPSBT, err = o.signer(ctx, committed.anchorPSBT)
		if err != nil {
			return nil, fmt.Errorf("sign onboarding anchor "+
				"PSBT: %w", err)
		}
		if err := o.driver.VerifyFinalOnboarding(
			state.TransferPackage, state.FinalAnchorPSBT,
		); err != nil {
			return nil, err
		}
		if err := o.storeState(
			ctx, request.RequestID, state,
		); err != nil {
			return nil, err
		}
	} else if err := o.driver.VerifyFinalOnboarding(
		state.TransferPackage, state.FinalAnchorPSBT,
	); err != nil {
		return nil, fmt.Errorf("restore final onboarding PSBT: %w", err)
	}

	if !state.Published {
		if err := o.driver.PublishOnboarding(
			ctx, state.TransferPackage, state.FinalAnchorPSBT,
		); err != nil {
			return nil, err
		}
		state.Published = true
		if err := o.storeState(
			ctx, request.RequestID, state,
		); err != nil {
			return nil, err
		}
	}

	return result, nil
}

func (o *Onboarder) commit(ctx context.Context, request *OnboardingRequest,
	policy *arkscript.VTXOPolicy, state *onboardingState) (*commitResult,
	error) {

	if len(state.TransferPackage) != 0 {
		committed, err := o.driver.DecodePackage(state.TransferPackage)
		if err != nil {
			return nil, fmt.Errorf("restore onboarding package: %w",
				err)
		}

		return committed, nil
	}

	assetRef, assetInputs, verifier, err := o.verifyInputs(ctx, request)
	if err != nil {
		return nil, err
	}
	fee, err := onboardingAnchorFee(request)
	if err != nil {
		return nil, err
	}
	outputValue := int64(request.CarrierValueSat)
	if outputValue < int64(onboardingDustFloorSat) {
		return nil, fmt.Errorf("onboarding output value %d is below "+
			"the Taproot dust floor", outputValue)
	}

	// The funding surplus is whatever the selected anchors carry beyond
	// the boarded amount. Its keys are derived once and pinned, because
	// the split commitment binds the change script key and every later
	// rebuild must reproduce the same transition.
	if err := o.pinChange(
		ctx, request, state, assetInputs,
	); err != nil {
		return nil, err
	}

	inputs := make([]tapsdk.CustomAssetInput, 0, len(assetInputs))
	plans := make(
		[]tapsdk.CustomAnchorInputSigningPlan, 0, len(assetInputs),
	)
	anchorInputs := make([]tapsdk.Outpoint, 0, len(assetInputs))
	for idx := range assetInputs {
		input := &assetInputs[idx]
		inputs = append(inputs, tapsdk.CustomAssetInput{
			ID: fmt.Sprintf(
				"wavelength-onboarding-input-%d", idx,
			),
			AssetRef:  assetRef,
			Amount:    input.amount,
			ProofFile: append([]byte(nil), input.proofFile...),
			Witness: tapsdk.CustomAssetWitnessPlan{
				Mode: tapsdk.CustomAssetWitnessBackendSigner,
			},
		})

		// Each funding anchor is a tapd key spend under its own
		// internal key; wallet funding appends its own inputs after
		// these and the backend manages them itself.
		signer, err := onboardingAnchorSigner(input.anchor)
		if err != nil {
			return nil, err
		}
		plans = append(plans, tapsdk.CustomAnchorInputSigningPlan{
			InputIndex: uint32(idx),
			KeyPath: &tapsdk.CustomAnchorKeyPathSigningPlan{
				Signer: signer,
			},
		})
		anchorInputs = append(anchorInputs, input.anchor.OutPoint)
	}

	requestDigest := onboardingRequestDigest(request)
	outputs := []tapsdk.CustomAssetOutput{{
		ID:                onboardingOutputID,
		AssetRef:          assetRef,
		Amount:            request.AssetAmount,
		AnchorOutputIndex: 0,
		AnchorValueSat:    uint64(outputValue),
		Script:            boardingScriptPlan(requestDigest),
		Anchor: anchorPlan(
			policy.InternalKey, policyTapLeaves(policy),
		),
	}}
	anchorValues := []int64{outputValue}
	if change := state.Change; change != nil {
		outputs = append(outputs, tapsdk.CustomAssetOutput{
			ID:                onboardingChangeID,
			AssetRef:          assetRef,
			Amount:            change.Amount,
			AnchorOutputIndex: onboardingChangeOutputIndex,
			AnchorValueSat:    uint64(change.OutputValueSat),

			// The pinned wallet script key rides as an external
			// key: the wallet script mode would derive a fresh
			// key on every build, and a rebuild must reproduce
			// the identical transition.
			Script: tapsdk.CustomAssetScriptPlan{
				Mode: scriptExternal,
				External: &tapsdk.
					CustomAssetExternalScriptPlan{
					ScriptKey: change.ScriptKey,
				},
			},
			Anchor: tapsdk.CustomAnchorOutputPlan{
				InternalKey: change.AnchorInternalKey,
			},
		})
		anchorValues = append(anchorValues, change.OutputValueSat)
	}

	anchorPSBT, err := onboardingAnchorPSBT(anchorInputs, anchorValues)
	if err != nil {
		return nil, err
	}

	requestDTO := &tapsdk.CustomAnchorRequest{
		Inputs:     inputs,
		Outputs:    outputs,
		AnchorPSBT: anchorPSBT,
		Funding: tapsdk.CustomAnchorFundingPlan{
			Mode: tapsdk.CustomAnchorFundingWalletFunded,
			WalletFunded: &tapsdk.CustomAnchorWalletFunding{
				ChangeOutput: tapsdk.AnchorChangeOutput{
					Mode: tapsdk.AnchorChangeOutputAdd,
				},
				Fee:       fee,
				MaxFeeSat: request.MaxFeeSat,
				CustomLockID: onboardingCustomLockID(
					requestDigest,
				),
			},
		},
		PassiveAssets: tapsdk.CustomAnchorPassiveAssets{
			Policy: tapsdk.CustomAnchorPassiveReject,
		},
		LossPolicy: tapsdk.CustomAnchorLossPolicy{
			Mode: tapsdk.CustomAnchorLossReject,
		},
		SigningPlans: plans,
	}

	state.Attempt = onboardingAttemptCommit
	if err := o.storeState(ctx, request.RequestID, state); err != nil {
		return nil, err
	}

	committed, err := o.driver.CommitOnboarding(
		ctx, requestDTO, verifier,
	)
	if err != nil {
		if !commitOutcomeKnown(err) {
			return nil, fmt.Errorf("%w: onboarding commit for "+
				"request %q", ErrReconciliationRequired,
				request.RequestID)
		}
		state.Attempt = ""
		if storeErr := o.storeState(
			ctx, request.RequestID, state,
		); storeErr != nil {
			return nil, errors.Join(err, storeErr)
		}

		return nil, err
	}

	state.Attempt = ""
	state.TransferPackage = append([]byte(nil), committed.packageBytes...)
	if err := o.storeState(ctx, request.RequestID, state); err != nil {
		return nil, err
	}

	return committed, nil
}

// onboardingAssetInput is one verified funding UTXO of an onboarding: the
// proof selecting it, the tapd-managed anchor holding it, and the exact
// amount its proof tip carries.
type onboardingAssetInput struct {
	proofFile []byte
	anchor    *tapsdk.ManagedUtxo
	amount    uint64
}

// verifyInputs binds every funding proof to tapd's own managed inventory
// and returns the verifier the builder revalidates them with. The proofs
// select whole anchors, so their amounts are whatever the anchors hold; the
// caller reconciles the total against the requested amount.
func (o *Onboarder) verifyInputs(ctx context.Context,
	request *OnboardingRequest) (tapsdk.AssetRef, []onboardingAssetInput,
	tapsdk.ConfirmedProofVerifier, error) {

	assetRef, err := tapsdk.ParseAssetRef(request.AssetRef)
	if err != nil {
		return "", nil, nil,
			fmt.Errorf("parse Taproot Asset ref: %w", err)
	}

	utxos, err := o.inventory.ListUtxos(ctx, &tapsdk.ListUtxosRequest{
		IncludeLeased: true,
	})
	if err != nil {
		return "", nil, nil,
			fmt.Errorf("list tapd onboarding inventory: %w", err)
	}

	proofs := onboardingProofFiles(request)
	if len(proofs) == 0 {
		return "", nil, nil,
			fmt.Errorf("onboarding funding proof is required")
	}
	var (
		inputs    = make([]onboardingAssetInput, 0, len(proofs))
		verifiers = make(
			[]tapsdk.ConfirmedProofVerifier, 0, len(proofs),
		)
		selected = make(map[tapsdk.Outpoint]struct{}, len(proofs))
	)
	for idx, proofFile := range proofs {
		verified, err := o.inventory.VerifyProof(ctx, proofFile)
		if err != nil {
			return "", nil, nil, fmt.Errorf("verify onboarding "+
				"proof %d with tapd: %w", idx, err)
		}
		if verified == nil || !verified.Valid ||
			verified.DecodedProof == nil {
			return "", nil, nil, fmt.Errorf("tapd rejected "+
				"onboarding proof %d", idx)
		}
		tip := verified.DecodedProof
		if !tip.AssetRef.Equivalent(assetRef) || tip.Amount == 0 {
			return "", nil, nil, fmt.Errorf("onboarding proof %d "+
				"tip does not match request", idx)
		}

		// Two proofs selecting one anchor would double-count the units
		// it holds against the requested amount.
		if _, ok := selected[tip.Outpoint]; ok {
			return "", nil, nil, fmt.Errorf("onboarding proof %d "+
				"selects anchor %v twice", idx, tip.Outpoint)
		}
		selected[tip.Outpoint] = struct{}{}

		var anchor *tapsdk.ManagedUtxo
		for _, candidate := range utxos {
			if candidate != nil &&
				candidate.OutPoint == tip.Outpoint {

				anchor = candidate

				break
			}
		}
		if anchor == nil {
			return "", nil, nil, fmt.Errorf("onboarding proof %d "+
				"anchor is not managed by tapd", idx)
		}
		if len(anchor.Assets) != 1 {
			return "", nil, nil, fmt.Errorf("Taproot Asset "+
				"onboarding PoC requires one isolated asset, "+
				"found %d", len(anchor.Assets))
		}
		asset := anchor.Assets[0]
		if asset == nil ||
			asset.Genesis.IssuanceID != tip.IssuanceID ||
			asset.Amount != tip.Amount ||
			asset.ScriptKey.PubKey != tip.ScriptKey {
			return "", nil, nil, fmt.Errorf("tapd onboarding "+
				"inventory does not match proof %d", idx)
		}

		inputs = append(inputs, onboardingAssetInput{
			proofFile: proofFile,
			anchor:    anchor,
			amount:    tip.Amount,
		})
		verifiers = append(verifiers, &proofInventoryVerifier{
			client:    o.inventory,
			assetRef:  assetRef,
			amount:    tip.Amount,
			anchor:    tip.Outpoint,
			assetRoot: anchor.TaprootAssetRoot,
		})
	}

	// Every verifier pins its own tip claim, so one proof can only ever
	// satisfy the input it belongs to.
	verifier := verifiers[0]
	if len(verifiers) > 1 {
		verifier = &multiSourceVerifier{verifiers: verifiers}
	}

	return assetRef, inputs, verifier, nil
}

// pinChange resolves the funding surplus and, the first time an onboarding
// needs one, derives and persists the tapd wallet keys owning it. An exact
// funding total pins no change and keeps the single-output transition.
func (o *Onboarder) pinChange(ctx context.Context, request *OnboardingRequest,
	state *onboardingState, inputs []onboardingAssetInput) error {

	var total uint64
	for idx := range inputs {
		if total > math.MaxUint64-inputs[idx].amount {
			return fmt.Errorf("onboarding funding amounts overflow")
		}
		total += inputs[idx].amount
	}
	if total < request.AssetAmount {
		return fmt.Errorf("onboarding funding proofs carry %d units, "+
			"the request boards %d", total, request.AssetAmount)
	}

	amount := total - request.AssetAmount
	switch {
	case amount == 0 && state.Change != nil:
		return fmt.Errorf("onboarding funding no longer needs the " +
			"pinned asset change")

	case amount == 0:
		return nil

	case state.Change != nil:
		if state.Change.Amount != amount {
			return fmt.Errorf("onboarding funding needs %d units "+
				"of change, %d are pinned", amount,
				state.Change.Amount)
		}

		return nil
	}

	scriptKey, err := o.keys.DeriveScriptKey(ctx)
	if err != nil {
		return fmt.Errorf("derive onboarding change script key: %w",
			err)
	}
	internalKey, err := o.keys.DeriveInternalKey(ctx)
	if err != nil {
		return fmt.Errorf("derive onboarding change anchor "+
			"internal key: %w", err)
	}
	if scriptKey == nil || internalKey == nil {
		return fmt.Errorf("tapd returned an empty onboarding change " +
			"key")
	}

	state.Change = &onboardingChange{
		Amount:            amount,
		OutputValueSat:    onboardingChangeValueSat,
		ScriptKey:         *scriptKey,
		AnchorInternalKey: *internalKey,
	}

	return o.storeState(ctx, request.RequestID, state)
}

// onboardingAnchorSigner returns the tapd key-path signer of one funding
// anchor's Bitcoin input.
func onboardingAnchorSigner(anchor *tapsdk.ManagedUtxo) (tapsdk.XOnlyPubKey,
	error) {

	internalKey, err := btcec.ParsePubKey(anchor.InternalKey[:])
	if err != nil {
		return tapsdk.XOnlyPubKey{}, fmt.Errorf("parse onboarding "+
			"anchor internal key: %w", err)
	}
	signer, err := tapsdk.ParseXOnlyPubKey(
		schnorr.SerializePubKey(internalKey),
	)
	if err != nil {
		return tapsdk.XOnlyPubKey{}, fmt.Errorf("parse onboarding "+
			"anchor signer: %w", err)
	}

	return signer, nil
}

func onboardingResultFromCommit(request *OnboardingRequest,
	state *onboardingState, ownerKey keychain.KeyDescriptor,
	policy *arkscript.VTXOPolicy, digest tapsdk.Hash,
	committed *commitResult) (*OnboardingResult, error) {

	wantInputs := len(onboardingProofFiles(request))
	wantOutputs := 1
	if state.Change != nil {
		wantOutputs = 2
	}
	if committed == nil || len(committed.inputs) != wantInputs ||
		len(committed.outputs) != wantOutputs {
		return nil, fmt.Errorf("onboarding package must contain %d "+
			"inputs and %d outputs", wantInputs, wantOutputs)
	}
	if committed.fundingMode != tapsdk.CustomAnchorFundingWalletFunded {
		return nil, fmt.Errorf("onboarding package is not wallet " +
			"funded")
	}
	if committed.maxFeeSat != request.MaxFeeSat {
		return nil, fmt.Errorf("onboarding package maximum fee %d "+
			"does not match request %d", committed.maxFeeSat,
			request.MaxFeeSat)
	}
	if committed.actualFeeSat > committed.maxFeeSat {
		return nil, fmt.Errorf("onboarding package actual fee %d "+
			"exceeds maximum %d", committed.actualFeeSat,
			committed.maxFeeSat)
	}
	assetRef, err := tapsdk.ParseAssetRef(request.AssetRef)
	if err != nil {
		return nil, err
	}
	output, err := committedOutput(committed, onboardingOutputID)
	if err != nil {
		return nil, err
	}

	// The transition conserves the asset exactly: everything the funding
	// anchors carry either boards or returns as pinned change.
	wantTotal := request.AssetAmount
	if state.Change != nil {
		wantTotal += state.Change.Amount
	}
	var inputTotal uint64
	for idx := range committed.inputs {
		input := committed.inputs[idx]
		if !input.assetRef.Equivalent(assetRef) {
			return nil, fmt.Errorf("onboarding package input %d "+
				"carries another asset", idx)
		}
		if inputTotal > math.MaxUint64-input.amount {
			return nil, fmt.Errorf("onboarding package input " +
				"amounts overflow")
		}
		inputTotal += input.amount
	}
	if inputTotal != wantTotal {
		return nil, fmt.Errorf("onboarding package spends %d units, "+
			"the boarding and change carry %d", inputTotal,
			wantTotal)
	}
	if !output.assetRef.Equivalent(assetRef) ||
		output.amount != request.AssetAmount {
		return nil, fmt.Errorf("onboarding package asset selection " +
			"mismatch")
	}
	if output.anchorOutputIndex != 0 || output.anchorValueSat !=
		int64(request.CarrierValueSat) {
		return nil, fmt.Errorf("onboarding package output shape " +
			"mismatch")
	}
	if output.anchorOutpoint.Index != output.anchorOutputIndex {
		return nil, fmt.Errorf("onboarding package output index " +
			"mismatch")
	}
	if output.taprootAssetRoot == (tapsdk.Hash{}) ||
		output.taprootMerkleRoot == (tapsdk.Hash{}) {
		return nil, fmt.Errorf("onboarding package root hints are " +
			"missing")
	}
	if len(output.proofBlob) == 0 {
		return nil, fmt.Errorf("onboarding package proof update is " +
			"missing")
	}

	root := chainhash.Hash(output.taprootAssetRoot)
	composed, err := arkscript.ComposeWithSiblingRoot(
		policy.CompiledPolicy, root,
	)
	if err != nil {
		return nil, err
	}
	pkScript, err := txscript.PayToTaprootScript(composed.OutputKey())
	if err != nil {
		return nil, err
	}
	packet, err := psbtutil.Parse(committed.anchorPSBT)
	if err != nil {
		return nil, err
	}
	if output.anchorOutputIndex >= uint32(len(packet.UnsignedTx.TxOut)) {
		return nil, fmt.Errorf("onboarding package output index is " +
			"out of range")
	}
	anchorOutput := packet.UnsignedTx.TxOut[output.anchorOutputIndex]
	if anchorOutput.Value != output.anchorValueSat {
		return nil, fmt.Errorf("committed onboarding anchor does not " +
			"match VTXO policy and root")
	}
	if err := validateOutputCommitment(
		anchorOutput, policy.InternalKey, policy.RootHash, output,
	); err != nil {
		return nil, fmt.Errorf("committed onboarding output: %w", err)
	}
	if !bytes.Equal(anchorOutput.PkScript, pkScript) {
		return nil, fmt.Errorf("committed onboarding output policy " +
			"mismatch")
	}
	outpoint := wire.OutPoint{
		Hash:  chainhash.Hash(output.anchorOutpoint.Txid),
		Index: output.anchorOutpoint.Index,
	}
	if packet.UnsignedTx.TxHash() != outpoint.Hash {
		return nil, fmt.Errorf("onboarding package outpoint mismatch")
	}
	if err := checkOnboardingChange(
		state.Change, committed, packet.UnsignedTx,
	); err != nil {
		return nil, fmt.Errorf("committed onboarding change: %w", err)
	}

	return &OnboardingResult{
		Outpoint:         outpoint,
		ValueSat:         output.anchorValueSat,
		AssetRef:         output.assetRef.String(),
		AssetAmount:      output.amount,
		ActualFeeSat:     committed.actualFeeSat,
		PolicyTemplate:   append([]byte(nil), state.PolicyTemplate...),
		PkScript:         pkScript,
		TaprootAssetRoot: root,
		ScriptKey:        output.scriptKey,
		OwnerKey:         ownerKey,
		OperatorKey:      request.OperatorKey,
		ExitDelay:        request.ExitDelay,
		Digest:           digest,
		OPTrueWitness:    cloneByteSlices(output.opTrueWitness),
	}, nil
}

// checkOnboardingChange verifies fail-closed that the committed asset change
// is the one that was pinned: the same units on the same anchor position,
// keyed to the pinned wallet script key, under a BIP-86 output of the pinned
// anchor internal key. Nil change passes.
func checkOnboardingChange(change *onboardingChange, committed *commitResult,
	anchorTx *wire.MsgTx) error {

	if change == nil {
		return nil
	}

	out, err := committedOutput(committed, onboardingChangeID)
	if err != nil {
		return err
	}
	if out.anchorOutputIndex != onboardingChangeOutputIndex {
		return fmt.Errorf("output index %d, want %d",
			out.anchorOutputIndex, onboardingChangeOutputIndex)
	}
	if out.anchorOutpoint.Index != out.anchorOutputIndex {
		return fmt.Errorf("outpoint index mismatch")
	}
	if out.amount != change.Amount {
		return fmt.Errorf("amount %d, want %d", out.amount,
			change.Amount)
	}
	if out.anchorValueSat != change.OutputValueSat {
		return fmt.Errorf("value %d, want %d", out.anchorValueSat,
			change.OutputValueSat)
	}
	if out.scriptMode != scriptExternal {
		return fmt.Errorf("script mode %d is not the pinned external "+
			"wallet key", out.scriptMode)
	}
	if out.scriptKey != change.ScriptKey.PubKey {
		return fmt.Errorf("script key does not reproduce the pinned " +
			"wallet key")
	}
	if out.taprootAssetRoot == (tapsdk.Hash{}) ||
		out.taprootMerkleRoot == (tapsdk.Hash{}) {
		return fmt.Errorf("root hints are missing")
	}

	internalKey, err := btcec.ParsePubKey(
		change.AnchorInternalKey.PubKey[:],
	)
	if err != nil {
		return fmt.Errorf("pinned anchor internal key: %w", err)
	}
	pkScript, err := composedScript(internalKey, out.taprootMerkleRoot)
	if err != nil {
		return err
	}
	if int(out.anchorOutputIndex) >= len(anchorTx.TxOut) {
		return fmt.Errorf("output index %d is out of range",
			out.anchorOutputIndex)
	}
	anchorOutput := anchorTx.TxOut[out.anchorOutputIndex]
	if anchorOutput.Value != change.OutputValueSat {
		return fmt.Errorf("anchor value %d, want %d",
			anchorOutput.Value, change.OutputValueSat)
	}
	if !bytes.Equal(anchorOutput.PkScript, pkScript) {
		return fmt.Errorf("anchor output does not reproduce the " +
			"pinned wallet script")
	}

	return nil
}

func (o *Onboarder) loadState(ctx context.Context, request *OnboardingRequest,
	digest tapsdk.Hash) (*onboardingState, error) {

	encoded, err := o.store.Load(
		ctx, onboardingStorePrefix+request.RequestID,
	)
	if errors.Is(err, ErrStoreNotFound) {
		owner, deriveErr := o.deriveOwnerKey(ctx)
		if deriveErr != nil {
			return nil, fmt.Errorf("derive onboarding "+
				"owner key: %w", deriveErr)
		}
		if owner == nil || owner.PubKey == nil {
			return nil, fmt.Errorf("owner key deriver returned " +
				"empty key")
		}

		return &onboardingState{
			Version:        onboardingStateVersion,
			RequestDigest:  digest,
			OwnerPubKey:    owner.PubKey.SerializeCompressed(),
			OwnerKeyFamily: int32(owner.Family),
			OwnerKeyIndex:  owner.Index,
		}, nil
	}
	if err != nil {
		return nil, err
	}
	var state onboardingState
	if err := json.Unmarshal(encoded, &state); err != nil {
		return nil, fmt.Errorf("decode taproot asset onboarding "+
			"state: %w", err)
	}
	if state.Version != onboardingStateVersion {
		return nil, fmt.Errorf("unsupported taproot asset onboarding "+
			"state version %d", state.Version)
	}
	if state.RequestDigest != digest {
		return nil, fmt.Errorf("Taproot Asset onboarding idempotency " +
			"key reused with different request")
	}
	if state.Attempt != "" && state.Attempt != onboardingAttemptCommit {
		return nil, fmt.Errorf("invalid taproot asset onboarding "+
			"attempt %q", state.Attempt)
	}

	return &state, nil
}

func (o *Onboarder) storeState(ctx context.Context, requestID string,
	state *onboardingState) error {

	encoded, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode taproot asset onboarding state: %w",
			err)
	}

	return o.store.Store(ctx, onboardingStorePrefix+requestID, encoded)
}

func validateOnboardingRequest(request *OnboardingRequest) error {
	if request == nil {
		return fmt.Errorf("taproot asset onboarding request is " +
			"required")
	}
	if request.RequestID == "" {
		return fmt.Errorf("taproot asset onboarding idempotency key " +
			"is required")
	}
	if request.AssetRef == "" || request.AssetAmount == 0 {
		return fmt.Errorf("taproot asset ref and amount are required")
	}
	if len(request.ProofFile) != 0 && len(request.ProofFiles) != 0 {
		return fmt.Errorf("taproot asset onboarding takes either one " +
			"proof file or a proof file set")
	}
	proofs := onboardingProofFiles(request)
	if len(proofs) == 0 {
		return fmt.Errorf("taproot asset ref, amount, and proof are " +
			"required")
	}
	for idx := range proofs {
		if len(proofs[idx]) == 0 {
			return fmt.Errorf("taproot asset onboarding proof %d "+
				"is empty", idx)
		}
	}
	if len(request.AssetRef) > vtxo.MaxTaprootAssetRefBytes {
		return fmt.Errorf("taproot asset ref exceeds %d bytes",
			vtxo.MaxTaprootAssetRefBytes)
	}
	if request.CarrierValueSat == 0 {
		return fmt.Errorf("taproot asset onboarding carrier value is " +
			"required")
	}
	if request.CarrierValueSat < onboardingDustFloorSat {
		return fmt.Errorf("taproot asset onboarding carrier value %d "+
			"is below the Taproot dust floor",
			request.CarrierValueSat)
	}
	if request.CarrierValueSat > math.MaxInt64 {
		return fmt.Errorf("taproot asset onboarding carrier value %d "+
			"is too large", request.CarrierValueSat)
	}
	if request.MaxFeeSat == 0 {
		return fmt.Errorf("taproot asset onboarding maximum fee is " +
			"required")
	}
	if _, err := onboardingAnchorFee(request); err != nil {
		return err
	}
	if request.OperatorKey == nil || request.ExitDelay == 0 {
		return fmt.Errorf("taproot asset onboarding operator policy " +
			"is required")
	}

	return nil
}

// onboardingRequestDigest binds one onboarding's retry identity. The funding
// proofs enter in content order, so the digest is independent of the order
// the selection or the durable replay slice hands them over in.
func onboardingRequestDigest(request *OnboardingRequest) tapsdk.Hash {
	var value bytes.Buffer
	writeDigestBytes(&value, []byte(request.RequestID))
	writeDigestBytes(&value, []byte(request.AssetRef))
	_ = binary.Write(&value, binary.BigEndian, request.AssetAmount)
	for _, proofFile := range sortedProofFiles(request) {
		writeDigestBytes(&value, proofFile)
	}
	_ = binary.Write(&value, binary.BigEndian, request.CarrierValueSat)
	_ = binary.Write(
		&value, binary.BigEndian, request.FeeRateSatPerVByte,
	)
	_ = binary.Write(&value, binary.BigEndian, request.TargetConf)
	_ = binary.Write(&value, binary.BigEndian, request.MaxFeeSat)
	writeDigestBytes(&value, request.OperatorKey.SerializeCompressed())
	_ = binary.Write(&value, binary.BigEndian, request.ExitDelay)
	digest := sha256.Sum256(value.Bytes())

	return tapsdk.Hash(digest)
}

func onboardingAnchorFee(request *OnboardingRequest) (tapsdk.AnchorFee, error) {
	if request == nil {
		return tapsdk.AnchorFee{}, fmt.Errorf("taproot asset " +
			"onboarding request is required")
	}
	hasFeeRate := request.FeeRateSatPerVByte != 0
	hasTargetConf := request.TargetConf != 0
	if hasFeeRate == hasTargetConf {
		return tapsdk.AnchorFee{}, fmt.Errorf("taproot asset " +
			"onboarding requires exactly one of fee rate and " +
			"target confirmation")
	}
	if hasTargetConf {
		return tapsdk.AnchorFee{
			Mode:       tapsdk.AnchorFeeTargetConf,
			TargetConf: request.TargetConf,
		}, nil
	}

	feeRate, err := tapsdk.NewFeeRateSatPerVByte(
		request.FeeRateSatPerVByte,
	)
	if err != nil {
		return tapsdk.AnchorFee{}, fmt.Errorf("taproot asset "+
			"onboarding fee rate: %w", err)
	}

	return tapsdk.AnchorFee{
		Mode:    tapsdk.AnchorFeeSatPerVByte,
		FeeRate: feeRate,
	}, nil
}

// sortedProofFiles returns the request's funding proofs ordered by content,
// without disturbing the caller's slice.
func sortedProofFiles(request *OnboardingRequest) [][]byte {
	proofs := onboardingProofFiles(request)
	sorted := append([][]byte(nil), proofs...)
	sort.Slice(sorted, func(i, j int) bool {
		return bytes.Compare(sorted[i], sorted[j]) < 0
	})

	return sorted
}

func onboardingCustomLockID(requestDigest tapsdk.Hash) []byte {
	var value bytes.Buffer
	writeDigestBytes(&value, []byte(onboardingLockIDDomain))
	writeDigestBytes(&value, requestDigest[:])
	lockID := sha256.Sum256(value.Bytes())

	return lockID[:]
}

func ownerKeyFromState(state *onboardingState) (keychain.KeyDescriptor, error) {
	if state == nil {
		return keychain.KeyDescriptor{}, fmt.Errorf("onboarding " +
			"state is nil")
	}
	pubKey, err := btcec.ParsePubKey(state.OwnerPubKey)
	if err != nil {
		return keychain.KeyDescriptor{}, fmt.Errorf("parse stored "+
			"onboarding owner key: %w", err)
	}

	return keychain.KeyDescriptor{
		KeyLocator: keychain.KeyLocator{
			Family: keychain.KeyFamily(state.OwnerKeyFamily),
			Index:  state.OwnerKeyIndex,
		},
		PubKey: pubKey,
	}, nil
}

// onboardingAnchorPSBT builds the unfunded anchor template: one input per
// funding anchor and one placeholder output per asset output, in index
// order. Wallet funding adds the Bitcoin inputs and appends its change after
// them, so tapd rewrites the placeholder scripts with the real composed
// ones and every asset output keeps the position derived here.
func onboardingAnchorPSBT(inputs []tapsdk.Outpoint,
	values []int64) ([]byte, error) {

	if len(inputs) == 0 || len(values) == 0 {
		return nil, fmt.Errorf("onboarding anchor template needs an " +
			"input and an output")
	}
	placeholderKey := txscript.ComputeTaprootKeyNoScript(
		&arkscript.ARKNUMSKey,
	)
	placeholderScript, err := txscript.PayToTaprootScript(placeholderKey)
	if err != nil {
		return nil, err
	}

	tx := wire.NewMsgTx(2)
	for _, input := range inputs {
		tx.AddTxIn(
			wire.NewTxIn(
				&wire.OutPoint{
					Hash:  chainhash.Hash(input.Txid),
					Index: input.Index,
				},
				nil,
				nil,
			),
		)
	}
	for _, value := range values {
		tx.AddTxOut(&wire.TxOut{
			Value:    value,
			PkScript: placeholderScript,
		})
	}
	packet, err := psbt.NewFromUnsignedTx(tx)
	if err != nil {
		return nil, fmt.Errorf("build onboarding anchor PSBT: %w", err)
	}

	return psbtutil.Serialize(packet)
}

func policyTapLeaves(policy *arkscript.VTXOPolicy) []txscript.TapLeaf {
	if policy == nil {
		return nil
	}
	leaves := make([]txscript.TapLeaf, len(policy.Leaves))
	for idx := range policy.Leaves {
		leaves[idx] = policy.Leaves[idx].Leaf
	}

	return leaves
}

// boardingScriptPlan keys the new anchor's asset output to a
// digest-scoped OP_TRUE script, so the operator of the round that
// consumes it can build the transition that spends it. Custody is
// unaffected: it rests on the composed Bitcoin output, which still
// requires the owner's collaborative-leaf signature.
func boardingScriptPlan(digest tapsdk.Hash) tapsdk.CustomAssetScriptPlan {
	return tapsdk.CustomAssetScriptPlan{
		Mode: tapsdk.CustomAssetScriptOPTrue,
		OPTrue: &tapsdk.CustomAssetOPTrueScriptPlan{
			InternalKey: tapsdk.KeyDescriptor{
				RawKeyBytes: deterministicKey(
					digest, "onboarding-boarding",
				),
			},
		},
	}
}
