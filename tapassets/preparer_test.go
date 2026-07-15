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
		t, []byte("checkpoint-proof"),
		driver.requests[1].Inputs[0].ProofPath.Steps[0].TransitionProof,
	)
	require.Equal(
		t, [][]byte{{txscript.OP_TRUE}, {1, 2, 3}},
		driver.requests[1].Inputs[0].Witness.Stack,
	)
	require.NoError(t, prepared.Validate(request))
	require.Len(t, prepared.PreparedSubmit.CheckpointPSBTs, 1)
	require.NotNil(t, prepared.Recipients[0].TaprootAssetRoot)
	require.Equal(
		t, prepared.PreparedSubmit.CheckpointPSBTs[0].
			UnsignedTx.TxHash(),
		prepared.PreparedSubmit.ArkPSBT.UnsignedTx.
			TxIn[0].PreviousOutPoint.Hash,
	)
	require.Equal(
		t, [][]byte{[]byte("checkpoint-package")},
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
	first := newTestPreparer(driver, inventory, store)
	prepared, err := first.PrepareTaprootAssetOOR(t.Context(), request)
	require.NoError(t, err)
	require.Len(t, driver.requests, 2)
	inventory.err = errors.New("tapd unavailable")

	restarted := newTestPreparer(driver, inventory, store)
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
	require.Len(t, driver.requests, 1)
	driver.commitErr = nil

	restarted := newTestPreparer(driver, inventory, store)
	_, err = restarted.PrepareTaprootAssetOOR(t.Context(), request)
	require.ErrorIs(t, err, ErrReconciliationRequired)
	require.Len(t, driver.requests, 1)
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
	preparer := newTestPreparer(driver, inventory, store)

	_, err = preparer.PrepareTaprootAssetOOR(t.Context(), request)
	require.ErrorContains(t, err, "tapd rejected request")
	require.Len(t, driver.requests, 1)
	driver.commitErr = nil

	restarted := newTestPreparer(driver, inventory, store)
	_, err = restarted.PrepareTaprootAssetOOR(t.Context(), request)
	require.NoError(t, err)
	require.Len(t, driver.requests, 3)
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

// TestProofInventoryVerifierBindsUnconfirmedAnchor proves compact proof steps
// cannot substitute a different transaction at the checkpoint boundary.
func TestProofInventoryVerifierBindsUnconfirmedAnchor(t *testing.T) {
	t.Parallel()

	expected := &expectedUnconfirmedAnchor{
		previousOutpoint: tapsdk.Outpoint{
			Index: 1,
		},
		anchorOutpoint: tapsdk.Outpoint{
			Index: 2,
		},
		transaction: []byte("checkpoint"),
	}
	verifier := &proofInventoryVerifier{unconfirmed: expected}
	transition := tapsdk.UnconfirmedAnchorVerification{
		StepIndex:              0,
		PreviousAnchorOutpoint: expected.previousOutpoint,
		AnchorOutpoint:         expected.anchorOutpoint,
		AnchorTransaction: append(
			[]byte(nil), expected.transaction...,
		),
	}
	require.NoError(
		t,
		verifier.VerifyUnconfirmedAnchor(
			t.Context(), transition,
		),
	)

	transition.AnchorTransaction[0] ^= 1
	err := verifier.VerifyUnconfirmedAnchor(t.Context(), transition)
	require.ErrorContains(t, err, "transaction mismatch")
}

type fakeDriver struct {
	mu        sync.Mutex
	requests  []*tapsdk.CustomAnchorRequest
	results   map[string]*commitResult
	commitErr error
}

// newFakeDriver constructs a deterministic SDK commit boundary for graph
// orchestration tests.
func newFakeDriver() *fakeDriver {
	return &fakeDriver{results: make(map[string]*commitResult)}
}

// Commit records one SDK request and returns a root-composed anchor PSBT.
func (d *fakeDriver) Commit(_ context.Context,
	request *tapsdk.CustomAnchorRequest, _ tapsdk.ConfirmedProofVerifier) (
	*commitResult, error) {

	d.mu.Lock()
	defer d.mu.Unlock()

	d.requests = append(d.requests, request.Clone())
	if d.commitErr != nil {
		return nil, d.commitErr
	}
	packet, err := psbtutil.Parse(request.AnchorPSBT)
	if err != nil {
		return nil, err
	}
	outputRequest := request.Outputs[0]
	assetRoot := tapsdk.Hash(
		sha256Bytes(
			[]byte(outputRequest.ID + "-asset"),
		),
	)
	policyRoot, internalKey, err := requestPolicyRoot(outputRequest.Anchor)
	if err != nil {
		return nil, err
	}
	combined := tapBranchHash(policyRoot[:], assetRoot[:])
	outputKey := txscript.ComputeTaprootOutputKey(internalKey, combined[:])
	packet.UnsignedTx.TxOut[outputRequest.AnchorOutputIndex].PkScript, err =
		txscript.PayToTaprootScript(outputKey)
	if err != nil {
		return nil, err
	}
	encoded, err := psbtutil.Serialize(packet)
	if err != nil {
		return nil, err
	}
	input := request.Inputs[0]
	packageBytes := []byte("checkpoint-package")
	proofBlob := []byte("checkpoint-proof")
	var opTrueWitness [][]byte
	if input.ProofPath != nil {
		packageBytes = []byte("ark-package")
		proofBlob = []byte("ark-proof")
	} else {
		opTrueWitness = [][]byte{{txscript.OP_TRUE}, {1, 2, 3}}
	}
	result := &commitResult{
		packageBytes: packageBytes,
		anchorPSBT:   encoded,
		inputs: []commitInput{{
			anchorOutpoint: sdkOutpoint(
				packet.UnsignedTx.TxIn[0].PreviousOutPoint,
			),
			assetRef: input.AssetRef,
			amount:   input.Amount,
		}},
		outputs: []commitOutput{{
			anchorOutputIndex: outputRequest.AnchorOutputIndex,
			anchorOutpoint: sdkOutpoint(wire.OutPoint{
				Hash:  packet.UnsignedTx.TxHash(),
				Index: outputRequest.AnchorOutputIndex,
			}),
			anchorValueSat:    int64(outputRequest.AnchorValueSat),
			assetRef:          outputRequest.AssetRef,
			amount:            outputRequest.Amount,
			taprootAssetRoot:  assetRoot,
			taprootMerkleRoot: tapsdk.Hash(combined),
			opTrueWitness:     opTrueWitness,
			proofBlob:         proofBlob,
		}},
	}
	d.results[string(packageBytes)] = result

	return cloneCommitResult(result), nil
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
	utxos        map[string]*tapsdk.ManagedUtxo
	err          error
}

// VerifyProof returns the configured tapd proof result.
func (f *fakeInventory) VerifyProof(context.Context, []byte) (
	*tapsdk.VerifyProofResponse, error) {

	if f.err != nil {
		return nil, f.err
	}

	return f.verification, nil
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
	store Store) *Preparer {

	return &Preparer{
		driver:    driver,
		inventory: inventory,
		store:     store,
	}
}

// testPreparationRequest constructs one asset-bearing standard VTXO and one
// Bitcoin-only recipient policy template.
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
			OperatorKey:    operator.PubKey(),
			TapScript:      legacyTapScript,
			RelativeExpiry: 10,
			Status:         vtxo.VTXOStatusLive,
		},
		VTXOPolicyTemplate: inputPolicyBytes,
		TaprootAssetRoot:   &inputRoot,
	}
	policy := arkscript.CheckpointPolicy{
		OperatorKey: operator.PubKey(),
		CSVDelay:    10,
	}
	inputs := []oor.TransferInput{input}
	require.NoError(t, oor.NormalizeCheckpointOwnerLeaves(policy, inputs))
	input = inputs[0]
	recipientPolicy, err := arkscript.NewVTXOPolicy(
		recipient.PubKey(), operator.PubKey(), 10,
	)
	require.NoError(t, err)
	recipientPolicyBytes, err := recipientPolicy.Template.Encode()
	require.NoError(t, err)
	recipientScript, err := recipientPolicy.Template.PkScript()
	require.NoError(t, err)

	request := &oor.TaprootAssetOORPrepareRequest{
		RequestID: "taproot-asset-request",
		Policy:    policy,
		Inputs: []oor.TransferInput{
			input,
		},
		Recipients: []oortx.RecipientOutput{{
			PkScript:           recipientScript,
			Value:              5_000,
			VTXOPolicyTemplate: recipientPolicyBytes,
		}},
		Intent: oor.TaprootAssetOORIntent{
			AssetRef:    assetRef.String(),
			AssetAmount: 21,
			ProofFile:   []byte("confirmed-proof"),
			RecipientScriptKey: assetScript.
				PubKey().
				SerializeCompressed(),
		},
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
	if len(leaves) == 0 {
		return chainhash.Hash{}, nil, fmt.Errorf("fake policy has no " +
			"leaves")
	}
	tree := txscript.AssembleTaprootScriptTree(leaves...)

	return tree.RootNode.TapHash(), internalKey, nil
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
