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
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
	"github.com/lightninglabs/wavelength/walletcore"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

const (
	onboardingStateVersion   = uint16(0)
	onboardingAttemptCommit  = "commit"
	onboardingAttemptPublish = "publish"
	onboardingStorePrefix    = "onboarding/"

	onboardingLockIDDomain = "wavelength/taproot-assets/onboarding-lock/v1"
	onboardingDustFloorSat = uint64(330)

	// Asset change uses tapd's wallet anchor value, independently of the
	// operator's boarding carrier value.
	onboardingChangeValueSat = int64(1_000)

	// onboardingLeaseExpiry bounds how long one funding attempt reserves
	// its wallet inputs. A resumed attempt renews the leases.
	onboardingLeaseExpiry = 24 * time.Hour

	// onboardingAnchorInputWeight is the weight a tapd anchor adds when
	// spent through its key path with a default sighash signature.
	onboardingAnchorInputWeight = 4*input.InputSize +
		input.TaprootKeyPathWitnessSize
)

// These output IDs are persisted in sealed packages.
const (
	onboardingOutputID = "wavelength-onboarding-output-0"
	onboardingChangeID = "wavelength-onboarding-change"
)

// Asset outputs precede the Bitcoin change of the funding wallet.
const onboardingChangeOutputIndex = uint32(1)

// OnboardingRequest funds a boarding output from confirmed tapd assets.
// Selected proofs must cover AssetAmount; surplus returns to tapd as change.
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

// onboardingProofFiles normalizes the singular and plural proof fields.
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

// OnboardingResult describes a boarding output accepted by tapd for
// publication. Callers must wait for confirmation and export its proof using
// ScriptKey before joining a round.
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

	// ScriptKey identifies the proof in tapd; the OP_TRUE output is not
	// part of tapd's spendable wallet inventory.
	ScriptKey tapsdk.PubKey

	// OPTrueWitness authorizes the asset transition into a round.
	OPTrueWitness [][]byte
}

type onboardingDriver interface {
	CommitOnboarding(context.Context,
		*tapsdk.CustomAnchorRequest) (*commitResult, error)

	DecodePackage([]byte) (*commitResult, error)

	VerifyFinalOnboarding([]byte, []byte) error

	Publish(context.Context, []byte, []byte) error

	ReconcileOnboarding(context.Context, []byte) error
}

// OnboardingFunder pays the Bitcoin side of an onboarding from a wallet other
// than tapd's: the outputs beyond what the spent tapd anchors carry, and the
// fee. Its inputs follow the anchors and its change follows the asset outputs.
type OnboardingFunder interface {
	walletcore.OutputLeaser

	// FeeRate returns the fee rate, in sat/vB, that confirms within
	// targetConf blocks.
	FeeRate(ctx context.Context, targetConf uint32) (uint64, error)

	// FundTemplate adds wallet inputs and change to an outputs-only
	// template without signing it, and leases the inputs under lockID. It
	// returns the change output's index, or -1 without change.
	FundTemplate(ctx context.Context, packet *psbt.Packet,
		feeRateSatPerVByte int64, lockID walletcore.LockID,
		lockExpiry time.Duration) (int32, error)

	// FinalizePsbt signs the wallet's inputs and returns the final
	// transaction. Every other input is already final.
	FinalizePsbt(ctx context.Context, packet []byte) (*wire.MsgTx, error)
}

// OnboarderConfig supplies the asset wallet, journal, and Bitcoin signers.
// Signer finalizes the tapd anchor inputs, and Funder pays and signs for the
// rest. The caller must serialize funding and use one Onboarder per Store.
type OnboarderConfig struct {
	Wallet         *tapsdk.Wallet
	Store          Store
	Signer         tapsdk.AnchorSigner
	Funder         OnboardingFunder
	DeriveOwnerKey OnboardingKeyDeriver
}

// Onboarder moves confirmed tapd assets into a Wavelength boarding output.
type Onboarder struct {
	driver         onboardingDriver
	inventory      proofInventoryClient
	keys           onboardingKeyClient
	store          Store
	signer         tapsdk.AnchorSigner
	funder         OnboardingFunder
	deriveOwnerKey OnboardingKeyDeriver
	mu             sync.Mutex
}

