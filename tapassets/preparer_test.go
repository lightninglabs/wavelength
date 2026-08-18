package tapassets

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
	"github.com/lightninglabs/wavelength/oor"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// TestPreparerBuildsTwoTransitionGraph proves the concrete adapter maps the
// Wavelength graph into two ordered SDK commits and returns root-bound PSBTs.
func TestPreparerBuildsTwoTransitionGraph(t *testing.T) {
	t.Parallel()

	request, inventory := testPreparationRequest(t)
	driver := newFakeDriver()
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	preparer := newTestPreparer(driver, inventory, store)

	prepared, err := preparer.PrepareTaprootAssetOOR(
		t.Context(), request,
	)
	require.NoError(t, err)
	require.Len(t, driver.requests, 2)
	require.Nil(t, driver.requests[0].Inputs[0].ProofPath)
	require.NotNil(t, driver.requests[1].Inputs[0].ProofPath)
	require.Len(t, driver.requests[1].Inputs[0].ProofPath.Steps, 1)
	require.Equal(
		t, []byte("wavelength-checkpoint-proof"),
		driver.requests[1].Inputs[0].ProofPath.Steps[0].TransitionProof,
	)
	require.Equal(
		t, [][]byte{{txscript.OP_TRUE}, {1, 2, 3}},
		driver.requests[1].Inputs[0].Witness.Stack,
	)
	require.NoError(t, prepared.Validate(request))
	require.Len(t, prepared.PreparedSubmit.CheckpointPSBTs, 2)
	require.NotNil(t, prepared.Recipients[0].TaprootAssetRoot)

	// The full send pays out the receiver at the floor, the sender's
	// returned carrier, and the operator's float residual.
	require.Len(t, prepared.Recipients, 3)
	require.Equal(t, btcutil.Amount(1_000), prepared.Recipients[0].Value)
	require.Equal(t, btcutil.Amount(5_000), prepared.Recipients[1].Value)
	require.Nil(t, prepared.Recipients[1].TaprootAssetRoot)
	require.Equal(t, btcutil.Amount(29_000), prepared.Recipients[2].Value)
	require.Equal(
		t, request.Lease.PkScript, prepared.Recipients[2].PkScript,
	)
	require.Nil(t, prepared.Recipients[2].TaprootAssetRoot)

	// Both checkpoints feed the Ark transaction; only the asset input's
	// checkpoint carries a sealed tap-sdk package.
	assetCheckpointHash := prepared.PreparedSubmit.CheckpointPSBTs[0].
		UnsignedTx.TxHash()
	assetArkInput, err := findArkInputIndex(
		prepared.PreparedSubmit.ArkPSBT, wire.OutPoint{
			Hash:  assetCheckpointHash,
			Index: 0,
		},
	)
	require.NoError(t, err)
	require.LessOrEqual(t, assetArkInput, uint32(1))
	require.Equal(
		t, [][]byte{[]byte("checkpoint-package"), nil},
		prepared.PreparedSubmit.TaprootAssetTransfer.CheckpointPackages,
	)
	require.Equal(
		t, []byte("ark-package"),
		prepared.PreparedSubmit.TaprootAssetTransfer.ArkPackage,
	)
}

// TestPreparerRestoresCommittedPackages proves a repeated request reconstructs
// the exact prepared graph without issuing another tapd commit.
func TestPreparerRestoresCommittedPackages(t *testing.T) {
	t.Parallel()

	request, inventory := testPreparationRequest(t)
	driver := newFakeDriver()
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	reservations := &fakeReservationStore{}
	first := newTestPreparer(driver, inventory, store, reservations)
	prepared, err := first.PrepareTaprootAssetOOR(t.Context(), request)
	require.NoError(t, err)
	require.Len(t, driver.requests, 2)
	inventory.err = errors.New("tapd unavailable")

	restarted := newTestPreparer(driver, inventory, store, reservations)
	resume, err := restarted.ResumeTaprootAssetOOR(
		t.Context(), testResumeRequest(request),
	)
	require.NoError(t, err)
	require.Equal(
		t, oor.WalletInputOutpoints(request.Inputs),
		resume.InputOutpoints,
	)

	changed := testResumeRequest(request)
	changed.Recipients[0].Value--
	_, err = restarted.ResumeTaprootAssetOOR(t.Context(), changed)
	require.ErrorContains(t, err, "idempotency key reused")

	restored, err := restarted.PrepareTaprootAssetOOR(t.Context(), request)
	require.NoError(t, err)
	require.Len(t, driver.requests, 2)
	require.Equal(
		t, prepared.PreparedSubmit.TaprootAssetTransfer,
		restored.PreparedSubmit.TaprootAssetTransfer,
	)
	firstArk, err := psbtutil.Serialize(prepared.PreparedSubmit.ArkPSBT)
	require.NoError(t, err)
	secondArk, err := psbtutil.Serialize(restored.PreparedSubmit.ArkPSBT)
	require.NoError(t, err)
	require.Equal(t, firstArk, secondArk)

	records := reservations.records()
	require.Len(t, records, 2)
	for _, record := range records {
		require.Equal(
			t, request.Inputs[0].VTXO.Outpoint, record.outpoint,
		)
		require.Equal(
			t, oor.ReservationOwnerKindTaprootAssetPreparation,
			record.ownerKind,
		)
		require.Equal(
			t, oor.TaprootAssetPreparationReservationOwner(
				request.RequestID,
			),
			record.ownerID,
		)
	}
}

