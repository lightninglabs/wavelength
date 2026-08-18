package tapassets

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	tapsdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	"github.com/lightninglabs/wavelength/oor"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// multiInputFixture carries one prepared two-asset-input request together
// with the fake driver seeded with each input's creating package.
type multiInputFixture struct {
	request   *oor.TaprootAssetOORPrepareRequest
	inventory *fakeInventory
	driver    *fakeDriver
	loader    CreatedPackageLoader
}

// chainedAssetLeaf is one asset VTXO backed by a real base proof file and a
// real creating transition, seeded into the fake driver as a sealed package.
type chainedAssetLeaf struct {
	outpoint     wire.OutPoint
	root         chainhash.Hash
	summary      *tapsdk.AssetProofPathStepSummary
	packageBytes []byte
}

// seedChainedAssetLeaf registers one created-package projection for a leaf
// whose creating transition really spends the shared confirmed base.
func seedChainedAssetLeaf(t *testing.T, driver *fakeDriver, baseFile []byte,
	transitionBytes []byte, rootSeed string) *chainedAssetLeaf {

	t.Helper()

	step := tapsdk.AssetProofPathStep{
		TransitionProof: append([]byte(nil), transitionBytes...),
	}
	summary, err := step.Summary()
	require.NoError(t, err)

	root := chainhash.HashH([]byte(rootSeed))
	packageBytes := []byte("created-package/" + rootSeed)
	driver.results[string(packageBytes)] = &commitResult{
		packageBytes: packageBytes,
		inputs: []commitInput{{
			logicalInputID: "input-0",
			anchorOutpoint: summary.PreviousAnchorOutpoint,
			assetRef:       summary.AssetRef,
			issuanceID:     summary.IssuanceID,
			amount:         summary.Amount,
			proofSource: commitProofSource{
				kind: tapsdk.
					CustomAnchorProofSourceConfirmedFile,
				blob: append([]byte(nil), baseFile...),
			},
		}},
		outputs: []commitOutput{{
			logicalOutputID:   "output-0",
			anchorOutputIndex: summary.AnchorOutpoint.Index,
			anchorOutpoint:    summary.AnchorOutpoint,
			anchorValueSat:    summary.AnchorValueSat,
			assetRef:          summary.AssetRef,
			issuanceID:        summary.IssuanceID,
			amount:            summary.Amount,
			taprootAssetRoot:  tapsdk.Hash(root),
			scriptKey:         summary.ScriptKey,
			scriptMode:        tapsdk.CustomAssetScriptOPTrue,
			opTrueWitness: [][]byte{
				{
					txscript.OP_TRUE,
				}, {
					9,
					9,
					9,
				},
			},
			proofBlob: append([]byte(nil), transitionBytes...),
		}},
	}

	return &chainedAssetLeaf{
		outpoint: wire.OutPoint{
			Hash:  summary.AnchorOutpoint.Txid,
			Index: summary.AnchorOutpoint.Index,
		},
		root:         root,
		summary:      summary,
		packageBytes: packageBytes,
	}
}

// chainedTransferInput builds the wallet-descriptor view of one chained
// asset leaf.
func chainedTransferInput(t *testing.T, leaf *chainedAssetLeaf, ownerByte byte,
	operatorKey *keychain.KeyDescriptor,
	roundCreated bool) oor.TransferInput {

	t.Helper()

	owner := testPrivateKey(t, ownerByte)
	policy, err := arkscript.NewVTXOPolicy(
		owner.PubKey(), operatorKey.PubKey, 10,
	)
	require.NoError(t, err)
	policyBytes, err := policy.Template.Encode()
	require.NoError(t, err)
	composed, err := arkscript.ComposeWithSiblingRoot(
		policy.CompiledPolicy, leaf.root,
	)
	require.NoError(t, err)
	pkScript, err := txscript.PayToTaprootScript(composed.OutputKey())
	require.NoError(t, err)
	tapScript, err := arkscript.VTXOTapScript(
		owner.PubKey(), operatorKey.PubKey, 10,
	)
	require.NoError(t, err)
	root := leaf.root

	return oor.TransferInput{
		VTXO: &vtxo.Descriptor{
			Outpoint: leaf.outpoint,
			Amount:   btcutil.Amount(leaf.summary.AnchorValueSat),
			PkScript: pkScript,
			ClientKey: keychain.KeyDescriptor{
				KeyLocator: keychain.KeyLocator{
					Family: 1,
					Index:  uint32(ownerByte),
				},
				PubKey: owner.PubKey(),
			},
			OperatorKey:        operatorKey.PubKey,
			TapScript:          tapScript,
			RelativeExpiry:     10,
			Status:             vtxo.VTXOStatusLive,
			TaprootAssetRoot:   &root,
			TaprootAssetRef:    leaf.summary.AssetRef.String(),
			TaprootAssetAmount: leaf.summary.Amount,
		},
		VTXOPolicyTemplate:       policyBytes,
		TaprootAssetRoot:         &root,
		TaprootAssetRoundCreated: roundCreated,
	}
}

