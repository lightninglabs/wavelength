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
	"strings"
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
	preparationStateVersion = uint16(3)
	attemptCheckpoint       = "checkpoint"
	attemptArk              = "ark"
	opTrueKeyDomain         = "wavelength/taproot-assets-oor/optrue/v0/"
	maxOrderingNonces       = 256
	maxOrderingIterations   = 16
	witnessBackendSigner    = tapsdk.CustomAssetWitnessBackendSigner
	witnessCallerProvided   = tapsdk.CustomAssetWitnessCallerProvided
	scriptExternal          = tapsdk.CustomAssetScriptExternal
)

// ErrReconciliationRequired reports a commit attempt whose durable outcome is
// unknown. Retrying it could create a competing Taproot Asset transition.
// It aliases the OOR-layer sentinel so the RPC boundary can preserve input
// reservations without depending on this concrete adapter package.
var ErrReconciliationRequired = oor.ErrTaprootAssetCommitOutcomeUnknown

// CreatedPackageLoader returns the sealed Ark package that created an exact
// local asset VTXO. It is injected by waved without exposing database types to
// tapassets or tap-sdk types to the daemon RPC layer.
type CreatedPackageLoader func(context.Context, wire.OutPoint) ([]byte, error)

// PreparerConfig contains the dependencies of the concrete tap-sdk adapter.
type PreparerConfig struct {
	Wallet             *tapsdk.Wallet
	Store              Store
	ReservationStore   oor.ReservationSetStore
	LoadCreatedPackage CreatedPackageLoader
}

// proofPathVerifier is the preflight seam running tap-sdk's complete path
// verification. It exists as a function value so graph-orchestration tests
// can fake the SDK boundary without real proof material.
type proofPathVerifier func(context.Context, *tapsdk.AssetProofPath,
	tapsdk.ConfirmedProofVerifier) error

// Preparer commits the checkpoint and Ark asset transitions before handing a
// sealed, immutable package to Wavelength's outgoing OOR actor.
type Preparer struct {
	driver             customAnchorDriver
	inventory          proofInventoryClient
	store              Store
	reservations       oor.ReservationSetStore
	loadCreatedPackage CreatedPackageLoader
	verifyProofPath    proofPathVerifier
	mu                 sync.Mutex
}

type preparationState struct {
	Version        uint16          `json:"version"`
	IntentDigest   tapsdk.Hash     `json:"intent_digest"`
	RequestDigest  tapsdk.Hash     `json:"request_digest"`
	InputOutpoints []wire.OutPoint `json:"input_outpoints"`
	Attempt        string          `json:"attempt,omitempty"`

	// CheckpointPackages holds one sealed checkpoint package per asset
	// input, indexed by the intent's spine-first input order.
	CheckpointPackages [][]byte `json:"checkpoint_packages,omitempty"` //nolint:ll

	ArkPackage        []byte                  `json:"ark_package,omitempty"`
	PlannedRecipients []oortx.RecipientOutput `json:"planned_recipients,omitempty"` //nolint:ll
	OrderingNonce     uint32                  `json:"ordering_nonce,omitempty"`     //nolint:ll
	CarrierLease      *oor.OORCarrierLease    `json:"carrier_lease,omitempty"`      //nolint:ll
}

// hasCommittedPackages reports whether any external tapd commit is journaled.
func (s *preparationState) hasCommittedPackages() bool {
	if len(s.ArkPackage) != 0 {
		return true
	}
	for _, checkpointPackage := range s.CheckpointPackages {
		if len(checkpointPackage) != 0 {
			return true
		}
	}

	return false
}

// checkpointAttempt returns the journal marker and deterministic key domain
// of one asset input's checkpoint commit.
func checkpointAttempt(ordinal int) string {
	return fmt.Sprintf("%s/%d", attemptCheckpoint, ordinal)
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
	if cfg.ReservationStore == nil {
		return nil, fmt.Errorf("taproot asset reservation store is " +
			"required")
	}

	return &Preparer{
		driver: &sdkDriver{
			wallet: cfg.Wallet,
		},
		inventory:          cfg.Wallet.Client(),
		store:              cfg.Store,
		reservations:       cfg.ReservationStore,
		loadCreatedPackage: cfg.LoadCreatedPackage,
		verifyProofPath:    verifyAssetProofPath,
	}, nil
}