// TestPreparerRestoresAfterOORReservationHandoff proves the admitted OOR
// actor's deterministic session ownership is accepted without stealing the
// complete input set back for the preparation request.
func TestPreparerRestoresAfterOORReservationHandoff(t *testing.T) {
	t.Parallel()

	request, inventory := testPreparationRequest(t)
	driver := newFakeDriver()
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	reservations := &fakeReservationStore{}
	first := newTestPreparer(driver, inventory, store, reservations)
	prepared, err := first.PrepareTaprootAssetOOR(t.Context(), request)
	require.NoError(t, err)
	require.Len(t, driver.requests, 2)

	sessionID := prepared.PreparedSubmit.ArkPSBT.UnsignedTx.TxHash()
	require.NoError(
		t,
		reservations.UpsertReservation(
			t.Context(), request.Inputs[0].VTXO.Outpoint,
			oor.ReservationOwnerKindOOROutgoing, sessionID,
		),
	)

	restarted := newTestPreparer(
		driver, inventory, store, reservations,
	)
	resume, err := restarted.ResumeTaprootAssetOOR(
		t.Context(), testResumeRequest(request),
	)
	require.NoError(t, err)
	require.Equal(
		t, oor.WalletInputOutpoints(request.Inputs),
		resume.InputOutpoints,
	)
	restored, err := restarted.PrepareTaprootAssetOOR(
		t.Context(), request,
	)
	require.NoError(t, err)
	require.Equal(
		t, prepared.PreparedSubmit.TaprootAssetTransfer,
		restored.PreparedSubmit.TaprootAssetTransfer,
	)
	require.Len(t, driver.requests, 2)

	records := reservations.records()
	require.Len(t, records, 2)
	require.Equal(
		t, oor.ReservationOwnerKindOOROutgoing,
		records[len(records)-1].ownerKind,
	)
	require.Equal(t, sessionID, records[len(records)-1].ownerID)
}

// TestPreparerBlocksUnknownCommitRetry proves an ambiguous external commit
// leaves a durable marker that prevents a competing transition after restart.
func TestPreparerBlocksUnknownCommitRetry(t *testing.T) {
	t.Parallel()

	request, inventory := testPreparationRequest(t)
	driver := newFakeDriver()
	driver.commitErr = &tapsdk.CustomAnchorCommitAttemptError{
		Err:            errors.New("transport lost"),
		OutcomeUnknown: true,
	}
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	preparer := newTestPreparer(driver, inventory, store)

	_, err = preparer.PrepareTaprootAssetOOR(t.Context(), request)
	require.ErrorContains(t, err, "transport lost")
	require.ErrorIs(t, err, ErrReconciliationRequired)
	require.Len(t, driver.requests, 1)
	driver.commitErr = nil

	restarted := newTestPreparer(driver, inventory, store)
	_, err = restarted.ResumeTaprootAssetOOR(
		t.Context(), testResumeRequest(request),
	)
	require.ErrorIs(t, err, ErrReconciliationRequired)
	_, err = restarted.PrepareTaprootAssetOOR(t.Context(), request)
	require.ErrorIs(t, err, ErrReconciliationRequired)
	require.Len(t, driver.requests, 1)
}

// TestPreparerRebindsCarrierSelectionBeforeCommit proves a known-negative
// pre-checkpoint failure can retry the same public intent with a different
// Bitcoin top-up set, while carrier inputs become immutable at the first
// external transition boundary.
func TestPreparerRebindsCarrierSelectionBeforeCommit(t *testing.T) {
	t.Parallel()

	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	preparer := &Preparer{
		store:        store,
		reservations: &fakeReservationStore{},
	}
	intentDigest := tapsdk.Hash(sha256Bytes([]byte("intent")))
	firstDigest := tapsdk.Hash(sha256Bytes([]byte("first")))
	secondDigest := tapsdk.Hash(sha256Bytes([]byte("second")))
	thirdDigest := tapsdk.Hash(sha256Bytes([]byte("third")))
	firstInputs := []wire.OutPoint{{
		Hash: chainhash.Hash(sha256Bytes([]byte("first-input"))),
	}}
	secondInputs := []wire.OutPoint{{
		Hash: chainhash.Hash(sha256Bytes([]byte("second-input"))),
	}}

	state, err := preparer.loadState(
		t.Context(),
		"rebind", firstDigest, intentDigest, firstInputs,
	)
	require.NoError(t, err)
	state.PlannedRecipients = []oortx.RecipientOutput{{Value: 123}}
	require.NoError(
		t, preparer.storeState(t.Context(), "rebind", state),
	)

	rebound, err := preparer.loadState(
		t.Context(),
		"rebind", secondDigest, intentDigest, secondInputs,
	)
	require.NoError(t, err)
	require.Equal(t, secondDigest, rebound.RequestDigest)
	require.Equal(t, secondInputs, rebound.InputOutpoints)
	require.Empty(t, rebound.PlannedRecipients)

	rebound.CheckpointPackage = []byte("committed")
	require.NoError(
		t,
		preparer.storeState(
			t.Context(),
			"rebind", rebound,
		),
	)
	_, err = preparer.loadState(
		t.Context(),
		"rebind", thirdDigest, intentDigest, firstInputs,
	)
	require.ErrorContains(t, err, "different carrier inputs")
}

// TestPreparerResumeBeforeCommitAndRejectsCorruptVersion pins the concrete v2
// journal behavior used by the RPC adoption bridge.
func TestPreparerResumeBeforeCommitAndRejectsCorruptVersion(t *testing.T) {
	t.Parallel()

	request, _ := testPreparationRequest(t)
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	reservations := &fakeReservationStore{}
	preparer := &Preparer{
		store:        store,
		reservations: reservations,
	}
	requestDigest, err := preparationRequestDigest(request)
	require.NoError(t, err)
	resumeRequest := testResumeRequest(request)
	intentDigest, err := preparationIntentDigest(resumeRequest)
	require.NoError(t, err)
	state, err := preparer.loadState(
		t.Context(), request.RequestID, requestDigest, intentDigest,
		oor.WalletInputOutpoints(request.Inputs),
	)
	require.NoError(t, err)

	resume, err := preparer.ResumeTaprootAssetOOR(
		t.Context(), resumeRequest,
	)
	require.NoError(t, err)
	require.Nil(t, resume)

	require.NoError(
		t,
		reservations.UpsertReservationSet(
			t.Context(), state.InputOutpoints,
			oor.ReservationOwnerKindTaprootAssetPreparation,
			oor.TaprootAssetPreparationReservationOwner(
				request.RequestID,
			),
		),
	)
	resume, err = preparer.ResumeTaprootAssetOOR(
		t.Context(), resumeRequest,
	)
	require.NoError(t, err)
	require.Equal(t, state.InputOutpoints, resume.InputOutpoints)

	require.NoError(
		t,
		reservations.UpsertReservation(
			t.Context(), state.InputOutpoints[0],
			oor.ReservationOwnerKindOOROutgoing,
			chainhash.HashH(
				[]byte("other owner"),
			),
		),
	)
	_, err = preparer.ResumeTaprootAssetOOR(
		t.Context(), resumeRequest,
	)
	require.ErrorIs(t, err, ErrReconciliationRequired)

	state.Version++
	require.NoError(
		t,
		preparer.storeState(
			t.Context(), request.RequestID, state,
		),
	)
	_, err = preparer.ResumeTaprootAssetOOR(
		t.Context(), resumeRequest,
	)
	require.ErrorContains(t, err, "unsupported taproot asset preparation")
}

