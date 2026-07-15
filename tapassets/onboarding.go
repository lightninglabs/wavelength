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
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
	"github.com/lightningnetwork/lnd/keychain"
)

const (
	onboardingStateVersion  = uint16(0)
	onboardingAttemptCommit = "commit"
	onboardingStorePrefix   = "onboarding/"
)

// ErrOnboardingPendingConfirmation means the exact published asset anchor is
// not confirmed deeply enough for the operator to admit it yet. Retrying the
// same request is safe and reuses the sealed package and final anchor PSBT.
var ErrOnboardingPendingConfirmation = errors.New("taproot asset onboarding " +
	"pending confirmation")

// OnboardingRequest selects one complete Taproot Asset proof and the standard
// Wavelength policy that will own its new on-chain anchor.
type OnboardingRequest struct {
	RequestID    string
	AssetRef     string
	AssetAmount  uint64
	ProofFile    []byte
	AnchorFeeSat uint64
	OperatorKey  *btcec.PublicKey
	ExitDelay    uint32
}

// OnboardingKeyDeriver returns the next wallet-owned standard VTXO key.
type OnboardingKeyDeriver func(context.Context) (*keychain.KeyDescriptor, error)

// OnboardingRegistration is the credential-free package sent to the
// operator after tap-sdk has committed and Wavelength has signed the anchor.
type OnboardingRegistration struct {
	TransferPackage  []byte
	FinalAnchorPSBT  []byte
	PolicyTemplate   []byte
	TaprootAssetRoot tapsdk.Hash
}

// OnboardingRegistrationResult is the operator's confirmed admission result.
type OnboardingRegistrationResult struct {
	Outpoint           wire.OutPoint
	ConfirmationHeight int32
}

// OnboardingRegistrar admits one confirmed direct-on-chain asset VTXO.
type OnboardingRegistrar func(context.Context, OnboardingRegistration) (
	*OnboardingRegistrationResult, error)

// OnboardingStatus is the durable stage visible to the daemon RPC.
type OnboardingStatus uint8

const (
	OnboardingStatusUnknown OnboardingStatus = iota
	OnboardingStatusPendingConfirmation
	OnboardingStatusReady
)

// OnboardingResult contains the local descriptor material for the admitted
// direct-on-chain VTXO. The final asset proof remains managed by tapd.
type OnboardingResult struct {
	Status             OnboardingStatus
	Outpoint           wire.OutPoint
	ValueSat           int64
	PolicyTemplate     []byte
	PkScript           []byte
	TaprootAssetRoot   chainhash.Hash
	OwnerKey           keychain.KeyDescriptor
	OperatorKey        *btcec.PublicKey
	ExitDelay          uint32
	ConfirmationHeight int32
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
	Registrar      OnboardingRegistrar
}

// Onboarder moves a tapd-managed confirmed asset anchor into a standard
// Wavelength policy and registers the resulting direct-on-chain VTXO.
type Onboarder struct {
	driver         onboardingDriver
	inventory      proofInventoryClient
	store          Store
	signer         tapsdk.AnchorSigner
	deriveOwnerKey OnboardingKeyDeriver
	registrar      OnboardingRegistrar
	mu             sync.Mutex
}

type onboardingState struct {
	Version            uint16      `json:"version"`
	RequestDigest      tapsdk.Hash `json:"request_digest"`
	Attempt            string      `json:"attempt,omitempty"`
	OwnerPubKey        []byte      `json:"owner_pub_key"`
	OwnerKeyFamily     int32       `json:"owner_key_family"`
	OwnerKeyIndex      uint32      `json:"owner_key_index"`
	PolicyTemplate     []byte      `json:"policy_template"`
	TransferPackage    []byte      `json:"transfer_package,omitempty"`
	FinalAnchorPSBT    []byte      `json:"final_anchor_psbt,omitempty"`
	Published          bool        `json:"published"`
	Registered         bool        `json:"registered"`
	ConfirmationHeight int32       `json:"confirmation_height,omitempty"`
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
	if cfg.Registrar == nil {
		return nil, fmt.Errorf("taproot asset onboarding registrar " +
			"is required")
	}

	return &Onboarder{
		driver: &sdkDriver{
			wallet: cfg.Wallet,
		},
		inventory:      cfg.Wallet.Client(),
		store:          cfg.Store,
		signer:         cfg.Signer,
		deriveOwnerKey: cfg.DeriveOwnerKey,
		registrar:      cfg.Registrar,
	}, nil
}