// testMultiInputPreparationRequest builds a two-asset-input partial send: a
// round-created spine, an OOR-created co-input, one leased float, and real
// per-input creating transitions behind the fake package loader.
func testMultiInputPreparationRequest(t *testing.T) *multiInputFixture {
	t.Helper()

	driver := newFakeDriver()
	baseFile, baseProof, senderKey := proofSourceBase(t)
	transitionA, err := proofSourceTransition(
		t, baseProof, senderKey, testPrivateKey(t, 51),
	).Bytes()
	require.NoError(t, err)
	transitionB, err := proofSourceTransition(
		t, baseProof, senderKey, testPrivateKey(t, 52),
	).Bytes()
	require.NoError(t, err)
	spineLeaf := seedChainedAssetLeaf(
		t, driver, baseFile, transitionA, "spine",
	)
	coLeaf := seedChainedAssetLeaf(t, driver, baseFile, transitionB, "co")

	operator := testPrivateKey(t, 2)
	operatorDesc := &keychain.KeyDescriptor{PubKey: operator.PubKey()}
	spineInput := chainedTransferInput(t, spineLeaf, 11, operatorDesc, true)
	coInput := chainedTransferInput(t, coLeaf, 12, operatorDesc, false)
	floatInput, lease := testFloatInput(t, operator.PubKey(), 30_000)

	policy := arkscript.CheckpointPolicy{
		OperatorKey: operator.PubKey(),
		CSVDelay:    10,
	}
	inputs := []oor.TransferInput{spineInput, coInput, floatInput}
	require.NoError(t, oor.NormalizeCheckpointOwnerLeaves(policy, inputs))

	recipient := testPrivateKey(t, 3)
	recipientPolicy, err := arkscript.NewVTXOPolicy(
		recipient.PubKey(), operator.PubKey(), 10,
	)
	require.NoError(t, err)
	recipientPolicyBytes, err := recipientPolicy.Template.Encode()
	require.NoError(t, err)
	recipientScript, err := recipientPolicy.Template.PkScript()
	require.NoError(t, err)

	totalUnits := spineLeaf.summary.Amount + coLeaf.summary.Amount
	assetRef := spineLeaf.summary.AssetRef.String()
	request := &oor.TaprootAssetOORPrepareRequest{
		RequestID:   "taproot-asset-multi-request",
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
				spineLeaf.outpoint,
				coLeaf.outpoint,
			},
			AssetRef:             assetRef,
			AssetAmount:          totalUnits,
			RecipientAssetAmount: totalUnits - 50,
		},
		Lease: lease,
	}
	require.NoError(t, request.Validate())

	inventory := &fakeInventory{
		verifications: map[string]*tapsdk.VerifyProofResponse{
			string(baseFile): {
				Valid:        true,
				DecodedProof: &tapsdk.DecodedProof{},
			},
		},
	}
	loader := func(_ context.Context, outpoint wire.OutPoint) ([]byte,
		error) {

		switch outpoint {
		case spineLeaf.outpoint:
			return spineLeaf.packageBytes, nil

		case coLeaf.outpoint:
			return coLeaf.packageBytes, nil

		default:
			return nil, fmt.Errorf("unknown created package for %s",
				outpoint)
		}
	}

	return &multiInputFixture{
		request:   request,
		inventory: inventory,
		driver:    driver,
		loader:    loader,
	}
}

// newMultiInputPreparer wires one preparer over the fixture's fake driver
// and package loader.
func newMultiInputPreparer(fixture *multiInputFixture, store Store,
	reservations oor.ReservationSetStore) *Preparer {

	preparer := newTestPreparer(
		fixture.driver, fixture.inventory, store, reservations,
	)
	preparer.loadCreatedPackage = fixture.loader

	return preparer
}