// TestPreparerRejectsCorruptPlannedRecipientsBeforeCommit proves journaled
// change scripts are revalidated before any tapd mutation on restart.
func TestPreparerRejectsCorruptPlannedRecipientsBeforeCommit(t *testing.T) {
	t.Parallel()

	request, inventory := testPreparationRequest(t)
	driver := newFakeDriver()
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	preparer := newTestPreparer(driver, inventory, store)
	requestDigest, err := preparationRequestDigest(request)
	require.NoError(t, err)
	intentDigest, err := preparationIntentDigest(
		testResumeRequest(request),
	)
	require.NoError(t, err)
	state, err := preparer.loadState(
		t.Context(), request.RequestID, requestDigest, intentDigest,
		oor.WalletInputOutpoints(request.Inputs),
	)
	require.NoError(t, err)

	// Journal a complete plan whose receiver was tampered with.
	plan, err := request.CarrierAllocation()
	require.NoError(t, err)
	senderChange, err := request.BuildChangeRecipient(
		t.Context(), plan.SenderChange,
	)
	require.NoError(t, err)
	planned := cloneRecipients(request.Recipients)
	planned = append(planned, senderChange, oortx.RecipientOutput{
		PkScript:           request.Lease.PkScript,
		Value:              plan.OperatorChange,
		VTXOPolicyTemplate: request.Lease.PolicyTemplate,
	})
	planned[0].Value++
	state.PlannedRecipients = planned
	require.NoError(
		t,
		preparer.storeState(
			t.Context(), request.RequestID, state,
		),
	)

	_, err = preparer.PrepareTaprootAssetOOR(t.Context(), request)
	require.ErrorContains(t, err, "caller receiver changed")
	require.Empty(t, driver.requests)
}

// TestPreparerRetriesKnownCommitFailure proves a known-negative SDK response
// clears the attempt marker and can be retried with the same request identity.
func TestPreparerRetriesKnownCommitFailure(t *testing.T) {
	t.Parallel()

	request, inventory := testPreparationRequest(t)
	driver := newFakeDriver()
	driver.commitErr = errors.New("tapd rejected request")
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	reservations := &fakeReservationStore{}
	preparer := newTestPreparer(
		driver, inventory, store, reservations,
	)

	_, err = preparer.PrepareTaprootAssetOOR(t.Context(), request)
	require.ErrorContains(t, err, "tapd rejected request")
	require.ErrorIs(t, err, ErrReconciliationRequired)
	require.Len(t, driver.requests, 1)
	driver.commitErr = nil

	restarted := newTestPreparer(
		driver, inventory, store, reservations,
	)
	resume, err := restarted.ResumeTaprootAssetOOR(
		t.Context(), testResumeRequest(request),
	)
	require.NoError(t, err)
	require.Equal(
		t, oor.WalletInputOutpoints(request.Inputs),
		resume.InputOutpoints,
	)
	_, err = restarted.PrepareTaprootAssetOOR(t.Context(), request)
	require.NoError(t, err)
	require.Len(t, driver.requests, 3)
}

// TestPreparerQuarantinesAfterCheckpointCommit proves any later failure keeps
// the original managed VTXO quarantined once tapd has accepted the checkpoint
// transition, even when the subsequent Ark failure is known-negative.
func TestPreparerQuarantinesAfterCheckpointCommit(t *testing.T) {
	t.Parallel()

	request, inventory := testPreparationRequest(t)
	driver := newFakeDriver()
	driver.commitErrs = []error{
		nil, errors.New("Ark transition rejected"),
	}
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	reservations := &fakeReservationStore{}
	preparer := newTestPreparer(
		driver, inventory, store, reservations,
	)

	_, err = preparer.PrepareTaprootAssetOOR(t.Context(), request)
	require.ErrorContains(t, err, "Ark transition rejected")
	require.ErrorIs(t, err, ErrReconciliationRequired)
	require.Len(t, driver.requests, 2)
	require.Len(t, reservations.records(), 1)

	restarted := newTestPreparer(
		driver, inventory, store, reservations,
	)
	_, err = restarted.PrepareTaprootAssetOOR(t.Context(), request)
	require.NoError(t, err)
	require.Len(t, driver.requests, 3)
	require.Len(t, reservations.records(), 2)
}

// TestPreparerFailsBeforeCommitWhenReservationFails proves the durable input
// quarantine is established before the first external tapd side effect.
func TestPreparerFailsBeforeCommitWhenReservationFails(t *testing.T) {
	t.Parallel()

	request, inventory := testPreparationRequest(t)
	driver := newFakeDriver()
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	reservations := &fakeReservationStore{
		err: errors.New("reservation unavailable"),
	}
	preparer := newTestPreparer(
		driver, inventory, store, reservations,
	)

	_, err = preparer.PrepareTaprootAssetOOR(t.Context(), request)
	require.ErrorContains(t, err, "Taproot Asset input reservations")
	require.ErrorIs(t, err, ErrReconciliationRequired)
	require.Empty(t, driver.requests)
}

// TestPreparerVerifiesProofBeforeReservation proves deterministic proof and
// inventory failures do not quarantine otherwise healthy carrier inputs.
func TestPreparerVerifiesProofBeforeReservation(t *testing.T) {
	t.Parallel()

	request, inventory := testPreparationRequest(t)
	inventory.err = errors.New("proof inventory unavailable")
	driver := newFakeDriver()
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	reservations := &fakeReservationStore{}
	preparer := newTestPreparer(
		driver, inventory, store, reservations,
	)

	_, err = preparer.PrepareTaprootAssetOOR(t.Context(), request)
	require.ErrorContains(t, err, "preflight Taproot Asset confirmed proof")
	require.ErrorContains(t, err, "proof inventory unavailable")
	require.Empty(t, reservations.records())
	require.Empty(t, driver.requests)
}