type proofInventoryClient interface {
	VerifyProof(context.Context,
		[]byte) (*tapsdk.VerifyProofResponse, error)

	ListUtxos(context.Context,
		*tapsdk.ListUtxosRequest) (
		map[string]*tapsdk.ManagedUtxo,
		error,
	)
}

// onboardingRecord distinguishes assets sharing one Bitcoin anchor.
type onboardingRecord struct {
	anchor    tapsdk.Outpoint
	issuance  tapsdk.AssetID
	scriptKey tapsdk.PubKey
}

// onboardingKeyClient derives the tapd wallet keys owning asset change.
type onboardingKeyClient interface {
	DeriveScriptKey(context.Context) (*tapsdk.ScriptKey, error)

	DeriveInternalKey(context.Context) (*tapsdk.InternalKey, error)
}

// onboardingChange pins the surplus and its wallet keys so retries produce
// the same split commitment.
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
	FundedPSBT      []byte            `json:"funded_psbt,omitempty"`
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
	if cfg.Funder == nil {
		return nil, fmt.Errorf("taproot asset onboarding funder is " +
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
		funder:         cfg.Funder,
		deriveOwnerKey: cfg.DeriveOwnerKey,
	}, nil
}

// Onboard performs or resumes one idempotent onboarding request. Once the
// commit succeeds, every retry reuses the exact package and final PSBT bytes.
// An unknown commit outcome requires external lease reconciliation; an unknown
// publish outcome is resumed only once tapd records the saved transaction.
func (o *Onboarder) Onboard(ctx context.Context, request *OnboardingRequest) (
	*OnboardingResult, error) {

	if o == nil || o.driver == nil || o.inventory == nil || o.keys == nil ||
		o.store == nil || o.signer == nil || o.funder == nil ||
		o.deriveOwnerKey == nil {
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
	if state.Attempt == onboardingAttemptCommit {
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
		state.FinalAnchorPSBT, err = o.sign(ctx, committed)
		if err != nil {
			return nil, err
		}
		if err := o.driver.VerifyFinalOnboarding(
			state.TransferPackage, state.FinalAnchorPSBT,
		); err != nil {
			return nil, err
		}
		if err := o.storeResult(
			ctx, request.RequestID, state,
		); err != nil {
			return nil, err
		}
	} else if err := o.driver.VerifyFinalOnboarding(
		state.TransferPackage, state.FinalAnchorPSBT,
	); err != nil {
		return nil, fmt.Errorf("restore final onboarding PSBT: %w", err)
	}

	if err := o.publish(ctx, request.RequestID, state); err != nil {
		return nil, err
	}

	return result, nil
}

// publish records completion only after publication or exact reconciliation.
func (o *Onboarder) publish(ctx context.Context, requestID string,
	state *onboardingState) error {

	if state.Published {
		return nil
	}

	var err error
	if state.Attempt == onboardingAttemptPublish {
		err = o.driver.ReconcileOnboarding(ctx, state.FinalAnchorPSBT)
	} else {
		err = o.startPublication(ctx, requestID, state)
	}
	if err != nil {
		return err
	}

	state.Attempt = ""
	state.Published = true

	return o.storeResult(ctx, requestID, state)
}

// startPublication journals the attempt before invoking tapd. An ambiguous
// result retains the marker so retries reconcile instead of publishing again.
func (o *Onboarder) startPublication(ctx context.Context, requestID string,
	state *onboardingState) error {

	state.Attempt = onboardingAttemptPublish
	if err := o.storeState(ctx, requestID, state); err != nil {
		return err
	}
	err := o.driver.Publish(
		ctx, state.TransferPackage, state.FinalAnchorPSBT,
	)
	if err == nil {
		return nil
	}

	var attempted *tapsdk.CustomAnchorPublishAttemptError
	if errors.As(err, &attempted) && attempted.OutcomeUnknown {
		return errors.Join(ErrReconciliationRequired, err)
	}
	state.Attempt = ""

	return errors.Join(err, o.storeResult(ctx, requestID, state))
}

// commit resumes a sealed package or journals a fresh tapd commit.
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

	assetRef, assetInputs, err := o.verifyInputs(ctx, request)
	if err != nil {
		return nil, err
	}
	outputValue := int64(request.CarrierValueSat)
	if outputValue < int64(onboardingDustFloorSat) {
		return nil, fmt.Errorf("onboarding output value %d is below "+
			"the Taproot dust floor", outputValue)
	}

	if err := o.pinChange(
		ctx, request, state, assetInputs,
	); err != nil {
		return nil, err
	}

	inputs := make([]tapsdk.CustomAssetInput, 0, len(assetInputs))
	plans := make(
		[]tapsdk.CustomAnchorInputSigningPlan, 0, len(assetInputs),
	)
	anchors := make([]*tapsdk.ManagedUtxo, 0, len(assetInputs))
	seen := make(map[tapsdk.Outpoint]struct{})
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

		if _, ok := seen[input.anchor.OutPoint]; ok {
			continue
		}
		seen[input.anchor.OutPoint] = struct{}{}

		signer, err := onboardingAnchorSigner(input.anchor)
		if err != nil {
			return nil, err
		}
		plans = append(plans, tapsdk.CustomAnchorInputSigningPlan{
			InputIndex: uint32(len(anchors)),
			KeyPath: &tapsdk.CustomAnchorKeyPathSigningPlan{
				Signer: signer,
			},
		})
		anchors = append(anchors, input.anchor)
	}

	requestDigest := onboardingRequestDigest(request)
	anchor, err := anchorPlan(policy.InternalKey, policyTapLeaves(policy))
	if err != nil {
		return nil, err
	}
	outputs := []tapsdk.CustomAssetOutput{{
		ID:                onboardingOutputID,
		AssetRef:          assetRef,
		Amount:            request.AssetAmount,
		AnchorOutputIndex: 0,
		AnchorValueSat:    uint64(outputValue),
		Script:            boardingScriptPlan(requestDigest),
		Anchor:            anchor,
	}}
	anchorValues := []int64{outputValue}
	if change := state.Change; change != nil {
		outputs = append(outputs, tapsdk.CustomAssetOutput{
			ID:                onboardingChangeID,
			AssetRef:          assetRef,
			Amount:            change.Amount,
			AnchorOutputIndex: onboardingChangeOutputIndex,
			AnchorValueSat:    uint64(change.OutputValueSat),

			// Wallet mode would derive a new key on replay.
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

	funded, err := o.fundedTemplate(
		ctx, request, state, anchors, anchorValues,
	)
	if err != nil {
		return nil, err
	}
	for idx := len(anchors); idx < len(funded.UnsignedTx.TxIn); idx++ {
		plans = append(plans, tapsdk.CustomAnchorInputSigningPlan{
			InputIndex:   uint32(idx),
			CallerSigned: &tapsdk.CustomAnchorCallerSignedPlan{},
		})
	}
	anchorPSBT, err := psbtutil.Serialize(funded)
	if err != nil {
		return nil, err
	}

	requestDTO := &tapsdk.CustomAnchorRequest{
		Inputs:     inputs,
		Outputs:    outputs,
		AnchorPSBT: anchorPSBT,
		Funding:    callerFundedExact(),
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
		ctx, requestDTO,
	)
	if err != nil {
		if !commitOutcomeKnown(err) {
			return nil, fmt.Errorf("onboarding commit for request "+
				"%q: %w", request.RequestID,
				errors.Join(ErrReconciliationRequired, err))
		}

		// tapd sealed nothing, so a retry funds afresh.
		err = errors.Join(
			err,
			o.releaseInputs(
				ctx, onboardingLockID(requestDigest), funded,
				len(anchors),
			),
		)
		state.Attempt = ""
		state.FundedPSBT = nil
		if storeErr := o.storeResult(
			ctx, request.RequestID, state,
		); storeErr != nil {
			return nil, errors.Join(err, storeErr)
		}

		return nil, err
	}

	state.Attempt = ""
	state.TransferPackage = append([]byte(nil), committed.packageBytes...)
	if err := o.storeResult(ctx, request.RequestID, state); err != nil {
		return nil, err
	}

	return committed, nil
}

// fundedTemplate returns the onboarding transaction tapd commits to: tapd's
// anchors and then the funder's inputs, the asset outputs and then the
// funder's change. A journaled template is reused with its leases renewed, so
// a retry spends the same coins.
func (o *Onboarder) fundedTemplate(ctx context.Context,
	request *OnboardingRequest, state *onboardingState,
	anchors []*tapsdk.ManagedUtxo, values []int64) (*psbt.Packet, error) {

	lockID := onboardingLockID(onboardingRequestDigest(request))
	if len(state.FundedPSBT) != 0 {
		packet, err := psbtutil.Parse(state.FundedPSBT)
		if err == nil {
			err = checkFundedTemplate(packet, anchors, values)
		}
		if err != nil {
			return nil, fmt.Errorf("restore funded onboarding: %w",
				err)
		}
		for _, txIn := range packet.UnsignedTx.TxIn[len(anchors):] {
			_, err := o.funder.LeaseOutput(
				ctx, lockID, txIn.PreviousOutPoint,
				onboardingLeaseExpiry,
			)
			if err != nil {
				return nil, fmt.Errorf("renew onboarding "+
					"input lease: %w", err)
			}
		}

		return packet, nil
	}

	feeRate, err := o.feeRate(ctx, request)
	if err != nil {
		return nil, err
	}

	// The funder sees only the outputs, so the anchors' value, less the
	// fee for the weight they add, comes off the boarding output first.
	var anchorValue int64
	for _, anchor := range anchors {
		anchorValue += anchor.AmtSat
	}
	anchorWeight := int64(len(anchors)) * onboardingAnchorInputWeight
	if feeRate > uint64(math.MaxInt64/(4*anchorWeight)) {
		return nil, fmt.Errorf("onboarding fee rate %d sat/vB is "+
			"too high", feeRate)
	}
	anchorFee := (int64(feeRate)*anchorWeight + 3) / 4
	template := append([]int64(nil), values...)
	template[0] -= anchorValue - anchorFee
	if template[0] < int64(onboardingDustFloorSat) {
		return nil, fmt.Errorf("tapd anchors carry %d sat, more than "+
			"the onboarding output needs", anchorValue)
	}

	packet, err := onboardingOutputsTemplate(template)
	if err != nil {
		return nil, err
	}
	changeIndex, err := o.funder.FundTemplate(
		ctx, packet, int64(feeRate), lockID, onboardingLeaseExpiry,
	)
	if err != nil {
		return nil, fmt.Errorf("fund onboarding: %w", err)
	}

	funded, err := arrangeFundedTemplate(
		packet, changeIndex, anchors, values,
	)
	if err == nil {
		err = checkOnboardingFee(funded, anchors, request.MaxFeeSat)
	}
	if err == nil {
		state.FundedPSBT, err = psbtutil.Serialize(funded)
	}
	if err == nil {
		err = o.storeState(ctx, request.RequestID, state)
	}
	if err != nil {
		state.FundedPSBT = nil

		return nil, errors.Join(
			err, o.releaseInputs(ctx, lockID, packet, 0),
		)
	}

	return funded, nil
}

// feeRate resolves the request's fee policy to a rate in sat/vB.
func (o *Onboarder) feeRate(ctx context.Context, request *OnboardingRequest) (
	uint64, error) {

	if request.FeeRateSatPerVByte != 0 {
		return request.FeeRateSatPerVByte, nil
	}

	rate, err := o.funder.FeeRate(ctx, request.TargetConf)
	if err != nil {
		return 0, fmt.Errorf("estimate onboarding fee rate: %w", err)
	}
	if rate == 0 {
		return 0, fmt.Errorf("onboarding fee estimate is zero")
	}
	if err := checkOnboardingFeeRate(rate); err != nil {
		return 0, err
	}

	return rate, nil
}

// releaseInputs drops the funder's leases on a template's inputs, skipping
// the leading anchors.
func (o *Onboarder) releaseInputs(ctx context.Context, lockID walletcore.LockID,
	packet *psbt.Packet, anchors int) error {

	var errs error
	for _, txIn := range packet.UnsignedTx.TxIn[anchors:] {
		errs = errors.Join(
			errs, o.funder.ReleaseOutput(
				ctx, lockID, txIn.PreviousOutPoint,
			),
		)
	}

	return errs
}

// sign has the anchor signer finalize tapd's anchor inputs, then the funder
// sign its own. The funder goes last, because its wallet finalizes the whole
// transaction.
func (o *Onboarder) sign(ctx context.Context, committed *commitResult) ([]byte,
	error) {

	signed, err := o.signer(ctx, committed.anchorPSBT)
	if err != nil {
		return nil, fmt.Errorf("sign onboarding anchors: %w", err)
	}
	packet, err := psbtutil.Parse(signed)
	if err != nil {
		return nil, err
	}
	anchors := make(map[wire.OutPoint]struct{}, len(committed.inputs))
	for _, input := range committed.inputs {
		anchors[wire.OutPoint{
			Hash:  chainhash.Hash(input.anchorOutpoint.Txid),
			Index: input.anchorOutpoint.Index,
		}] = struct{}{}
	}
	for idx, txIn := range packet.UnsignedTx.TxIn {
		_, isAnchor := anchors[txIn.PreviousOutPoint]
		if isAnchor && len(packet.Inputs[idx].FinalScriptWitness) == 0 {
			return nil, fmt.Errorf("onboarding signer left anchor "+
				"input %d unfinalized", idx)
		}
	}

	finalTx, err := o.funder.FinalizePsbt(ctx, signed)
	if err != nil {
		return nil, fmt.Errorf("sign onboarding wallet inputs: %w", err)
	}
	if finalTx.TxHash() != packet.UnsignedTx.TxHash() {
		return nil, fmt.Errorf("onboarding funder signed another " +
			"transaction")
	}
	for idx := range packet.Inputs {
		if len(packet.Inputs[idx].FinalScriptWitness) != 0 {
			continue
		}
		var witness bytes.Buffer
		err := psbt.WriteTxWitness(&witness, finalTx.TxIn[idx].Witness)
		if err != nil {
			return nil, err
		}
		packet.Inputs[idx].FinalScriptWitness = witness.Bytes()
	}
	if _, err := psbt.Extract(packet); err != nil {
		return nil, fmt.Errorf("onboarding transaction is not "+
			"final: %w", err)
	}

	return psbtutil.Serialize(packet)
}

// onboardingOutputsTemplate builds an outputs-only template whose outputs pay
// a placeholder script. tapd replaces the asset outputs' scripts at commit.
func onboardingOutputsTemplate(values []int64) (*psbt.Packet, error) {
	placeholder, err := onboardingPlaceholderScript()
	if err != nil {
		return nil, err
	}

	tx := wire.NewMsgTx(2)
	for _, value := range values {
		tx.AddTxOut(&wire.TxOut{
			Value:    value,
			PkScript: placeholder,
		})
	}

	return psbt.NewFromUnsignedTx(tx)
}

// onboardingPlaceholderScript is the script of an asset output before tapd
// commits to its assets.
func onboardingPlaceholderScript() ([]byte, error) {
	key := txscript.ComputeTaprootKeyNoScript(&arkscript.ARKNUMSKey)

	return txscript.PayToTaprootScript(key)
}

// arrangeFundedTemplate lays a funded outputs-only template out for tapd:
// the anchors ahead of the funder's inputs, and the asset outputs at their
// full values ahead of the funder's change. The funder may reorder what it
// funds, so only its inputs and change are taken from the template.
func arrangeFundedTemplate(funded *psbt.Packet, changeIndex int32,
	anchors []*tapsdk.ManagedUtxo, values []int64) (*psbt.Packet, error) {

	placeholder, err := onboardingPlaceholderScript()
	if err != nil {
		return nil, err
	}
	fundedTx := funded.UnsignedTx
	wantOutputs := len(values)
	if changeIndex >= 0 {
		wantOutputs++
	}
	if len(fundedTx.TxOut) != wantOutputs ||
		changeIndex >= int32(len(fundedTx.TxOut)) {
		return nil, fmt.Errorf("funder changed the onboarding outputs")
	}
	for idx, txOut := range fundedTx.TxOut {
		if int32(idx) != changeIndex &&
			!bytes.Equal(txOut.PkScript, placeholder) {
			return nil, fmt.Errorf("funder changed the " +
				"onboarding outputs")
		}
	}
	if len(fundedTx.TxIn) == 0 {
		return nil, fmt.Errorf("funder added no inputs")
	}

	tx := wire.NewMsgTx(fundedTx.Version)
	tx.LockTime = fundedTx.LockTime
	inputs := make([]psbt.PInput, 0, len(anchors)+len(fundedTx.TxIn))
	for _, anchor := range anchors {
		tx.AddTxIn(
			wire.NewTxIn(
				&wire.OutPoint{
					Hash: chainhash.Hash(
						anchor.OutPoint.Txid,
					),
					Index: anchor.OutPoint.Index,
				},
				nil,
				nil,
			),
		)
		inputs = append(inputs, psbt.PInput{})
	}
	for idx, txIn := range fundedTx.TxIn {
		if funded.Inputs[idx].WitnessUtxo == nil {
			return nil, fmt.Errorf("funder input %d has no "+
				"witness UTXO", idx)
		}
		tx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: txIn.PreviousOutPoint,
			Sequence:         txIn.Sequence,
		})
		inputs = append(inputs, funded.Inputs[idx])
	}

	outputs := make([]psbt.POutput, 0, wantOutputs)
	for _, value := range values {
		tx.AddTxOut(&wire.TxOut{
			Value:    value,
			PkScript: placeholder,
		})
		outputs = append(outputs, psbt.POutput{})
	}
	if changeIndex >= 0 {
		change := fundedTx.TxOut[changeIndex]
		tx.AddTxOut(&wire.TxOut{
			Value:    change.Value,
			PkScript: append([]byte(nil), change.PkScript...),
		})
		outputs = append(outputs, funded.Outputs[changeIndex])
	}

	packet, err := psbt.NewFromUnsignedTx(tx)
	if err != nil {
		return nil, err
	}
	packet.Inputs = inputs
	packet.Outputs = outputs

	return packet, nil
}