// verifyAssetProofPath runs tap-sdk's complete compact-path verification.
func verifyAssetProofPath(ctx context.Context, path *tapsdk.AssetProofPath,
	verifier tapsdk.ConfirmedProofVerifier) error {

	_, err := path.Verify(ctx, verifier)

	return err
}

// PrepareTaprootAssetOOR implements oor.TaprootAssetOORPreparer. It commits one
// asset checkpoint beside zero or more Bitcoin checkpoints and then commits the
// split receiver/change transition into the canonically ordered Ark outputs.
func (p *Preparer) PrepareTaprootAssetOOR(ctx context.Context,
	request *oor.TaprootAssetOORPrepareRequest) (
	*oor.TaprootAssetOORPreparation, error) {

	if p == nil || p.driver == nil || p.inventory == nil ||
		p.store == nil ||
		p.reservations == nil {
		return nil, fmt.Errorf("taproot asset preparer is not " +
			"configured")
	}
	if err := request.Validate(); err != nil {
		return nil, err
	}
	assetInputIndices, err := request.AssetInputIndices()
	if err != nil {
		return nil, err
	}
	for idx := range request.Inputs {
		if request.Inputs[idx].CustomSpend != nil ||
			len(request.Inputs[idx].ExternalSignatures) != 0 {
			return nil, fmt.Errorf("Taproot Asset OOR input %d "+
				"must be a standard VTXO", idx)
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	digest, err := preparationRequestDigest(request)
	if err != nil {
		return nil, err
	}
	intentDigest, err := preparationIntentDigest(
		&oor.TaprootAssetOORResumeRequest{
			RequestID:   request.RequestID,
			Policy:      request.Policy,
			Recipients:  request.Recipients,
			OutputFloor: request.OutputFloor,
			Intent:      request.Intent,
		},
	)
	if err != nil {
		return nil, err
	}
	// The float input is not a wallet VTXO: it must never enter the
	// wallet-side reservation journal, which precondition-checks every
	// outpoint against a local Spending row.
	state, err := p.loadState(
		ctx, request.RequestID, digest, intentDigest,
		oor.WalletInputOutpoints(request.Inputs),
	)
	if err != nil {
		return nil, err
	}
	if state.Attempt != "" {
		return nil, fmt.Errorf("%w: %s commit for request %q",
			ErrReconciliationRequired, state.Attempt,
			request.RequestID)
	}
	if err := p.journalCarrierLease(ctx, request, state); err != nil {
		return nil, err
	}
	recipients, err := p.plannedRecipients(ctx, request, state)
	if err != nil {
		return nil, err
	}
	assetRef, err := tapsdk.ParseAssetRef(request.Intent.AssetRef)
	if err != nil {
		return nil, fmt.Errorf("parse Taproot Asset ref: %w", err)
	}

	// A journaled Ark package implies every checkpoint package exists, so
	// nothing needs the spend sources on a pure restore.
	var sources []*assetSpendSource
	if len(state.ArkPackage) == 0 {
		sources = make([]*assetSpendSource, len(assetInputIndices))
		for ordinal, inputIndex := range assetInputIndices {
			source, err := p.resolveAssetSpendSource(
				ctx, request, inputIndex, assetRef,
			)
			if err != nil {
				return nil, err
			}
			err = source.validateTransitionCapacity()
			if err != nil {
				return nil, err
			}
			if err := p.verifySpendSource(ctx, source); err != nil {
				return nil, err
			}
			sources[ordinal] = source
		}
	}

	if err := p.ensureReservationSet(
		ctx, request.RequestID, state,
	); err != nil {
		return nil, err
	}

	checkpoints, artifacts, checkpointResults, err := p.prepareCheckpoints(
		ctx, request, assetInputIndices, assetRef, digest, sources,
		state,
	)
	if err != nil {
		return nil, retainedPreparationError(request.RequestID, err)
	}

	ark, arkResult, recipients, err := p.prepareMixedArk(
		ctx, request, assetInputIndices, assetRef, checkpoints,
		artifacts, checkpointResults, recipients, digest, sources,
		state,
	)
	if err != nil {
		return nil, retainedPreparationError(request.RequestID, err)
	}

	checkpointPackages := make([][]byte, len(checkpoints))
	for ordinal, inputIndex := range assetInputIndices {
		checkpointPackages[inputIndex] = append(
			[]byte(nil), checkpointResults[ordinal].packageBytes...,
		)
	}
	assetTransfer := &oortx.TaprootAssetTransfer{
		Version:            oortx.TaprootAssetTransferVersion,
		CheckpointPackages: checkpointPackages,
		ArkPackage: append(
			[]byte(nil), arkResult.packageBytes...,
		),
	}
	prepared := &oor.TaprootAssetOORPreparation{
		PreparedSubmit: &oor.PreparedSubmitPackage{
			ArkPSBT:              ark,
			CheckpointPSBTs:      checkpoints,
			TaprootAssetTransfer: assetTransfer,
		},
		Recipients: recipients,
		Receiver:   recipients[0],
	}
	if err := prepared.Validate(request); err != nil {
		return nil, retainedPreparationError(
			request.RequestID, fmt.Errorf("validate prepared "+
				"Taproot Asset OOR: %w", err),
		)
	}

	return prepared, nil
}

// journalCarrierLease adopts the request's carrier lease into the durable
// state on first sight and afterwards holds the journal authoritative: an
// idempotent replay must reuse the exact float funding the graph committed
// to, never a fresh lease.
func (p *Preparer) journalCarrierLease(ctx context.Context,
	request *oor.TaprootAssetOORPrepareRequest,
	state *preparationState) error {

	if state.CarrierLease != nil {
		if !state.CarrierLease.FundingEquals(request.Lease) {
			return fmt.Errorf("Taproot Asset OOR idempotency key " +
				"reused with a different carrier lease")
		}

		return nil
	}

	// A committed journal without a lease predates carrier funding and
	// cannot be adopted safely.
	if state.hasCommittedPackages() {
		return fmt.Errorf("%w: committed request %q has no journaled "+
			"carrier lease", ErrReconciliationRequired,
			request.RequestID)
	}

	state.CarrierLease = request.Lease.Clone()
	if err := p.storeState(ctx, request.RequestID, state); err != nil {
		state.CarrierLease = nil

		return fmt.Errorf("persist Taproot Asset carrier lease: %w",
			err)
	}

	return nil
}

// ensureReservationSet either atomically acquires/revalidates preparation
// ownership, or accepts the one valid handoff to the already-admitted OOR
// session derived from the sealed Ark package. It never steals another owner.
func (p *Preparer) ensureReservationSet(ctx context.Context, requestID string,
	state *preparationState) error {

	outpoints := cloneOutpoints(state.InputOutpoints)
	ownerID := oor.TaprootAssetPreparationReservationOwner(requestID)
	reservationState, err := p.reservations.InspectReservationSet(
		ctx, outpoints, oor.ReservationOwnerKindTaprootAssetPreparation,
		ownerID,
	)
	if err != nil {
		return fmt.Errorf("%w: inspect Taproot Asset input "+
			"reservations for request %q: %v",
			ErrReconciliationRequired, requestID, err)
	}
	if reservationState == oor.ReservationSetInconsistent &&
		len(state.ArkPackage) != 0 {

		reservationState, err = p.inspectOORReservationHandoff(
			ctx, state,
		)
		if err != nil {
			return fmt.Errorf("%w: inspect Taproot Asset OOR "+
				"reservation handoff for request %q: %v",
				ErrReconciliationRequired, requestID, err)
		}
		if reservationState == oor.ReservationSetOwned {
			return nil
		}
	}
	if reservationState == oor.ReservationSetInconsistent {
		return fmt.Errorf("%w: input reservation ownership for "+
			"request %q is inconsistent", ErrReconciliationRequired,
			requestID)
	}

	// This write is also an atomic lifecycle precondition check in the
	// production store. Repeating it for an already-owned set closes the
	// window where an exit could remove the reservation before tapd work.
	if err := p.reservations.UpsertReservationSet(
		ctx, outpoints, oor.ReservationOwnerKindTaprootAssetPreparation,
		ownerID,
	); err != nil {
		return fmt.Errorf("%w: atomically reserve Taproot Asset "+
			"inputs for request %q: %v", ErrReconciliationRequired,
			requestID, err)
	}

	return nil
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
		if !commitOutcomeKnown(err) {
			return nil, fmt.Errorf("%w: %s commit for request "+
				"%q: %w", ErrReconciliationRequired, attempt,
				requestID, err)
		}

		state.Attempt = ""
		if storeErr := p.storeState(
			ctx, requestID, state,
		); storeErr != nil {

			state.Attempt = attempt

			return nil, fmt.Errorf("%w; clear commit intent: %w",
				err, storeErr)
		}

		return nil, err
	}

	return result, nil
}

// retainedPreparationError marks every failure after atomic reservation-set
// acquisition as unsafe to release. Retaining the complete set avoids a crash
// halfway through per-VTXO cleanup from producing a partial, unresumable set.
func retainedPreparationError(requestID string, err error) error {
	if err == nil || errors.Is(err, ErrReconciliationRequired) {
		return err
	}

	return fmt.Errorf("%w: request %q owns a durable Taproot Asset input "+
		"reservation set: %w", ErrReconciliationRequired, requestID,
		err)
}

// ResumeTaprootAssetOOR returns the exact carrier input set for a preparation
// that crossed the first tapd commit boundary. The RPC layer can then adopt
// the already-Spending VTXOs without repeating wallet selection.
func (p *Preparer) ResumeTaprootAssetOOR(ctx context.Context,
	request *oor.TaprootAssetOORResumeRequest) (*oor.TaprootAssetOORResume,
	error) {

	if p == nil || p.store == nil || p.reservations == nil {
		return nil, fmt.Errorf("taproot asset preparation store is " +
			"not configured")
	}
	digest, err := preparationIntentDigest(request)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	encoded, err := p.store.Load(ctx, request.RequestID)
	if errors.Is(err, ErrStoreNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	state, err := decodePreparationState(encoded)
	if err != nil {
		return nil, err
	}
	if state.IntentDigest != digest {
		return nil, fmt.Errorf("Taproot Asset OOR idempotency key " +
			"reused with different request")
	}
	reservationOwner := oor.TaprootAssetPreparationReservationOwner(
		request.RequestID,
	)
	reservationState, err := p.reservations.InspectReservationSet(
		ctx, state.InputOutpoints,
		oor.ReservationOwnerKindTaprootAssetPreparation,
		reservationOwner,
	)
	if err != nil {
		return nil, fmt.Errorf("inspect Taproot Asset input "+
			"reservations: %w", err)
	}
	if reservationState == oor.ReservationSetInconsistent &&
		len(state.ArkPackage) != 0 {

		reservationState, err = p.inspectOORReservationHandoff(
			ctx, state,
		)
		if err != nil {
			return nil, fmt.Errorf("inspect Taproot Asset OOR "+
				"reservation handoff: %w", err)
		}
	}
	if reservationState == oor.ReservationSetInconsistent {
		return nil, fmt.Errorf("%w: input reservation ownership for "+
			"request %q is inconsistent", ErrReconciliationRequired,
			request.RequestID)
	}
	if state.Attempt != "" {
		return nil, fmt.Errorf("%w: %s commit for request %q",
			ErrReconciliationRequired, state.Attempt,
			request.RequestID)
	}
	if reservationState == oor.ReservationSetAbsent {
		if state.hasCommittedPackages() {
			return nil, fmt.Errorf("%w: committed request %q has "+
				"no owned input reservations",
				ErrReconciliationRequired, request.RequestID)
		}

		return nil, nil
	}

	return &oor.TaprootAssetOORResume{
		InputOutpoints: cloneOutpoints(state.InputOutpoints),
		Lease:          state.CarrierLease.Clone(),
	}, nil
}

// inspectOORReservationHandoff accepts the one intentional ownership change:
// once the durable OOR actor admits the sealed Ark package, it owns the same
// complete set under the deterministic Ark txid/session ID. Any partial or
// foreign handoff remains inconsistent.
func (p *Preparer) inspectOORReservationHandoff(ctx context.Context,
	state *preparationState) (oor.ReservationSetState, error) {

	if p.driver == nil || state == nil || len(state.ArkPackage) == 0 {
		return oor.ReservationSetInconsistent, nil
	}
	result, err := p.driver.DecodePackage(state.ArkPackage)
	if err != nil {
		return oor.ReservationSetInconsistent, err
	}
	ark, err := psbtutil.Parse(result.anchorPSBT)
	if err != nil {
		return oor.ReservationSetInconsistent, err
	}
	if ark == nil || ark.UnsignedTx == nil {
		return oor.ReservationSetInconsistent,
			fmt.Errorf("sealed Ark transaction is missing")
	}
	ownerID := ark.UnsignedTx.TxHash()

	return p.reservations.InspectReservationSet(
		ctx, state.InputOutpoints, oor.ReservationOwnerKindOOROutgoing,
		ownerID,
	)
}

// loadState restores and validates the durable state for one request. Before
// the first external commit, the same public intent may bind a newly selected
// Bitcoin carrier set after a known-negative failure. Once a checkpoint could
// exist, the exact input set is immutable.
func (p *Preparer) loadState(ctx context.Context, requestID string,
	digest, intentDigest tapsdk.Hash, inputOutpoints []wire.OutPoint) (
	*preparationState, error) {

	encoded, err := p.store.Load(ctx, requestID)
	if errors.Is(err, ErrStoreNotFound) {
		state := &preparationState{
			Version:        preparationStateVersion,
			IntentDigest:   intentDigest,
			RequestDigest:  digest,
			InputOutpoints: cloneOutpoints(inputOutpoints),
		}
		if err := p.storeState(ctx, requestID, state); err != nil {
			return nil, err
		}

		return state, nil
	}
	if err != nil {
		return nil, err
	}

	state, err := decodePreparationState(encoded)
	if err != nil {
		return nil, err
	}
	if state.IntentDigest != intentDigest {
		return nil, fmt.Errorf("Taproot Asset OOR idempotency key " +
			"reused with different request")
	}
	if state.RequestDigest != digest {
		if state.Attempt != "" || state.hasCommittedPackages() {
			return nil, fmt.Errorf("Taproot Asset OOR " +
				"idempotency key reused with different " +
				"carrier inputs")
		}

		state.RequestDigest = digest
		state.InputOutpoints = cloneOutpoints(inputOutpoints)
		state.PlannedRecipients = nil
		state.OrderingNonce = 0
		state.CarrierLease = nil
		if err := p.storeState(ctx, requestID, state); err != nil {
			return nil, fmt.Errorf("rebind Taproot Asset carrier "+
				"selection: %w", err)
		}
	} else if !outpointsEqual(state.InputOutpoints, inputOutpoints) {
		return nil, fmt.Errorf("Taproot Asset preparation input " +
			"journal does not match request digest")
	}

	return state, nil
}

// decodePreparationState validates the durable envelope before any caller
// makes a restart or reconciliation decision from it.
func decodePreparationState(encoded []byte) (*preparationState, error) {
	var state preparationState
	// wire.OutPoint intentionally uses its stable exported field names in
	// this versioned private envelope.
	//nolint:musttag
	if err := json.Unmarshal(encoded, &state); err != nil {
		return nil, fmt.Errorf("decode taproot asset preparation "+
			"state: %w", err)
	}
	if state.Version != preparationStateVersion {
		return nil, fmt.Errorf("unsupported taproot asset preparation "+
			"state version %d", state.Version)
	}
	if state.Attempt != "" && state.Attempt != attemptArk &&
		!strings.HasPrefix(state.Attempt, attemptCheckpoint+"/") {
		return nil, fmt.Errorf("invalid taproot asset commit "+
			"attempt %q", state.Attempt)
	}
	if len(state.ArkPackage) != 0 {
		if len(state.CheckpointPackages) == 0 {
			return nil, fmt.Errorf("Taproot Asset Ark package " +
				"has no checkpoint packages")
		}
		for _, checkpointPackage := range state.CheckpointPackages {
			if len(checkpointPackage) == 0 {
				return nil, fmt.Errorf("Taproot Asset Ark " +
					"package has an uncommitted " +
					"checkpoint slot")
			}
		}
	}
	if (state.Attempt != "" || state.hasCommittedPackages()) &&
		len(state.InputOutpoints) == 0 {
		return nil, fmt.Errorf("Taproot Asset preparation has no " +
			"input journal")
	}
	seen := make(map[wire.OutPoint]struct{}, len(state.InputOutpoints))
	for _, outpoint := range state.InputOutpoints {
		if _, ok := seen[outpoint]; ok {
			return nil, fmt.Errorf("Taproot Asset preparation has "+
				"duplicate input %s", outpoint)
		}
		seen[outpoint] = struct{}{}
	}

	return &state, nil
}

// storeState atomically persists one preparation state.
func (p *Preparer) storeState(ctx context.Context, requestID string,
	state *preparationState) error {

	// wire.OutPoint intentionally uses its stable exported field names in
	// this versioned private envelope.
	//nolint:musttag
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
	assetInputIndex int, assetRef tapsdk.AssetRef, checkpoint *psbt.Packet,
	result *commitResult) error {

	if checkpoint == nil || checkpoint.UnsignedTx == nil {
		return fmt.Errorf("committed checkpoint PSBT is required")
	}
	if len(result.inputs) != 1 || len(result.outputs) != 1 {
		return fmt.Errorf("checkpoint package must contain one asset " +
			"input and output")
	}
	if result.fundingMode != tapsdk.CustomAnchorFundingCallerFundedExact ||
		result.actualFeeSat != 0 || result.maxFeeSat != 0 {
		return fmt.Errorf("checkpoint package funding mode mismatch")
	}
	input := result.inputs[0]
	output := result.outputs[0]
	assetInput := request.Inputs[assetInputIndex]
	expectedInput := sdkOutpoint(assetInput.VTXO.Outpoint)
	if input.anchorOutpoint != expectedInput ||
		!input.assetRef.Equivalent(assetRef) ||
		input.amount != assetInput.VTXO.TaprootAssetAmount {
		return fmt.Errorf("checkpoint package asset input mismatch")
	}
	if output.anchorOutputIndex != 0 ||
		output.anchorOutpoint != sdkOutpoint(wire.OutPoint{
			Hash: checkpoint.UnsignedTx.TxHash(), Index: 0,
		}) || !output.assetRef.Equivalent(assetRef) ||
		output.amount != assetInput.VTXO.TaprootAssetAmount ||
		output.anchorValueSat != int64(assetInput.VTXO.Amount) ||
		len(output.opTrueWitness) == 0 || len(output.proofBlob) == 0 {
		return fmt.Errorf("checkpoint package asset output mismatch")
	}
	if len(checkpoint.UnsignedTx.TxOut) < 2 {
		return fmt.Errorf("committed checkpoint outputs are incomplete")
	}
	tree, err := arkscript.CheckpointTapScript(
		request.Policy, assetInput.OwnerLeafScript,
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

// preparationRequestDigest binds the exact carrier selection and signing
// material in addition to the selection-independent public send intent.
func preparationRequestDigest(request *oor.TaprootAssetOORPrepareRequest) (
	tapsdk.Hash, error) {

	if request == nil {
		return tapsdk.Hash{}, fmt.Errorf("Taproot Asset OOR prepare " +
			"request is required")
	}
	intentDigest, err := preparationIntentDigest(
		&oor.TaprootAssetOORResumeRequest{
			RequestID:   request.RequestID,
			Policy:      request.Policy,
			Recipients:  request.Recipients,
			OutputFloor: request.OutputFloor,
			Intent:      request.Intent,
		},
	)
	if err != nil {
		return tapsdk.Hash{}, err
	}

	var value bytes.Buffer
	writeDigestBytes(&value, []byte("wavelength-asset-prepare-v3"))
	writeDigestBytes(&value, intentDigest[:])

	// The resolved selection binds here: the ordered asset outpoint set
	// and the exact unit split are immutable once a commit could exist,
	// while the intent digest above stays selection-independent.
	_ = binary.Write(
		&value, binary.BigEndian,
		uint32(
			len(request.Intent.InputVTXOOutpoints),
		),
	)
	for _, outpoint := range request.Intent.InputVTXOOutpoints {
		writeDigestBytes(&value, outpoint.Hash[:])
		_ = binary.Write(&value, binary.BigEndian, outpoint.Index)
	}
	_ = binary.Write(&value, binary.BigEndian, request.Intent.AssetAmount)
	_ = binary.Write(
		&value, binary.BigEndian, request.Intent.RecipientAssetAmount,
	)
	_ = binary.Write(&value, binary.BigEndian, uint32(len(request.Inputs)))
	for idx := range request.Inputs {
		input := request.Inputs[idx]
		if input.VTXO == nil {
			return tapsdk.Hash{}, fmt.Errorf("Taproot Asset OOR "+
				"input %d has no VTXO", idx)
		}
		writeDigestBytes(&value, input.VTXO.Outpoint.Hash[:])
		_ = binary.Write(
			&value, binary.BigEndian, input.VTXO.Outpoint.Index,
		)
		_ = binary.Write(
			&value, binary.BigEndian, uint64(input.VTXO.Amount),
		)
		writeDigestBytes(&value, input.VTXO.PkScript)
		writeDigestBytes(&value, input.VTXOPolicyTemplate)
		writeDigestBytes(&value, input.OwnerLeafScript)
		writeDigestBytes(&value, input.OwnerLeafPolicy)
		if input.TaprootAssetRoot != nil {
			writeDigestBytes(&value, input.TaprootAssetRoot[:])
		} else {
			writeDigestBytes(&value, nil)
		}
		writeDigestBytes(
			&value, []byte(input.VTXO.TaprootAssetRef),
		)
		_ = binary.Write(
			&value, binary.BigEndian, input.VTXO.TaprootAssetAmount,
		)
		if input.VTXO.ClientKey.PubKey != nil {
			writeDigestBytes(
				&value, input.VTXO.ClientKey.PubKey.
					SerializeCompressed(),
			)
		} else {
			writeDigestBytes(&value, nil)
		}
		_ = binary.Write(
			&value, binary.BigEndian,
			int32(input.VTXO.ClientKey.Family),
		)
		_ = binary.Write(
			&value, binary.BigEndian, input.VTXO.ClientKey.Index,
		)
		if input.VTXO.OperatorKey != nil {
			writeDigestBytes(
				&value,
				input.VTXO.OperatorKey.SerializeCompressed(),
			)
		} else {
			writeDigestBytes(&value, nil)
		}
		_ = binary.Write(
			&value, binary.BigEndian, input.VTXO.RelativeExpiry,
		)
	}
	digest := sha256.Sum256(value.Bytes())

	return tapsdk.Hash(digest), nil
}

// preparationIntentDigest binds the public send and current operator policy
// independently of whichever ordinary Bitcoin VTXOs coin selection adds.
func preparationIntentDigest(request *oor.TaprootAssetOORResumeRequest) (
	tapsdk.Hash, error) {

	if request == nil {
		return tapsdk.Hash{}, fmt.Errorf("Taproot Asset OOR resume " +
			"request is required")
	}
	if request.RequestID == "" {
		return tapsdk.Hash{}, fmt.Errorf("Taproot Asset OOR request " +
			"ID is required")
	}
	if request.Policy.OperatorKey == nil {
		return tapsdk.Hash{}, fmt.Errorf("Taproot Asset OOR operator " +
			"key is required")
	}
	if len(request.Recipients) != 1 {
		return tapsdk.Hash{}, fmt.Errorf("Taproot Asset OOR requires " +
			"exactly one recipient")
	}
	if request.OutputFloor <= 0 {
		return tapsdk.Hash{}, fmt.Errorf("Taproot Asset OOR output " +
			"floor is required")
	}
	if err := request.Intent.Validate(); err != nil {
		return tapsdk.Hash{}, err
	}

	// Selection-dependent values are deliberately absent: a retry of a
	// daemon-selected send must resolve the same journal even though a
	// fresh selection could pick other inputs or a different unit split.
	// The receiver's effective units are the same under every split of
	// one logical send, so they anchor the public intent instead.
	var value bytes.Buffer
	writeDigestBytes(&value, []byte("wavelength-asset-intent-v3"))
	writeDigestBytes(&value, []byte(request.RequestID))
	writeDigestBytes(
		&value, request.Policy.OperatorKey.SerializeCompressed(),
	)
	_ = binary.Write(&value, binary.BigEndian, request.Policy.CSVDelay)
	recipient := request.Recipients[0]
	_ = binary.Write(&value, binary.BigEndian, uint64(recipient.Value))
	writeDigestBytes(&value, recipient.PkScript)
	writeDigestBytes(&value, recipient.VTXOPolicyTemplate)
	_ = binary.Write(
		&value, binary.BigEndian, uint64(request.OutputFloor),
	)
	writeDigestBytes(&value, []byte(request.Intent.AssetRef))
	_ = binary.Write(
		&value, binary.BigEndian,
		request.Intent.EffectiveRecipientAssetAmount(),
	)
	writeDigestBytes(&value, request.Intent.ProofFile)
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

func cloneOutpoints(values []wire.OutPoint) []wire.OutPoint {
	return append([]wire.OutPoint(nil), values...)
}

func outpointsEqual(left, right []wire.OutPoint) bool {
	if len(left) != len(right) {
		return false
	}
	for idx := range left {
		if left[idx] != right[idx] {
			return false
		}
	}

	return true
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