// TestPreparerMergesMultipleAssetInputs proves one atomic transfer commits
// one checkpoint per asset input, merges every checkpoint into a single Ark
// transition (spine first), splits recipient and change units, and sums the
// carrier plan across mixed leaf origins.
func TestPreparerMergesMultipleAssetInputs(t *testing.T) {
	t.Parallel()

	fixture := testMultiInputPreparationRequest(t)
	request := fixture.request
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	reservations := &fakeReservationStore{}
	preparer := newMultiInputPreparer(fixture, store, reservations)

	prepared, err := preparer.PrepareTaprootAssetOOR(t.Context(), request)
	require.NoError(t, err)
	require.NoError(t, prepared.Validate(request))

	// Two checkpoint commits and one Ark commit.
	require.Len(t, fixture.driver.requests, 3)
	for ordinal := 0; ordinal < 2; ordinal++ {
		checkpointRequest := fixture.driver.requests[ordinal]
		require.Len(t, checkpointRequest.Inputs, 1)
		require.Len(t, checkpointRequest.Outputs, 1)
		require.Equal(
			t, "wavelength-checkpoint",
			checkpointRequest.Outputs[0].ID,
		)
		require.Equal(
			t, request.Inputs[ordinal].VTXO.TaprootAssetAmount,
			checkpointRequest.Outputs[0].Amount,
		)
		require.Equal(
			t, uint64(request.Inputs[ordinal].VTXO.Amount),
			checkpointRequest.Outputs[0].AnchorValueSat,
		)
	}

	// Distinct deterministic OP_TRUE key domains per checkpoint ordinal.
	require.NotEqual(
		t,
		fixture.driver.requests[0].Outputs[0].Script.OPTrue.InternalKey,
		fixture.driver.requests[1].Outputs[0].Script.OPTrue.InternalKey,
	)

	// One Ark transition: the spine is input 0, the co-input follows in
	// intent order, each carrying its own per-input amount and appended
	// checkpoint step.
	arkRequest := fixture.driver.requests[2]
	require.Len(t, arkRequest.Inputs, 2)
	require.Equal(t, "wavelength-checkpoint", arkRequest.Inputs[0].ID)
	require.Equal(t, "wavelength-checkpoint/1", arkRequest.Inputs[1].ID)
	for ordinal := 0; ordinal < 2; ordinal++ {
		transitionInput := arkRequest.Inputs[ordinal]
		require.Equal(
			t, request.Inputs[ordinal].VTXO.TaprootAssetAmount,
			transitionInput.Amount,
		)
		require.NotNil(t, transitionInput.ProofPath)
		require.Len(t, transitionInput.ProofPath.Steps, 2)
	}

	// Merge-and-split: 150 units to the receiver, 50 units change, both
	// on operator-floor carriers.
	require.Len(t, arkRequest.Outputs, 2)
	require.Equal(t, receiverOutputID, arkRequest.Outputs[0].ID)
	require.Equal(t, uint64(150), arkRequest.Outputs[0].Amount)
	require.Equal(t, changeOutputID, arkRequest.Outputs[1].ID)
	require.Equal(t, uint64(50), arkRequest.Outputs[1].Amount)

	// Reclaim interplay: the round-created spine's carrier returns to the
	// sender; the OOR-created co-input's carrier joins the operator
	// change on top of the float residual.
	require.Len(t, prepared.Recipients, 4)
	require.Equal(t, btcutil.Amount(1_000), prepared.Recipients[0].Value)
	require.Equal(
		t, uint64(150), prepared.Recipients[0].TaprootAssetAmount,
	)
	require.Equal(t, btcutil.Amount(1_000), prepared.Recipients[1].Value)
	require.Equal(
		t, uint64(50), prepared.Recipients[1].TaprootAssetAmount,
	)
	require.Equal(
		t, request.Inputs[0].VTXO.Amount, prepared.Recipients[2].Value,
	)
	require.Nil(t, prepared.Recipients[2].TaprootAssetRoot)
	require.Equal(
		t, btcutil.Amount(30_000)-2_000+request.Inputs[1].VTXO.Amount,
		prepared.Recipients[3].Value,
	)
	require.Equal(
		t, request.Lease.PkScript, prepared.Recipients[3].PkScript,
	)

	// The sealed container fills one checkpoint package slot per asset
	// input, at the transfer-input positions.
	require.Equal(
		t, [][]byte{
			fakeCheckpointPackageName(
				request.Inputs[0].
					VTXO.Outpoint,
			),
			fakeCheckpointPackageName(
				request.Inputs[1].
					VTXO.Outpoint,
			),
			nil,
		},
		prepared.PreparedSubmit.TaprootAssetTransfer.CheckpointPackages,
	)

	// Both wallet inputs are quarantined under the preparation owner.
	records := reservations.records()
	require.Len(t, records, 2)
	require.Equal(t, request.Inputs[0].VTXO.Outpoint, records[0].outpoint)
	require.Equal(t, request.Inputs[1].VTXO.Outpoint, records[1].outpoint)

	// Restart restores every sealed package without new tapd commits.
	restarted := newMultiInputPreparer(fixture, store, reservations)
	restored, err := restarted.PrepareTaprootAssetOOR(t.Context(), request)
	require.NoError(t, err)
	require.Len(t, fixture.driver.requests, 3)
	require.Equal(
		t, prepared.PreparedSubmit.TaprootAssetTransfer,
		restored.PreparedSubmit.TaprootAssetTransfer,
	)
	require.Equal(t, prepared.Recipients, restored.Recipients)

	resume, err := restarted.ResumeTaprootAssetOOR(
		t.Context(), testResumeRequest(request),
	)
	require.NoError(t, err)
	require.Equal(
		t, request.Intent.InputVTXOOutpoints, resume.InputOutpoints,
	)
}