// checkFundedTemplate binds a journaled template to the anchors and output
// values it was funded for.
func checkFundedTemplate(packet *psbt.Packet, anchors []*tapsdk.ManagedUtxo,
	values []int64) error {

	tx := packet.UnsignedTx
	if len(tx.TxIn) <= len(anchors) || len(tx.TxOut) < len(values) {
		return fmt.Errorf("template shape mismatch")
	}
	for idx, anchor := range anchors {
		want := wire.OutPoint{
			Hash:  chainhash.Hash(anchor.OutPoint.Txid),
			Index: anchor.OutPoint.Index,
		}
		if tx.TxIn[idx].PreviousOutPoint != want {
			return fmt.Errorf("template input %d is not anchor %s",
				idx, want)
		}
	}
	for idx, value := range values {
		if tx.TxOut[idx].Value != value {
			return fmt.Errorf("template output %d value "+
				"%d, want %d", idx, tx.TxOut[idx].Value, value)
		}
	}

	return nil
}

// checkOnboardingFee bounds the fee a funded template pays.
func checkOnboardingFee(packet *psbt.Packet, anchors []*tapsdk.ManagedUtxo,
	maxFeeSat uint64) error {

	var fee int64
	for _, anchor := range anchors {
		fee += anchor.AmtSat
	}
	for _, input := range packet.Inputs[len(anchors):] {
		fee += input.WitnessUtxo.Value
	}
	for _, output := range packet.UnsignedTx.TxOut {
		fee -= output.Value
	}
	switch {
	case fee < 0:
		return fmt.Errorf("onboarding outputs exceed its inputs "+
			"by %d sat", -fee)

	case uint64(fee) > maxFeeSat:
		return fmt.Errorf("onboarding fee %d exceeds maximum %d", fee,
			maxFeeSat)
	}

	return nil
}