// TestPreparerRejectsIdempotencyRewrite proves the durable request digest
// prevents the same idempotency key from being reused for another allocation.
func TestPreparerRejectsIdempotencyRewrite(t *testing.T) {
	t.Parallel()

	request, inventory := testPreparationRequest(t)
	driver := newFakeDriver()
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	preparer := newTestPreparer(driver, inventory, store)
	_, err = preparer.PrepareTaprootAssetOOR(t.Context(), request)
	require.NoError(t, err)

	request.Intent.ProofDeliveryMetadata = []byte("different")
	_, err = preparer.PrepareTaprootAssetOOR(t.Context(), request)
	require.ErrorContains(t, err, "idempotency key reused")
	require.Len(t, driver.requests, 2)
}

// TestProofInventoryVerifierFailsClosed proves the host verifier requires the
// exact Wavelength root and rejects co-anchored passive assets.
func TestProofInventoryVerifierFailsClosed(t *testing.T) {
	t.Parallel()

	request, inventory := testPreparationRequest(t)
	assetRef, err := tapsdk.ParseAssetRef(request.Intent.AssetRef)
	require.NoError(t, err)
	verifier := &proofInventoryVerifier{
		client:    inventory,
		assetRef:  assetRef,
		amount:    request.Intent.AssetAmount,
		anchor:    sdkOutpoint(request.Inputs[0].VTXO.Outpoint),
		assetRoot: tapsdk.Hash(*request.Inputs[0].TaprootAssetRoot),
	}

	result, err := verifier.VerifyConfirmedProof(
		t.Context(), request.Intent.ProofFile,
	)
	require.NoError(t, err)
	require.Zero(t, result.PassiveAssetCount)

	anchor := inventory.onlyAnchor()
	anchor.TaprootAssetRoot[0] ^= 1
	_, err = verifier.VerifyConfirmedProof(
		t.Context(), request.Intent.ProofFile,
	)
	require.ErrorContains(t, err, "root does not match")
	anchor.TaprootAssetRoot[0] ^= 1
	passive := *anchor.Assets[0]
	passive.Genesis.IssuanceID[0] ^= 1
	anchor.Assets = append(anchor.Assets, &passive)
	result, err = verifier.VerifyConfirmedProof(
		t.Context(), request.Intent.ProofFile,
	)
	require.NoError(t, err)
	require.Equal(t, uint32(1), result.PassiveAssetCount)
}

type fakeDriver struct {
	mu                   sync.Mutex
	requests             []*tapsdk.CustomAnchorRequest
	previewRequests      []*tapsdk.CustomAnchorRequest
	results              map[string]*commitResult
	commitErr            error
	commitErrs           []error
	commitPreviewMutator func(
		*tapsdk.CustomAnchorRequest, []commitmentPreview,
	)
	forcedAssetInputIndex   *uint32
	assetCheckpointOutpoint *wire.OutPoint
	assetPreviousOutpoint   *wire.OutPoint
	assetCheckpointTx       []byte
}

// newFakeDriver constructs a deterministic SDK commit boundary for graph
// orchestration tests.
func newFakeDriver() *fakeDriver {
	return &fakeDriver{results: make(map[string]*commitResult)}
}

// Preview records one read-only SDK plan and returns the same roots Commit
// will seal.
func (d *fakeDriver) Preview(ctx context.Context,
	request *tapsdk.CustomAnchorRequest,
	verifier tapsdk.ConfirmedProofVerifier) ([]commitmentPreview, error) {

	d.mu.Lock()
	defer d.mu.Unlock()

	if err := d.verifyFakeSource(ctx, request, verifier); err != nil {
		return nil, err
	}
	d.previewRequests = append(d.previewRequests, request.Clone())

	return fakeCommitmentPreviews(request)
}

// Commit records one SDK request and returns a root-composed anchor PSBT.
func (d *fakeDriver) Commit(ctx context.Context,
	request *tapsdk.CustomAnchorRequest,
	verifier tapsdk.ConfirmedProofVerifier) (*commitResult, error) {

	d.mu.Lock()
	defer d.mu.Unlock()

	if err := d.verifyFakeSource(ctx, request, verifier); err != nil {
		return nil, err
	}
	d.requests = append(d.requests, request.Clone())
	if len(d.commitErrs) != 0 {
		commitErr := d.commitErrs[0]
		d.commitErrs = d.commitErrs[1:]
		if commitErr != nil {
			return nil, commitErr
		}
	}
	if d.commitErr != nil {
		return nil, d.commitErr
	}
	packet, err := psbtutil.Parse(request.AnchorPSBT)
	if err != nil {
		return nil, err
	}
	previews, err := fakeCommitmentPreviews(request)
	if err != nil {
		return nil, err
	}
	if d.commitPreviewMutator != nil {
		d.commitPreviewMutator(request, previews)
	}
	outputs := make([]commitOutput, len(request.Outputs))
	for idx := range request.Outputs {
		outputRequest := request.Outputs[idx]
		preview := previews[idx]
		policyRoot, internalKey, err := requestPolicyRoot(
			outputRequest.Anchor,
		)
		if err != nil {
			return nil, err
		}
		combined := requestMerkleRoot(policyRoot, preview.assetRoot)
		outputKey := txscript.ComputeTaprootOutputKey(
			internalKey, combined[:],
		)
		pkScript, err := txscript.PayToTaprootScript(
			outputKey,
		)
		if err != nil {
			return nil, err
		}
		packet.UnsignedTx.TxOut[outputRequest.AnchorOutputIndex].
			PkScript = pkScript

		// An external script plan commits the caller's own key and has
		// no OP_TRUE spend; every other plan is anyone-can-spend at
		// the asset layer.
		var (
			scriptKey tapsdk.PubKey
			witness   [][]byte
		)
		if outputRequest.Script.Mode == scriptExternal {
			scriptKey = outputRequest.
				Script.External.ScriptKey.PubKey
		} else {
			witness = [][]byte{
				{
					txscript.OP_TRUE,
				}, {
					byte(idx + 1),
					2,
					3,
				},
			}
		}
		outputs[idx] = commitOutput{
			logicalOutputID:   outputRequest.ID,
			anchorOutputIndex: outputRequest.AnchorOutputIndex,
			anchorValueSat:    int64(outputRequest.AnchorValueSat),
			assetRef:          outputRequest.AssetRef,
			amount:            outputRequest.Amount,
			taprootAssetRoot:  preview.assetRoot,
			taprootMerkleRoot: preview.merkleRoot,
			scriptMode:        outputRequest.Script.Mode,
			scriptKey:         scriptKey,
			opTrueWitness:     witness,
			proofBlob: []byte(
				fmt.Sprintf("%s-proof", outputRequest.ID),
			),
		}
	}
	encoded, err := psbtutil.Serialize(packet)
	if err != nil {
		return nil, err
	}
	input := request.Inputs[0]
	isCheckpoint := request.Outputs[0].ID == "wavelength-checkpoint"
	packageBytes := []byte("checkpoint-package")
	if !isCheckpoint {
		packageBytes = []byte("ark-package")
	}
	anchorInputIndex := uint32(0)
	if !isCheckpoint && d.assetCheckpointOutpoint != nil {
		for idx := range packet.UnsignedTx.TxIn {
			if packet.UnsignedTx.TxIn[idx].PreviousOutPoint ==
				*d.assetCheckpointOutpoint {

				anchorInputIndex = uint32(idx)
				break
			}
		}
	}
	if d.forcedAssetInputIndex != nil {
		anchorInputIndex = *d.forcedAssetInputIndex
	}
	assetAnchorOutpoint := sdkOutpoint(
		packet.UnsignedTx.TxIn[anchorInputIndex].PreviousOutPoint,
	)
	result := &commitResult{
		packageBytes: packageBytes,
		anchorPSBT:   encoded,
		fundingMode:  tapsdk.CustomAnchorFundingCallerFundedExact,
		inputs: []commitInput{{
			anchorInputIndex: anchorInputIndex,
			anchorOutpoint:   assetAnchorOutpoint,
			assetRef:         input.AssetRef,
			amount:           input.Amount,
		}},
		outputs: outputs,
	}
	for idx := range result.outputs {
		result.outputs[idx].anchorOutpoint = sdkOutpoint(
			wire.OutPoint{
				Hash:  packet.UnsignedTx.TxHash(),
				Index: result.outputs[idx].anchorOutputIndex,
			},
		)
	}
	if isCheckpoint {
		outpoint := wire.OutPoint{
			Hash:  packet.UnsignedTx.TxHash(),
			Index: result.outputs[0].anchorOutputIndex,
		}
		d.assetCheckpointOutpoint = &outpoint
		previous := packet.UnsignedTx.TxIn[0].PreviousOutPoint
		d.assetPreviousOutpoint = &previous
		d.assetCheckpointTx = serializeTx(packet.UnsignedTx)
	}
	d.results[string(packageBytes)] = result

	return cloneCommitResult(result), nil
}