// Onboard performs or resumes one idempotent onboarding request. Once the
// commit succeeds, every retry reuses the exact package and final PSBT bytes.
func (o *Onboarder) Onboard(ctx context.Context, request *OnboardingRequest) (
	*OnboardingResult, error) {

	if o == nil || o.driver == nil || o.inventory == nil ||
		o.store == nil ||
		o.signer == nil || o.deriveOwnerKey == nil ||
		o.registrar == nil {
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
		request, state, ownerKey, policy, committed,
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

	if !state.Registered {
		registration, registerErr := o.registrar(
			ctx, OnboardingRegistration{
				TransferPackage: append(
					[]byte(nil), state.TransferPackage...,
				),
				FinalAnchorPSBT: append(
					[]byte(nil), state.FinalAnchorPSBT...,
				),
				PolicyTemplate: append(
					[]byte(nil), state.PolicyTemplate...,
				),
				TaprootAssetRoot: tapsdk.Hash(
					result.TaprootAssetRoot,
				),
			},
		)
		if errors.Is(registerErr, ErrOnboardingPendingConfirmation) {
			result.Status = OnboardingStatusPendingConfirmation

			return result, nil
		}
		if registerErr != nil {
			return nil, fmt.Errorf("register onboarding VTXO: %w",
				registerErr)
		}
		if registration == nil {
			return nil, fmt.Errorf("operator returned empty " +
				"onboarding registration")
		}
		if registration.Outpoint != result.Outpoint {
			return nil, fmt.Errorf("operator registered " +
				"unexpected onboarding outpoint")
		}
		if registration.ConfirmationHeight <= 0 {
			return nil, fmt.Errorf("operator returned invalid " +
				"onboarding confirmation height")
		}

		state.Registered = true
		state.ConfirmationHeight = registration.ConfirmationHeight
		if err := o.storeState(
			ctx, request.RequestID, state,
		); err != nil {
			return nil, err
		}
	}

	result.Status = OnboardingStatusReady
	result.ConfirmationHeight = state.ConfirmationHeight

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

	assetRef, anchor, verifier, err := o.verifyInput(ctx, request)
	if err != nil {
		return nil, err
	}
	if anchor.AmtSat <= int64(request.AnchorFeeSat) {
		return nil, fmt.Errorf("asset anchor value %d does not "+
			"cover fee %d", anchor.AmtSat, request.AnchorFeeSat)
	}
	outputValue := anchor.AmtSat - int64(request.AnchorFeeSat)
	if outputValue < 330 {
		return nil, fmt.Errorf("onboarding output value %d is below "+
			"the Taproot dust floor", outputValue)
	}

	anchorPSBT, err := onboardingAnchorPSBT(anchor.OutPoint, outputValue)
	if err != nil {
		return nil, err
	}
	anchorInternalKey, err := btcec.ParsePubKey(anchor.InternalKey[:])
	if err != nil {
		return nil, fmt.Errorf("parse onboarding anchor "+
			"internal key: %w", err)
	}
	anchorSigner, err := tapsdk.ParseXOnlyPubKey(
		schnorr.SerializePubKey(anchorInternalKey),
	)
	if err != nil {
		return nil, fmt.Errorf("parse onboarding anchor signer: %w",
			err)
	}

	requestDTO := &tapsdk.CustomAnchorRequest{
		Inputs: []tapsdk.CustomAssetInput{{
			ID:        "wavelength-onboarding-input-0",
			AssetRef:  assetRef,
			Amount:    request.AssetAmount,
			ProofFile: append([]byte(nil), request.ProofFile...),
			Witness: tapsdk.CustomAssetWitnessPlan{
				Mode: tapsdk.CustomAssetWitnessBackendSigner,
			},
		}},
		Outputs: []tapsdk.CustomAssetOutput{{
			ID:                "wavelength-onboarding-output-0",
			AssetRef:          assetRef,
			Amount:            request.AssetAmount,
			AnchorOutputIndex: 0,
			AnchorValueSat:    uint64(outputValue),
			Script: tapsdk.CustomAssetScriptPlan{
				Mode:   tapsdk.CustomAssetScriptWallet,
				Wallet: &tapsdk.CustomAssetWalletScriptPlan{},
			},
			Anchor: anchorPlan(
				policy.InternalKey, policyTapLeaves(policy),
			),
		}},
		AnchorPSBT: anchorPSBT,
		Funding:    callerFundedExact(),
		PassiveAssets: tapsdk.CustomAnchorPassiveAssets{
			Policy: tapsdk.CustomAnchorPassiveReject,
		},
		LossPolicy: tapsdk.CustomAnchorLossPolicy{
			Mode: tapsdk.CustomAnchorLossReject,
		},
		SigningPlans: []tapsdk.CustomAnchorInputSigningPlan{{
			InputIndex: 0,
			KeyPath: &tapsdk.CustomAnchorKeyPathSigningPlan{
				Signer: anchorSigner,
			},
		}},
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

func (o *Onboarder) verifyInput(ctx context.Context,
	request *OnboardingRequest) (tapsdk.AssetRef, *tapsdk.ManagedUtxo,
	tapsdk.ConfirmedProofVerifier, error) {

	assetRef, err := tapsdk.ParseAssetRef(request.AssetRef)
	if err != nil {
		return "", nil, nil,
			fmt.Errorf("parse Taproot Asset ref: %w", err)
	}
	verified, err := o.inventory.VerifyProof(ctx, request.ProofFile)
	if err != nil {
		return "", nil, nil,
			fmt.Errorf("verify onboarding proof with tapd: %w", err)
	}
	if verified == nil || !verified.Valid || verified.DecodedProof == nil {
		return "", nil, nil,
			fmt.Errorf("tapd rejected onboarding proof")
	}
	tip := verified.DecodedProof
	if !tip.AssetRef.Equivalent(assetRef) ||
		tip.Amount != request.AssetAmount {
		return "", nil, nil,
			fmt.Errorf("onboarding proof tip does not match " +
				"request")
	}

	utxos, err := o.inventory.ListUtxos(ctx, &tapsdk.ListUtxosRequest{
		IncludeLeased: true,
	})
	if err != nil {
		return "", nil, nil,
			fmt.Errorf("list tapd onboarding inventory: %w", err)
	}
	var anchor *tapsdk.ManagedUtxo
	for _, candidate := range utxos {
		if candidate != nil && candidate.OutPoint == tip.Outpoint {
			anchor = candidate
			break
		}
	}
	if anchor == nil {
		return "", nil, nil,
			fmt.Errorf("onboarding proof anchor is not managed " +
				"by tapd")
	}
	if len(anchor.Assets) != 1 {
		return "", nil, nil, fmt.Errorf("Taproot Asset onboarding PoC "+
			"requires one isolated asset, found %d",
			len(anchor.Assets))
	}
	asset := anchor.Assets[0]
	if asset == nil || asset.Genesis.IssuanceID != tip.IssuanceID ||
		asset.Amount != tip.Amount ||
		asset.ScriptKey.PubKey != tip.ScriptKey {
		return "", nil, nil,
			fmt.Errorf("tapd onboarding inventory does not match " +
				"proof")
	}

	verifier := &proofInventoryVerifier{
		client:    o.inventory,
		assetRef:  assetRef,
		amount:    request.AssetAmount,
		anchor:    tip.Outpoint,
		assetRoot: anchor.TaprootAssetRoot,
	}

	return assetRef, anchor, verifier, nil
}

func onboardingResultFromCommit(request *OnboardingRequest,
	state *onboardingState, ownerKey keychain.KeyDescriptor,
	policy *arkscript.VTXOPolicy,
	committed *commitResult) (*OnboardingResult, error) {

	if committed == nil || len(committed.inputs) != 1 ||
		len(committed.outputs) != 1 {
		return nil, fmt.Errorf("onboarding package must contain one " +
			"input and one output")
	}
	assetRef, err := tapsdk.ParseAssetRef(request.AssetRef)
	if err != nil {
		return nil, err
	}
	input := committed.inputs[0]
	output := committed.outputs[0]
	if !input.assetRef.Equivalent(assetRef) ||
		!output.assetRef.Equivalent(assetRef) ||
		input.amount != request.AssetAmount ||
		output.amount != request.AssetAmount {
		return nil, fmt.Errorf("onboarding package asset selection " +
			"mismatch")
	}
	if output.anchorOutputIndex != 0 || output.anchorValueSat <= 0 {
		return nil, fmt.Errorf("onboarding package output shape " +
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
	if len(packet.UnsignedTx.TxOut) != 1 ||
		packet.UnsignedTx.TxOut[0].Value != output.anchorValueSat {
		return nil, fmt.Errorf("committed onboarding anchor does not " +
			"match VTXO policy and root")
	}
	if err := validateOutputCommitment(
		packet.UnsignedTx.TxOut[0], policy.InternalKey, policy.RootHash,
		output,
	); err != nil {
		return nil, fmt.Errorf("committed onboarding output: %w", err)
	}
	if !bytes.Equal(packet.UnsignedTx.TxOut[0].PkScript, pkScript) {
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

	return &OnboardingResult{
		Outpoint:         outpoint,
		ValueSat:         output.anchorValueSat,
		PolicyTemplate:   append([]byte(nil), state.PolicyTemplate...),
		PkScript:         pkScript,
		TaprootAssetRoot: root,
		OwnerKey:         ownerKey,
		OperatorKey:      request.OperatorKey,
		ExitDelay:        request.ExitDelay,
	}, nil
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
	if request.AssetRef == "" || request.AssetAmount == 0 ||
		len(request.ProofFile) == 0 {
		return fmt.Errorf("taproot asset ref, amount, and proof are " +
			"required")
	}
	if request.AnchorFeeSat == 0 {
		return fmt.Errorf("taproot asset onboarding anchor fee is " +
			"required")
	}
	if request.OperatorKey == nil || request.ExitDelay == 0 {
		return fmt.Errorf("taproot asset onboarding operator policy " +
			"is required")
	}

	return nil
}

func onboardingRequestDigest(request *OnboardingRequest) tapsdk.Hash {
	var value bytes.Buffer
	writeDigestBytes(&value, []byte(request.RequestID))
	writeDigestBytes(&value, []byte(request.AssetRef))
	_ = binary.Write(&value, binary.BigEndian, request.AssetAmount)
	writeDigestBytes(&value, request.ProofFile)
	_ = binary.Write(&value, binary.BigEndian, request.AnchorFeeSat)
	writeDigestBytes(&value, request.OperatorKey.SerializeCompressed())
	_ = binary.Write(&value, binary.BigEndian, request.ExitDelay)
	digest := sha256.Sum256(value.Bytes())

	return tapsdk.Hash(digest)
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

func onboardingAnchorPSBT(input tapsdk.Outpoint, value int64) ([]byte, error) {
	placeholderKey := txscript.ComputeTaprootKeyNoScript(
		&arkscript.ARKNUMSKey,
	)
	placeholderScript, err := txscript.PayToTaprootScript(placeholderKey)
	if err != nil {
		return nil, err
	}

	tx := wire.NewMsgTx(2)
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
	tx.AddTxOut(&wire.TxOut{
		Value:    value,
		PkScript: placeholderScript,
	})
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