// onboardingAssetInput identifies one verified asset record and its anchor.
type onboardingAssetInput struct {
	proofFile []byte
	anchor    *tapsdk.ManagedUtxo
	amount    uint64
}

// verifyInputs matches proofs to tapd inventory and requires complete
// selection of each spent anchor from one issuance.
func (o *Onboarder) verifyInputs(ctx context.Context,
	request *OnboardingRequest) (tapsdk.AssetRef, []onboardingAssetInput,
	error) {

	assetRef, err := tapsdk.ParseAssetRef(request.AssetRef)
	if err != nil {
		return "", nil,
			fmt.Errorf("parse Taproot Asset ref: %w", err)
	}

	utxos, err := o.inventory.ListUtxos(ctx, &tapsdk.ListUtxosRequest{
		ScriptKeyType: &tapsdk.ScriptKeyTypeQuery{AllTypes: true},
	})
	if err != nil {
		return "", nil,
			fmt.Errorf("list tapd onboarding inventory: %w", err)
	}

	proofs := sortedProofFiles(request)
	if len(proofs) == 0 {
		return "", nil,
			fmt.Errorf("onboarding funding proof is required")
	}
	var (
		inputs   = make([]onboardingAssetInput, 0, len(proofs))
		selected = make(map[onboardingRecord]struct{}, len(proofs))
		issuance tapsdk.AssetID
	)
	for idx, proofFile := range proofs {
		verified, err := o.inventory.VerifyProof(ctx, proofFile)
		if err != nil {
			return "", nil, fmt.Errorf("verify onboarding proof "+
				"%d with tapd: %w", idx, err)
		}
		if verified == nil || !verified.Valid ||
			verified.DecodedProof == nil {
			return "", nil, fmt.Errorf("tapd rejected onboarding "+
				"proof %d", idx)
		}
		tip := verified.DecodedProof
		if !tip.AssetRef.Equivalent(assetRef) || tip.Amount == 0 {
			return "", nil, fmt.Errorf("onboarding proof %d tip "+
				"does not match request", idx)
		}

		id := onboardingRecord{
			anchor:    tip.Outpoint,
			issuance:  tip.IssuanceID,
			scriptKey: tip.ScriptKey,
		}
		if _, ok := selected[id]; ok {
			return "", nil, fmt.Errorf("onboarding proof %d "+
				"selects asset record twice", idx)
		}
		selected[id] = struct{}{}
		if idx > 0 && issuance != tip.IssuanceID {
			return "", nil, fmt.Errorf("onboarding requires " +
				"funding from one issuance")
		}
		issuance = tip.IssuanceID

		var anchor *tapsdk.ManagedUtxo
		for _, candidate := range utxos {
			if candidate != nil &&
				candidate.OutPoint == tip.Outpoint {

				anchor = candidate

				break
			}
		}
		if anchor == nil {
			return "", nil, fmt.Errorf("onboarding proof %d "+
				"anchor is not managed by tapd", idx)
		}
		var matches int
		for _, asset := range anchor.Assets {
			if asset != nil &&
				asset.Genesis.IssuanceID == tip.IssuanceID &&
				asset.Amount == tip.Amount &&
				asset.ScriptKey.PubKey == tip.ScriptKey {

				matches++
			}
		}
		if matches != 1 {
			return "", nil, fmt.Errorf("tapd onboarding inventory "+
				"does not match proof %d", idx)
		}

		inputs = append(inputs, onboardingAssetInput{
			proofFile: proofFile,
			anchor:    anchor,
			amount:    tip.Amount,
		})
	}

	// Spending a Bitcoin anchor consumes every asset record on it. Require
	// a proof for each record before committing, including shared anchors.
	for _, input := range inputs {
		for _, asset := range input.anchor.Assets {
			if asset == nil {
				return "", nil, fmt.Errorf("onboarding " +
					"funding inventory is incomplete")
			}
			id := onboardingRecord{
				anchor:    input.anchor.OutPoint,
				issuance:  asset.Genesis.IssuanceID,
				scriptKey: asset.ScriptKey.PubKey,
			}
			if _, ok := selected[id]; !ok {
				return "", nil, fmt.Errorf("onboarding " +
					"requires a proof for every asset on " +
					"each selected anchor")
			}
		}
	}

	return assetRef, inputs, nil
}