func fakeCommitmentPreviews(request *tapsdk.CustomAnchorRequest) (
	[]commitmentPreview, error) {

	previews := make([]commitmentPreview, len(request.Outputs))
	for idx := range request.Outputs {
		output := request.Outputs[idx]
		rootMaterial := []byte(
			fmt.Sprintf("%s-%d-asset", output.ID,
				output.AnchorOutputIndex),
		)
		if output.Script.OPTrue != nil {
			opTrueKey := output.
				Script.
				OPTrue.
				InternalKey.
				RawKeyBytes
			rootMaterial = append(
				rootMaterial,
				opTrueKey[:]...,
			)
		}
		assetRoot := tapsdk.Hash(
			sha256Bytes(rootMaterial),
		)
		policyRoot, _, err := requestPolicyRoot(output.Anchor)
		if err != nil {
			return nil, err
		}
		previews[idx] = commitmentPreview{
			logicalOutputID:   output.ID,
			anchorOutputIndex: output.AnchorOutputIndex,
			assetRoot:         assetRoot,
			merkleRoot: requestMerkleRoot(
				policyRoot, assetRoot,
			),
		}
	}

	return previews, nil
}

func (d *fakeDriver) verifyFakeSource(ctx context.Context,
	request *tapsdk.CustomAnchorRequest,
	verifier tapsdk.ConfirmedProofVerifier) error {

	if verifier == nil || len(request.Inputs) == 0 {
		return nil
	}
	proofFile := request.Inputs[0].ProofFile
	if request.Inputs[0].ProofPath != nil {
		proofFile = request.Inputs[0].ProofPath.ConfirmedBaseProof
	}
	verification, err := verifier.VerifyConfirmedProof(
		ctx, proofFile,
	)
	if err != nil {
		return err
	}
	if verification == nil ||
		!verification.AnchorAssetInventoryComplete ||
		verification.PassiveAssetCount != 0 {
		return fmt.Errorf("fake verifier rejected passive inventory")
	}
	if request.Inputs[0].ProofPath != nil &&
		len(request.Inputs[0].ProofPath.Steps) != 0 {

		if d.assetPreviousOutpoint == nil ||
			d.assetCheckpointOutpoint == nil ||
			len(d.assetCheckpointTx) == 0 {
			return fmt.Errorf("fake checkpoint lineage is " +
				"incomplete")
		}
		stepIndex := len(request.Inputs[0].ProofPath.Steps) - 1
		unconfirmedVerifier, ok :=
			verifier.(tapsdk.UnconfirmedAnchorVerifier)
		if !ok {
			return fmt.Errorf("fake verifier has no unconfirmed " +
				"anchor verifier")
		}
		verification := tapsdk.UnconfirmedAnchorVerification{
			StepIndex: uint16(stepIndex),
			PreviousAnchorOutpoint: sdkOutpoint(
				*d.assetPreviousOutpoint,
			),
			AnchorOutpoint: sdkOutpoint(
				*d.assetCheckpointOutpoint,
			),
			AnchorTransaction: append(
				[]byte(nil), d.assetCheckpointTx...,
			),
			PreviousAnchorOutpoints: []tapsdk.Outpoint{
				sdkOutpoint(*d.assetPreviousOutpoint),
			},
		}
		if err := unconfirmedVerifier.VerifyUnconfirmedAnchor(
			ctx, verification,
		); err != nil {
			return err
		}
	}

	return nil
}

// DecodePackage restores a previously returned fake package.
func (d *fakeDriver) DecodePackage(encoded []byte) (*commitResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	result := d.results[string(encoded)]
	if result == nil {
		return nil, fmt.Errorf("unknown fake package %q", encoded)
	}

	return cloneCommitResult(result), nil
}