// TestPreparerResumesPartialCheckpointCommits proves a known-negative
// failure between two checkpoint commits retries only the missing
// checkpoint and the Ark transition, reusing the journaled first package.
func TestPreparerResumesPartialCheckpointCommits(t *testing.T) {
	t.Parallel()

	fixture := testMultiInputPreparationRequest(t)
	request := fixture.request
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	reservations := &fakeReservationStore{}
	preparer := newMultiInputPreparer(fixture, store, reservations)

	fixture.driver.commitErrs = []error{
		nil, errors.New("second checkpoint rejected"),
	}
	_, err = preparer.PrepareTaprootAssetOOR(t.Context(), request)
	require.ErrorContains(t, err, "second checkpoint rejected")
	require.ErrorIs(t, err, ErrReconciliationRequired)
	require.Len(t, fixture.driver.requests, 2)

	restarted := newMultiInputPreparer(fixture, store, reservations)
	prepared, err := restarted.PrepareTaprootAssetOOR(t.Context(), request)
	require.NoError(t, err)
	require.NoError(t, prepared.Validate(request))
	require.Len(t, fixture.driver.requests, 4)
	require.Equal(
		t, "wavelength-checkpoint",
		fixture.driver.requests[2].Outputs[0].ID,
	)
	require.Equal(
		t, request.Inputs[1].VTXO.TaprootAssetAmount,
		fixture.driver.requests[2].Outputs[0].Amount,
	)
}

// TestPreparationIntentDigestIgnoresSelection proves the intent digest stays
// stable across different resolutions of the same logical send, so a
// daemon-selected retry resolves the journal its earlier attempt wrote.
func TestPreparationIntentDigestIgnoresSelection(t *testing.T) {
	t.Parallel()

	operator := testPrivateKey(t, 2)
	newResume := func(outpoints []wire.OutPoint, total,
		recipientUnits uint64) *oor.TaprootAssetOORResumeRequest {

		return &oor.TaprootAssetOORResumeRequest{
			RequestID: "digest-request",
			Policy: arkscript.CheckpointPolicy{
				OperatorKey: operator.PubKey(),
				CSVDelay:    10,
			},
			Recipients: []oortx.RecipientOutput{{
				PkScript: []byte{
					0x51,
				},
				Value: 1_000,
				VTXOPolicyTemplate: []byte{
					0x01,
				},
			}},
			OutputFloor: 1_000,
			Intent: oor.TaprootAssetOORIntent{
				InputVTXOOutpoints:   outpoints,
				AssetRef:             "tapr1asset",
				AssetAmount:          total,
				RecipientAssetAmount: recipientUnits,
			},
		}
	}

	unresolved := newResume(nil, 100, 0)
	singleInput := newResume(
		[]wire.OutPoint{{Index: 1}}, 100, 0,
	)
	multiInput := newResume(
		[]wire.OutPoint{
			{Index: 1},
			{Index: 2},
		},
		150,
		100,
	)

	unresolvedDigest, err := preparationIntentDigest(unresolved)
	require.NoError(t, err)
	singleDigest, err := preparationIntentDigest(singleInput)
	require.NoError(t, err)
	multiDigest, err := preparationIntentDigest(multiInput)
	require.NoError(t, err)
	require.Equal(t, unresolvedDigest, singleDigest)
	require.Equal(t, unresolvedDigest, multiDigest)

	otherSend := newResume(nil, 99, 0)
	otherDigest, err := preparationIntentDigest(otherSend)
	require.NoError(t, err)
	require.NotEqual(t, unresolvedDigest, otherDigest)
}

// TestPreparationRequestDigestBindsInputOrder proves the request digest pins
// the ordered asset input set, so a journaled preparation can never be
// replayed against a reordered or substituted selection.
func TestPreparationRequestDigestBindsInputOrder(t *testing.T) {
	t.Parallel()

	fixture := testMultiInputPreparationRequest(t)
	request := fixture.request
	first, err := preparationRequestDigest(request)
	require.NoError(t, err)
	repeat, err := preparationRequestDigest(request)
	require.NoError(t, err)
	require.Equal(t, first, repeat)

	reordered := *request
	reordered.Inputs = []oor.TransferInput{
		request.Inputs[1], request.Inputs[0], request.Inputs[2],
	}
	reordered.Intent.InputVTXOOutpoints = []wire.OutPoint{
		request.Intent.InputVTXOOutpoints[1],
		request.Intent.InputVTXOOutpoints[0],
	}
	swapped, err := preparationRequestDigest(&reordered)
	require.NoError(t, err)
	require.NotEqual(t, first, swapped)
}