// pinChange persists wallet keys for any surplus before committing.
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

// onboardingResultFromCommit binds the sealed output to its policy and value.
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
	if committed.fundingMode !=
		tapsdk.CustomAnchorFundingCallerFundedExact {
		return nil, fmt.Errorf("onboarding package is not caller " +
			"funded")
	}
	if committed.actualFeeSat > request.MaxFeeSat {
		return nil, fmt.Errorf("onboarding package actual fee %d "+
			"exceeds maximum %d", committed.actualFeeSat,
			request.MaxFeeSat)
	}
	assetRef, err := tapsdk.ParseAssetRef(request.AssetRef)
	if err != nil {
		return nil, err
	}
	output, err := committedOutput(committed, onboardingOutputID)
	if err != nil {
		return nil, err
	}

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

// checkOnboardingChange binds the committed change to its saved amount,
// output index, and wallet keys.
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

// loadState restores the request identity and pinned owner key.
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
	if state.Attempt != "" && state.Attempt != onboardingAttemptCommit &&
		state.Attempt != onboardingAttemptPublish {
		return nil, fmt.Errorf("invalid taproot asset onboarding "+
			"attempt %q", state.Attempt)
	}

	return &state, nil
}

// storeResult records completed side effects even if the caller cancelled
// while waiting, so a retry retains the known outcome and saved artifacts.
func (o *Onboarder) storeResult(ctx context.Context, requestID string,
	state *onboardingState) error {

	journalCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx), commitJournalWriteTimeout,
	)
	defer cancel()

	return o.storeState(journalCtx, requestID, state)
}