// cloneCommitResult deep-copies the fake commit result used across restarts.
func cloneCommitResult(result *commitResult) *commitResult {
	clone := *result
	clone.packageBytes = append([]byte(nil), result.packageBytes...)
	clone.anchorPSBT = append([]byte(nil), result.anchorPSBT...)
	clone.inputs = append([]commitInput(nil), result.inputs...)
	for idx := range clone.inputs {
		clone.inputs[idx].proofSource.blob = append(
			[]byte(nil), result.inputs[idx].proofSource.blob...,
		)
	}
	clone.outputs = append([]commitOutput(nil), result.outputs...)
	for idx := range clone.outputs {
		clone.outputs[idx].opTrueWitness = cloneByteSlices(
			result.outputs[idx].opTrueWitness,
		)
		clone.outputs[idx].proofBlob = append(
			[]byte(nil), result.outputs[idx].proofBlob...,
		)
	}

	return &clone
}

type fakeInventory struct {
	verification *tapsdk.VerifyProofResponse

	// verifications resolves a proof file to its tip by content, for
	// fixtures holding the asset across more than one anchor. It wins
	// over verification whenever it has an entry.
	verifications map[string]*tapsdk.VerifyProofResponse

	utxos map[string]*tapsdk.ManagedUtxo
	err   error

	// derived counts key derivations, so a test can prove a pinned key is
	// derived exactly once.
	derived int
}

type reservationRecord struct {
	outpoint  wire.OutPoint
	ownerKind int
	ownerID   chainhash.Hash
}

type fakeReservationStore struct {
	mu  sync.Mutex
	err error

	upserts []reservationRecord
}

func (f *fakeReservationStore) UpsertReservation(_ context.Context,
	outpoint wire.OutPoint, ownerKind int, ownerID chainhash.Hash) error {

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.err != nil {
		return f.err
	}
	f.upserts = append(f.upserts, reservationRecord{
		outpoint:  outpoint,
		ownerKind: ownerKind,
		ownerID:   ownerID,
	})

	return nil
}

func (f *fakeReservationStore) UpsertReservationSet(_ context.Context,
	outpoints []wire.OutPoint, ownerKind int,
	ownerID chainhash.Hash) error {

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.err != nil {
		return f.err
	}
	for _, outpoint := range outpoints {
		f.upserts = append(f.upserts, reservationRecord{
			outpoint:  outpoint,
			ownerKind: ownerKind,
			ownerID:   ownerID,
		})
	}

	return nil
}

func (f *fakeReservationStore) InspectReservationSet(_ context.Context,
	outpoints []wire.OutPoint, ownerKind int, ownerID chainhash.Hash) (
	oor.ReservationSetState, error) {

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.err != nil {
		return oor.ReservationSetAbsent, f.err
	}
	latest := make(map[wire.OutPoint]reservationRecord, len(f.upserts))
	for _, record := range f.upserts {
		latest[record.outpoint] = record
	}
	found := 0
	for _, outpoint := range outpoints {
		record, ok := latest[outpoint]
		if !ok {
			continue
		}
		found++
		if record.ownerKind != ownerKind || record.ownerID != ownerID {
			return oor.ReservationSetInconsistent, nil
		}
	}
	if found == 0 {
		return oor.ReservationSetAbsent, nil
	}
	if found != len(outpoints) {
		return oor.ReservationSetInconsistent, nil
	}

	return oor.ReservationSetOwned, nil
}

func (f *fakeReservationStore) HandoffReservationSet(_ context.Context,
	outpoints []wire.OutPoint, fromOwnerKind int,
	fromOwnerID chainhash.Hash, toOwnerKind int,
	toOwnerID chainhash.Hash) error {

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.err != nil {
		return f.err
	}

	latest := make(map[wire.OutPoint]reservationRecord, len(f.upserts))
	for _, record := range f.upserts {
		latest[record.outpoint] = record
	}
	var replay bool
	for idx, outpoint := range outpoints {
		record, ok := latest[outpoint]
		if !ok {
			return fmt.Errorf("reservation %v is absent", outpoint)
		}
		isSource := record.ownerKind == fromOwnerKind &&
			record.ownerID == fromOwnerID
		isTarget := record.ownerKind == toOwnerKind &&
			record.ownerID == toOwnerID
		if idx == 0 {
			switch {
			case isSource:
				replay = false

			case isTarget:
				replay = true

			default:
				return fmt.Errorf("reservation %v has "+
					"wrong owner", outpoint)
			}
		}
		if (!replay && !isSource) || (replay && !isTarget) {
			return fmt.Errorf("reservation %v has wrong owner",
				outpoint)
		}
	}

	for _, outpoint := range outpoints {
		f.upserts = append(f.upserts, reservationRecord{
			outpoint:  outpoint,
			ownerKind: toOwnerKind,
			ownerID:   toOwnerID,
		})
	}

	return nil
}

func (f *fakeReservationStore) records() []reservationRecord {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]reservationRecord(nil), f.upserts...)
}

// VerifyProof returns the configured tapd proof result.
func (f *fakeInventory) VerifyProof(_ context.Context, proofFile []byte) (
	*tapsdk.VerifyProofResponse, error) {

	if f.err != nil {
		return nil, f.err
	}
	if verification, ok := f.verifications[string(proofFile)]; ok {
		return verification, nil
	}

	return f.verification, nil
}

// DeriveScriptKey returns a deterministic wallet script key, distinct on
// every call so a test can tell a pinned key from a re-derived one.
func (f *fakeInventory) DeriveScriptKey(context.Context) (*tapsdk.ScriptKey,
	error) {

	if f.err != nil {
		return nil, f.err
	}
	f.derived++
	seed := sha256Bytes([]byte(fmt.Sprintf("script-key-%d", f.derived)))
	_, pubKey := btcec.PrivKeyFromBytes(seed[:])
	key, err := tapsdk.ParsePubKey(pubKey.SerializeCompressed())
	if err != nil {
		return nil, err
	}

	return &tapsdk.ScriptKey{
		PubKey: key,
		KeyDesc: tapsdk.KeyDescriptor{
			RawKeyBytes: key,
			KeyLocator: tapsdk.KeyLocator{
				Family: 212,
				Index:  uint32(f.derived),
			},
		},
	}, nil
}

// DeriveInternalKey returns a deterministic wallet anchor internal key,
// distinct on every call.
func (f *fakeInventory) DeriveInternalKey(context.Context) (*tapsdk.InternalKey,
	error) {

	if f.err != nil {
		return nil, f.err
	}
	f.derived++
	seed := sha256Bytes([]byte(fmt.Sprintf("internal-key-%d", f.derived)))
	_, pubKey := btcec.PrivKeyFromBytes(seed[:])
	key, err := tapsdk.ParsePubKey(pubKey.SerializeCompressed())
	if err != nil {
		return nil, err
	}

	return &tapsdk.InternalKey{
		PubKey: key,
		KeyLocator: tapsdk.KeyLocator{
			Family: 213,
			Index:  uint32(f.derived),
		},
	}, nil
}

// UnpackProofFile returns one synthetic issuance proof for chained-path tests.
func (*fakeInventory) UnpackProofFile(_ context.Context, proofFile []byte) (
	[][]byte, error) {

	return [][]byte{append([]byte(nil), proofFile...)}, nil
}

// DecodeProof returns the synthetic issuance marker needed by chained-path
// tests. Detailed bootstrap behavior is covered by verifier_test.go.
func (*fakeInventory) DecodeProof(context.Context, []byte) (
	*tapsdk.DecodedProof, error) {

	return &tapsdk.DecodedProof{IsIssuance: true}, nil
}

// InsertProof accepts the synthetic issuance used by chained-path tests.
func (*fakeInventory) InsertProof(context.Context, []byte,
	*tapsdk.DecodedProof) error {

	return nil
}

// ListUtxos returns the configured complete anchor inventory.
func (f *fakeInventory) ListUtxos(context.Context, *tapsdk.ListUtxosRequest) (
	map[string]*tapsdk.ManagedUtxo, error) {

	if f.err != nil {
		return nil, f.err
	}

	return f.utxos, nil
}

// onlyAnchor returns the sole managed anchor in this fixture.
func (f *fakeInventory) onlyAnchor() *tapsdk.ManagedUtxo {
	for _, anchor := range f.utxos {
		return anchor
	}

	return nil
}

// newTestPreparer installs fake SDK dependencies while retaining the real
// durable journal and Wavelength graph builders.
func newTestPreparer(driver customAnchorDriver, inventory proofInventoryClient,
	store Store, reservationStores ...oor.ReservationSetStore) *Preparer {

	reservationStore := oor.ReservationSetStore(&fakeReservationStore{})
	if len(reservationStores) != 0 {
		reservationStore = reservationStores[0]
	}

	return &Preparer{
		driver:       driver,
		inventory:    inventory,
		store:        store,
		reservations: reservationStore,
	}
}

// testFloatInput builds the leased operator carrier-float input and its
// lease, mirroring the daemon's BuildOperatorFundedTransferInput output.
func testFloatInput(t *testing.T, operatorKey *btcec.PublicKey,
	value btcutil.Amount) (oor.TransferInput, *oor.OORCarrierLease) {

	t.Helper()

	floatKey := testPrivateKey(t, 5)
	policyTemplate, pkScript, err := arkscript.EncodeStandardVTXOArtifacts(
		floatKey.PubKey(), operatorKey, 10,
	)
	require.NoError(t, err)

	ownerLeafPolicy, err := arkscript.LeafTemplate{
		Node: &arkscript.Multisig{
			Keys: []*btcec.PublicKey{
				floatKey.PubKey(),
				operatorKey,
			},
		},
	}.Encode()
	require.NoError(t, err)

	input := oor.TransferInput{
		VTXO: &vtxo.Descriptor{
			Outpoint: wire.OutPoint{
				Hash: chainhash.Hash(
					sha256Bytes(
						[]byte("float-outpoint"),
					),
				),
				Index: 0,
			},
			Amount:         value,
			PkScript:       pkScript,
			PolicyTemplate: policyTemplate,
			OperatorKey:    operatorKey,
			RelativeExpiry: 10,
		},
		VTXOPolicyTemplate: policyTemplate,
		OwnerLeafPolicy:    ownerLeafPolicy,
		OperatorFunded:     true,
	}
	lease := &oor.OORCarrierLease{
		Outpoint:       input.VTXO.Outpoint,
		Value:          value,
		PolicyTemplate: policyTemplate,
		PkScript:       pkScript,
	}

	return input, lease
}

// testChangeRecipientBuilder returns a deterministic wallet-owned change
// builder that derives a fresh policy per call.
func testChangeRecipientBuilder(t *testing.T, operatorKey *btcec.PublicKey,
	firstKeyByte byte) oor.TaprootAssetChangeRecipientBuilder {

	t.Helper()

	calls := byte(0)

	return func(_ context.Context, value btcutil.Amount) (
		oortx.RecipientOutput, error) {

		calls++
		owner := testPrivateKey(t, firstKeyByte+calls)
		policyTemplate, pkScript, err := arkscript.
			EncodeStandardVTXOArtifacts(
				owner.PubKey(), operatorKey, 10,
			)
		if err != nil {
			return oortx.RecipientOutput{}, err
		}

		return oortx.RecipientOutput{
			PkScript:           pkScript,
			Value:              value,
			VTXOPolicyTemplate: policyTemplate,
		}, nil
	}
}