// storeState atomically replaces the onboarding journal.
func (o *Onboarder) storeState(ctx context.Context, requestID string,
	state *onboardingState) error {

	encoded, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode taproot asset onboarding state: %w",
			err)
	}

	return o.store.Store(ctx, onboardingStorePrefix+requestID, encoded)
}

// validateOnboardingRequest checks economics before deriving wallet keys.
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
	if _, err := tapsdk.ParseAssetRef(request.AssetRef); err != nil {
		return fmt.Errorf("parse Taproot Asset ref: %w", err)
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
	if err := checkOnboardingFeePolicy(request); err != nil {
		return err
	}
	if request.OperatorKey == nil || request.ExitDelay == 0 {
		return fmt.Errorf("taproot asset onboarding operator policy " +
			"is required")
	}

	return nil
}

// onboardingRequestDigest binds retries to the same request, regardless of
// proof ordering.
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

// checkOnboardingFeePolicy requires exactly one of a fee rate and a
// confirmation target.
func checkOnboardingFeePolicy(request *OnboardingRequest) error {
	if (request.FeeRateSatPerVByte != 0) == (request.TargetConf != 0) {
		return fmt.Errorf("taproot asset onboarding requires exactly " +
			"one of fee rate and target confirmation")
	}
	if request.FeeRateSatPerVByte != 0 {
		return checkOnboardingFeeRate(request.FeeRateSatPerVByte)
	}

	return nil
}

// checkOnboardingFeeRate bounds a fee rate to what a transaction fee can
// express.
func checkOnboardingFeeRate(satPerVByte uint64) error {
	if _, err := tapsdk.NewFeeRateSatPerVByte(satPerVByte); err != nil {
		return fmt.Errorf("taproot asset onboarding fee rate: %w", err)
	}

	return nil
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

// onboardingLockID scopes the funder's input leases to the request.
func onboardingLockID(requestDigest tapsdk.Hash) walletcore.LockID {
	var value bytes.Buffer
	writeDigestBytes(&value, []byte(onboardingLockIDDomain))
	writeDigestBytes(&value, requestDigest[:])

	return sha256.Sum256(value.Bytes())
}

// ownerKeyFromState restores the pinned Bitcoin owner descriptor.
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

// policyTapLeaves preserves the standard policy in the composed output.
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

// boardingScriptPlan uses OP_TRUE for operator-built asset transitions.
// Custody stays with the owner through the composed Bitcoin spending policy.
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