// testPreparationRequest constructs one asset-bearing standard VTXO, one
// leased operator float input, and one floor-valued Bitcoin-only recipient
// policy template.
func testPreparationRequest(t *testing.T) (*oor.TaprootAssetOORPrepareRequest,
	*fakeInventory) {

	t.Helper()
	owner := testPrivateKey(t, 1)
	operator := testPrivateKey(t, 2)
	recipient := testPrivateKey(t, 3)
	assetScript := testPrivateKey(t, 4)
	assetID := tapsdk.AssetID(sha256Bytes([]byte("asset-id")))
	assetRef := tapsdk.AssetRefFromAssetID(assetID)
	inputPolicy, err := arkscript.NewVTXOPolicy(
		owner.PubKey(), operator.PubKey(), 10,
	)
	require.NoError(t, err)
	inputPolicyBytes, err := inputPolicy.Template.Encode()
	require.NoError(t, err)
	inputRoot := chainhash.Hash(sha256Bytes([]byte("input-root")))
	inputComposed, err := arkscript.ComposeWithSiblingRoot(
		inputPolicy.CompiledPolicy, inputRoot,
	)
	require.NoError(t, err)
	inputScript, err := txscript.PayToTaprootScript(
		inputComposed.OutputKey(),
	)
	require.NoError(t, err)
	legacyTapScript, err := arkscript.VTXOTapScript(
		owner.PubKey(), operator.PubKey(), 10,
	)
	require.NoError(t, err)
	input := oor.TransferInput{
		VTXO: &vtxo.Descriptor{
			Outpoint: wire.OutPoint{
				Hash: chainhash.Hash(
					sha256Bytes(
						[]byte("input-outpoint"),
					),
				),
				Index: 1,
			},
			Amount:   btcutil.Amount(5_000),
			PkScript: inputScript,
			ClientKey: keychain.KeyDescriptor{
				KeyLocator: keychain.KeyLocator{
					Family: 1,
					Index:  2,
				},
				PubKey: owner.PubKey(),
			},
			OperatorKey:        operator.PubKey(),
			TapScript:          legacyTapScript,
			RelativeExpiry:     10,
			Status:             vtxo.VTXOStatusLive,
			TaprootAssetRoot:   &inputRoot,
			TaprootAssetRef:    assetRef.String(),
			TaprootAssetAmount: 21,
		},
		VTXOPolicyTemplate:       inputPolicyBytes,
		TaprootAssetRoot:         &inputRoot,
		TaprootAssetRoundCreated: true,
	}
	policy := arkscript.CheckpointPolicy{
		OperatorKey: operator.PubKey(),
		CSVDelay:    10,
	}
	floatInput, lease := testFloatInput(t, operator.PubKey(), 30_000)
	inputs := []oor.TransferInput{input, floatInput}
	require.NoError(t, oor.NormalizeCheckpointOwnerLeaves(policy, inputs))
	recipientPolicy, err := arkscript.NewVTXOPolicy(
		recipient.PubKey(), operator.PubKey(), 10,
	)
	require.NoError(t, err)
	recipientPolicyBytes, err := recipientPolicy.Template.Encode()
	require.NoError(t, err)
	recipientScript, err := recipientPolicy.Template.PkScript()
	require.NoError(t, err)

	request := &oor.TaprootAssetOORPrepareRequest{
		RequestID:   "taproot-asset-request",
		Policy:      policy,
		OutputFloor: 1_000,
		Inputs:      inputs,
		Recipients: []oortx.RecipientOutput{{
			PkScript:           recipientScript,
			Value:              1_000,
			VTXOPolicyTemplate: recipientPolicyBytes,
		}},
		BuildChangeRecipient: testChangeRecipientBuilder(
			t, operator.PubKey(), 40,
		),
		Intent: oor.TaprootAssetOORIntent{
			InputVTXOOutpoints: []wire.OutPoint{
				inputs[0].VTXO.Outpoint,
			},
			AssetRef:    assetRef.String(),
			AssetAmount: 21,
			ProofFile:   []byte("confirmed-proof"),
		},
		Lease: lease,
	}
	require.NoError(t, request.Validate())
	scriptKey, err := tapsdk.ParsePubKey(
		assetScript.PubKey().SerializeCompressed(),
	)
	require.NoError(t, err)
	anchor := &tapsdk.ManagedUtxo{
		OutPoint:         sdkOutpoint(input.VTXO.Outpoint),
		TaprootAssetRoot: tapsdk.Hash(inputRoot),
		Assets: []*tapsdk.AssetRecord{{
			AssetRef: assetRef,
			Genesis: tapsdk.IssuanceGenesis{
				IssuanceID: assetID,
			},
			Amount: 21,
			ScriptKey: tapsdk.ScriptKey{
				PubKey: scriptKey,
			},
		}},
	}
	inventory := &fakeInventory{
		verification: &tapsdk.VerifyProofResponse{
			Valid: true,
			DecodedProof: &tapsdk.DecodedProof{
				AssetRef:   assetRef,
				IssuanceID: assetID,
				ScriptKey:  scriptKey,
				Amount:     21,
				Outpoint:   anchor.OutPoint,
			},
		},
		utxos: map[string]*tapsdk.ManagedUtxo{
			anchor.OutPoint.String(): anchor,
		},
	}

	return request, inventory
}

type testAssetOORResumeRequest = oor.TaprootAssetOORResumeRequest

func testResumeRequest(
	request *oor.TaprootAssetOORPrepareRequest) *testAssetOORResumeRequest {

	return &oor.TaprootAssetOORResumeRequest{
		RequestID:   request.RequestID,
		Policy:      request.Policy,
		Recipients:  cloneRecipients(request.Recipients),
		OutputFloor: request.OutputFloor,
		Intent:      request.Intent,
	}
}

// requestPolicyRoot derives the exact host-policy root in one fake SDK output
// request.
func requestPolicyRoot(plan tapsdk.CustomAnchorOutputPlan) (chainhash.Hash,
	*btcec.PublicKey, error) {

	internalKey, err := btcec.ParsePubKey(plan.InternalKey.PubKey[:])
	if err != nil {
		return chainhash.Hash{}, nil, err
	}
	leaves := make([]txscript.TapLeaf, len(plan.Tapscript.TapLeaves))
	for idx := range plan.Tapscript.TapLeaves {
		leaves[idx] = txscript.NewBaseTapLeaf(
			plan.Tapscript.TapLeaves[idx].Script,
		)
	}

	// A leafless plan is a BIP-86 wallet anchor: no sibling branches the
	// asset commitment, so its root is the taproot root outright.
	if len(leaves) == 0 {
		return chainhash.Hash{}, internalKey, nil
	}
	tree := txscript.AssembleTaprootScriptTree(leaves...)

	return tree.RootNode.TapHash(), internalKey, nil
}

// requestMerkleRoot composes one fake output's taproot root from its host
// policy root and asset commitment root.
func requestMerkleRoot(policyRoot chainhash.Hash,
	assetRoot tapsdk.Hash) tapsdk.Hash {

	if policyRoot == (chainhash.Hash{}) {
		return assetRoot
	}

	return tapsdk.Hash(tapBranchHash(policyRoot[:], assetRoot[:]))
}

// testPrivateKey deterministically derives a test-only private key.
func testPrivateKey(t *testing.T, value byte) *btcec.PrivateKey {
	t.Helper()
	seed := bytes.Repeat([]byte{value}, 32)
	privateKey, _ := btcec.PrivKeyFromBytes(seed)

	return privateKey
}

// sha256Bytes returns a hash array suitable for SDK and btcd test DTOs.
func sha256Bytes(value []byte) [32]byte {
	return sha256.Sum256(value)
}
