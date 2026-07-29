package tapassets

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
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

// TestPreparerBuildsPartialAssetTransferWithCarrierTopUp proves an asset input
// can be supplemented by a Bitcoin-only VTXO while the asset allocation and
// all three carrier outputs remain explicit and independently conserved.
func TestPreparerBuildsPartialAssetTransferWithCarrierTopUp(t *testing.T) {
	t.Parallel()

	request, inventory := testPreparationRequest(t)
	assetInput := request.Inputs[0]
	assetInput.VTXO.Amount = 500
	assetInput.VTXO.TaprootAssetAmount = 1_000
	request.Intent.AssetAmount = 1_000
	request.Intent.RecipientAssetAmount = 800
	request.Intent.AssetChangeCarrierValueSat = 500
	request.Recipients[0].Value = 700
	request.OutputFloor = 300
	inventory.onlyAnchor().Assets[0].Amount = 1_000
	inventory.verification.DecodedProof.Amount = 1_000

	bitcoinInput := testBitcoinInput(
		t, request.Policy.OperatorKey, 1_000,
	)
	request.Inputs = []oor.TransferInput{bitcoinInput, assetInput}
	require.NoError(
		t, oor.NormalizeCheckpointOwnerLeaves(
			request.Policy, request.Inputs,
		),
	)

	var (
		changeCalls  int
		changeValues []btcutil.Amount
	)
	request.BuildChangeRecipient = func(_ context.Context,
		value btcutil.Amount) (oortx.RecipientOutput, error) {

		changeCalls++
		changeValues = append(changeValues, value)
		owner := testPrivateKey(t, byte(10+changeCalls))
		policy, err := arkscript.NewVTXOPolicy(
			owner.PubKey(), request.Policy.OperatorKey,
			request.Policy.CSVDelay,
		)
		if err != nil {
			return oortx.RecipientOutput{}, err
		}
		template, err := policy.Template.Encode()
		if err != nil {
			return oortx.RecipientOutput{}, err
		}
		pkScript, err := policy.Template.PkScript()
		if err != nil {
			return oortx.RecipientOutput{}, err
		}

		return oortx.RecipientOutput{
			PkScript:           pkScript,
			Value:              value,
			VTXOPolicyTemplate: template,
		}, nil
	}
	require.NoError(t, request.Validate())

	driver := newFakeDriver()
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	reservations := &fakeReservationStore{}
	preparer := newTestPreparer(
		driver, inventory, store, reservations,
	)
	prepared, err := preparer.PrepareTaprootAssetOOR(
		t.Context(), request,
	)
	require.NoError(t, err)
	require.NoError(t, prepared.Validate(request))

	// The input's 500 carrier sats cannot fund the 700-sat receiver and
	// 500-sat asset change alone. The Bitcoin-only input tops up the graph,
	// leaving an ordinary 300-sat Bitcoin change VTXO.
	require.Equal(t, 2, changeCalls)
	require.Equal(
		t, []btcutil.Amount{500, 300}, changeValues,
	)
	require.Len(t, prepared.Recipients, 3)
	require.Equal(t, btcutil.Amount(700), prepared.Recipients[0].Value)
	require.Equal(t, uint64(800), prepared.Recipients[0].TaprootAssetAmount)
	require.NotNil(t, prepared.Recipients[0].TaprootAssetRoot)
	require.Equal(t, btcutil.Amount(500), prepared.Recipients[1].Value)
	require.Equal(t, uint64(200), prepared.Recipients[1].TaprootAssetAmount)
	require.NotNil(t, prepared.Recipients[1].TaprootAssetRoot)
	require.Equal(t, btcutil.Amount(300), prepared.Recipients[2].Value)
	require.Nil(t, prepared.Recipients[2].TaprootAssetRoot)
	require.Empty(t, prepared.Recipients[2].TaprootAssetRef)
	require.Zero(t, prepared.Recipients[2].TaprootAssetAmount)
	require.Equal(t, prepared.Recipients[0], prepared.Receiver)
	require.NotEqual(t, prepared.Recipients[1], prepared.Receiver)

	// There is one checkpoint per input, but only the asset checkpoint has
	// a sealed tap-sdk package. The package remains at the transfer-input
	// position rather than the canonically sorted Ark-input position.
	require.Len(t, prepared.PreparedSubmit.CheckpointPSBTs, 2)
	require.Equal(
		t, [][]byte{nil, []byte("checkpoint-package")},
		prepared.PreparedSubmit.TaprootAssetTransfer.CheckpointPackages,
	)
	require.Len(t, driver.requests, 2)
	checkpointRequest := driver.requests[0]
	arkRequest := driver.requests[1]
	require.Equal(
		t, "wavelength-checkpoint", checkpointRequest.Outputs[0].ID,
	)
	require.Len(t, arkRequest.Outputs, 2)
	require.Equal(t, receiverOutputID, arkRequest.Outputs[0].ID)
	require.Equal(t, uint64(800), arkRequest.Outputs[0].Amount)
	require.Equal(t, uint64(700), arkRequest.Outputs[0].AnchorValueSat)
	require.Equal(t, changeOutputID, arkRequest.Outputs[1].ID)
	require.Equal(t, uint64(200), arkRequest.Outputs[1].Amount)
	require.Equal(t, uint64(500), arkRequest.Outputs[1].AnchorValueSat)
	for idx := range arkRequest.Outputs {
		require.Equal(
			t, tapsdk.CustomAssetScriptOPTrue,
			arkRequest.Outputs[idx].Script.Mode,
		)
		require.NotNil(t, arkRequest.Outputs[idx].Script.OPTrue)
	}

	// Each signing plan points at the actual BIP-69-sorted Ark input for
	// the corresponding transfer checkpoint, not its caller-side index.
	ark, err := psbtutil.Parse(arkRequest.AnchorPSBT)
	require.NoError(t, err)
	bitcoinChangeIndex, err := oortx.RecipientOutputIndex(
		prepared.Recipients, prepared.Recipients[2],
	)
	require.NoError(t, err)
	require.Zero(
		t, bitcoinChangeIndex, "the 300-sat Bitcoin change must "+
			"move from caller position 2 to canonical position 0",
	)
	bitcoinChangeOutput := ark.Outputs[bitcoinChangeIndex]
	_, bitcoinChangePolicy, err := recipientAnchorPlan(
		prepared.Recipients[2],
	)
	require.NoError(t, err)
	require.Equal(
		t, schnorr.SerializePubKey(bitcoinChangePolicy.InternalKey),
		bitcoinChangeOutput.TaprootInternalKey,
	)
	bitcoinChangeRoot := assertBIP371TapTree(
		t, bitcoinChangeOutput.TaprootTapTree, bitcoinChangePolicy,
	)
	require.Equal(
		t, bitcoinChangePolicy.RootHash, bitcoinChangeRoot[:],
	)
	privateTapTree, err := arkscript.EncodeTapTree(bitcoinChangePolicy)
	require.NoError(t, err)
	require.NotEqual(
		t, privateTapTree, bitcoinChangeOutput.TaprootTapTree, "Wave"+
			"length's count-prefixed checkpoint encoding is "+
			"not BIP-371",
	)
	for idx := range arkRequest.Outputs {
		anchorIndex := arkRequest.Outputs[idx].AnchorOutputIndex
		assetOutput := ark.Outputs[anchorIndex]
		require.Empty(t, assetOutput.TaprootInternalKey)
		require.Empty(t, assetOutput.TaprootTapTree)
	}
	p2aOutput := ark.Outputs[len(ark.Outputs)-1]
	require.Empty(t, p2aOutput.TaprootInternalKey)
	require.Empty(t, p2aOutput.TaprootTapTree)
	require.Len(t, arkRequest.SigningPlans, 2)
	for idx := range request.Inputs {
		checkpoint := prepared.PreparedSubmit.CheckpointPSBTs[idx]
		want, err := findArkInputIndex(
			ark, wire.OutPoint{
				Hash: checkpoint.UnsignedTx.TxHash(), Index: 0,
			},
		)
		require.NoError(t, err)
		require.Equal(
			t, want, arkRequest.SigningPlans[idx].InputIndex,
		)
	}

	// Restart uses the persisted change policies and sealed packages. It
	// neither derives new addresses nor repeats a tapd commit.
	restarted := newTestPreparer(
		driver, inventory, store, reservations,
	)
	restored, err := restarted.PrepareTaprootAssetOOR(
		t.Context(), request,
	)
	require.NoError(t, err)
	require.Equal(t, 2, changeCalls)
	require.Len(t, driver.requests, 2)
	require.Equal(t, prepared.Recipients, restored.Recipients)
	require.Equal(t, prepared.Receiver, restored.Receiver)
	require.Len(t, reservations.records(), 4)
}

// TestEncodeBIP371TapTreePreservesArkShape proves the standard depth-first
// tuples reconstruct the exact Ark root for balanced and non-power-of-two
// policy shapes.
func TestEncodeBIP371TapTreePreservesArkShape(t *testing.T) {
	t.Parallel()

	for _, leafCount := range []int{1, 2, 3, 5} {
		t.Run(fmt.Sprintf("%d leaves", leafCount), func(t *testing.T) {
			leaves := make([]arkscript.PolicyLeaf, leafCount)
			for idx := range leaves {
				script, err := txscript.NewScriptBuilder().
					AddInt64(int64(idx + 1)).Script()
				require.NoError(t, err)
				leaves[idx] = arkscript.PolicyLeaf{
					Leaf: txscript.NewBaseTapLeaf(script),
				}
			}
			policy, err := arkscript.BuildTree(
				leaves, &arkscript.ARKNUMSKey,
			)
			require.NoError(t, err)

			encoded, err := encodeBIP371TapTree(policy)
			require.NoError(t, err)
			root := assertBIP371TapTree(t, encoded, policy)
			require.Equal(t, policy.RootHash, root[:])
		})
	}
}

// bip371TreeNode is one pending node while reconstructing a depth-first tree.
type bip371TreeNode struct {
	depth uint8
	hash  chainhash.Hash
}

// assertBIP371TapTree verifies the standard depth-first tuple encoding used by
// PSBT_OUT_TAP_TREE, independently of Wavelength's private count-prefixed
// checkpoint encoding.
func assertBIP371TapTree(t *testing.T, encoded []byte,
	policy *arkscript.CompiledPolicy) chainhash.Hash {

	t.Helper()
	require.NotEmpty(t, encoded)

	reader := bytes.NewReader(encoded)
	nodes := make([]bip371TreeNode, 0, len(policy.Leaves))
	for idx := range policy.Leaves {
		depth, err := reader.ReadByte()
		require.NoError(t, err)
		spendInfo, err := policy.SpendInfo(idx)
		require.NoError(t, err)
		require.EqualValues(
			t, (len(spendInfo.ControlBlock)-33)/chainhash.HashSize,
			depth,
		)

		version, err := reader.ReadByte()
		require.NoError(t, err)
		require.EqualValues(
			t, policy.Leaves[idx].Leaf.LeafVersion, version,
		)

		scriptLen, err := wire.ReadVarInt(reader, 0)
		require.NoError(t, err)
		script := make([]byte, int(scriptLen))
		read, err := reader.Read(script)
		require.NoError(t, err)
		require.Equal(t, int(scriptLen), read)
		require.Equal(t, policy.Leaves[idx].Leaf.Script, script)

		leaf := txscript.NewTapLeaf(
			txscript.TapscriptLeafVersion(version), script,
		)
		nodes = append(nodes, bip371TreeNode{
			depth: depth,
			hash:  leaf.TapHash(),
		})
		for len(nodes) >= 2 {
			left := nodes[len(nodes)-2]
			right := nodes[len(nodes)-1]
			if left.depth != right.depth {
				break
			}
			require.NotZero(
				t, left.depth,
				"two roots cannot occupy depth zero",
			)

			parent := tapBranchHash(left.hash[:], right.hash[:])
			nodes = append(
				nodes[:len(nodes)-2], bip371TreeNode{
					depth: left.depth - 1,
					hash:  parent,
				},
			)
		}
	}
	require.Zero(t, reader.Len())
	require.Len(t, nodes, 1)
	require.Zero(t, nodes[0].depth)

	return nodes[0].hash
}

// TestPreparerRejectsInvalidRecipientPlansBeforeReservation proves policy
// decode and canonical output identity errors cannot first surface after the
// checkpoint transition has been committed.
func TestPreparerRejectsInvalidRecipientPlansBeforeReservation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*oor.TaprootAssetOORPrepareRequest)
		wantErr string
	}{
		{
			name: "malformed policy",
			mutate: func(
				request *oor.TaprootAssetOORPrepareRequest) {

				request.Recipients[0].
					VTXOPolicyTemplate = []byte{
					1,
				}
			},
			wantErr: "recipient 0 policy",
		},
		{
			name: "duplicate value and script",
			mutate: func(
				request *oor.TaprootAssetOORPrepareRequest) {

				request.Inputs[0].VTXO.Amount = 1_000
				request.Recipients[0].Value = 500
				request.OutputFloor = 500
				request.Intent.RecipientAssetAmount = 10
				request.Intent.AssetChangeCarrierValueSat = 500
				request.BuildChangeRecipient = func(
					_ context.Context,
					value btcutil.Amount) (
					oortx.RecipientOutput, error) {

					change := request.Recipients[0]
					change.Value = value

					return change, nil
				}
			},
			wantErr: "duplicates an earlier value and pkScript",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			request, inventory := testPreparationRequest(t)
			test.mutate(request)
			driver := newFakeDriver()
			store, err := NewFileStore(t.TempDir())
			require.NoError(t, err)
			reservations := &fakeReservationStore{}
			preparer := newTestPreparer(
				driver, inventory, store, reservations,
			)

			_, err = preparer.PrepareTaprootAssetOOR(
				t.Context(), request,
			)
			require.ErrorContains(t, err, test.wantErr)
			require.Empty(t, driver.requests)
			require.Empty(t, reservations.records())
		})
	}
}

// TestPreparerRejectsArkCommitPreviewMismatch proves a tapd commit response
// cannot silently substitute commitments after Wavelength has reached a stable
// canonical output ordering.
func TestPreparerRejectsArkCommitPreviewMismatch(t *testing.T) {
	t.Parallel()

	request, inventory := testPreparationRequest(t)
	driver := newFakeDriver()
	driver.commitPreviewMutator = func(request *tapsdk.CustomAnchorRequest,
		previews []commitmentPreview) {

		if request.Outputs[0].ID == "wavelength-checkpoint" {
			return
		}
		previews[0].assetRoot[0] ^= 1
	}
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	preparer := newTestPreparer(driver, inventory, store)

	_, err = preparer.PrepareTaprootAssetOOR(t.Context(), request)
	require.ErrorContains(t, err, "Ark commit diverged from preview")
	require.ErrorIs(t, err, ErrReconciliationRequired)
	require.Len(t, driver.requests, 2)
}

// TestAssetSpendSourceRequiresTwoTransitionSlots pins the pre-commit proof
// depth guard needed to keep the final Ark output spendable on the next hop.
func TestAssetSpendSourceRequiresTwoTransitionSlots(t *testing.T) {
	t.Parallel()

	available := &assetSpendSource{proofPath: &tapsdk.AssetProofPath{
		Steps: make(
			[]tapsdk.AssetProofPathStep,
			tapsdk.AssetProofPathMaxDepth-2,
		),
	}}
	require.NoError(t, available.validateTransitionCapacity())

	exhausted := &assetSpendSource{proofPath: &tapsdk.AssetProofPath{
		Steps: make(
			[]tapsdk.AssetProofPathStep,
			tapsdk.AssetProofPathMaxDepth-1,
		),
	}}
	require.ErrorContains(
		t, exhausted.validateTransitionCapacity(),
		"leaves no room for both Wavelength transitions",
	)
}

// TestPreparerBoundsCanonicalOrderingCycles proves equal-value asset outputs
// that perpetually swap BIP-69 positions cannot drive an unbounded preview
// loop or reach the second tapd commit.
func TestPreparerBoundsCanonicalOrderingCycles(t *testing.T) {
	t.Parallel()

	request, inventory := testPreparationRequest(t)
	request.Inputs[0].VTXO.Amount = 1_000
	request.Recipients[0].Value = 500
	request.OutputFloor = 500
	request.Intent.RecipientAssetAmount = 10
	request.Intent.AssetChangeCarrierValueSat = 500
	request.BuildChangeRecipient = func(_ context.Context,
		value btcutil.Amount) (oortx.RecipientOutput, error) {

		owner := testPrivateKey(t, 17)
		policy, err := arkscript.NewVTXOPolicy(
			owner.PubKey(), request.Policy.OperatorKey,
			request.Policy.CSVDelay,
		)
		if err != nil {
			return oortx.RecipientOutput{}, err
		}
		template, err := policy.Template.Encode()
		if err != nil {
			return oortx.RecipientOutput{}, err
		}
		pkScript, err := policy.Template.PkScript()
		if err != nil {
			return oortx.RecipientOutput{}, err
		}

		return oortx.RecipientOutput{
			PkScript:           pkScript,
			Value:              value,
			VTXOPolicyTemplate: template,
		}, nil
	}
	require.NoError(t, request.Validate())

	driver := &nonConvergingDriver{fakeDriver: newFakeDriver()}
	store, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	preparer := newTestPreparer(driver, inventory, store)

	_, err = preparer.PrepareTaprootAssetOOR(t.Context(), request)
	require.ErrorContains(
		t, err, "canonical output ordering did not converge",
	)
	require.ErrorIs(t, err, ErrReconciliationRequired)
	require.Len(t, driver.requests, 1)
	require.GreaterOrEqual(
		t, len(driver.previewRequests), maxOrderingNonces,
	)
}

type nonConvergingDriver struct {
	*fakeDriver
}

func (d *nonConvergingDriver) Preview(ctx context.Context,
	request *tapsdk.CustomAnchorRequest,
	verifier tapsdk.ConfirmedProofVerifier) ([]commitmentPreview, error) {

	if request.Outputs[0].ID == "wavelength-checkpoint" {
		return d.fakeDriver.Preview(
			ctx, request, verifier,
		)
	}
	if err := d.fakeDriver.verifyFakeSource(
		ctx, request, verifier,
	); err != nil {
		return nil, err
	}

	d.mu.Lock()
	d.previewRequests = append(d.previewRequests, request.Clone())
	d.mu.Unlock()

	var receiver, change *tapsdk.CustomAssetOutput
	for idx := range request.Outputs {
		switch request.Outputs[idx].ID {
		case receiverOutputID:
			receiver = &request.Outputs[idx]

		case changeOutputID:
			change = &request.Outputs[idx]
		}
	}
	if receiver == nil || change == nil {
		return nil, fmt.Errorf("two asset outputs are required")
	}
	wantReceiverBefore := receiver.AnchorOutputIndex >
		change.AnchorOutputIndex

	for attempt := uint32(0); attempt < 10_000; attempt++ {
		receiverRoot := tapsdk.Hash(
			sha256Bytes(
				[]byte(
					fmt.Sprintf("receiver-%d", attempt),
				),
			),
		)
		changeRoot := tapsdk.Hash(
			sha256Bytes(
				[]byte(
					fmt.Sprintf("change-%d", attempt),
				),
			),
		)
		receiverPreview, receiverScript, err := previewForRoot(
			*receiver, receiverRoot,
		)
		if err != nil {
			return nil, err
		}
		changePreview, changeScript, err := previewForRoot(
			*change, changeRoot,
		)
		if err != nil {
			return nil, err
		}
		receiverBefore := bytes.Compare(
			receiverScript, changeScript,
		) < 0
		if receiverBefore != wantReceiverBefore {
			continue
		}

		result := make([]commitmentPreview, len(request.Outputs))
		for idx := range request.Outputs {
			if request.Outputs[idx].ID == receiverOutputID {
				result[idx] = receiverPreview
			} else {
				result[idx] = changePreview
			}
		}

		return result, nil
	}

	return nil, fmt.Errorf("unable to synthesize reversing previews")
}

func previewForRoot(output tapsdk.CustomAssetOutput, assetRoot tapsdk.Hash) (
	commitmentPreview, []byte, error) {

	policyRoot, internalKey, err := requestPolicyRoot(output.Anchor)
	if err != nil {
		return commitmentPreview{}, nil, err
	}
	combined := tapBranchHash(policyRoot[:], assetRoot[:])
	outputKey := txscript.ComputeTaprootOutputKey(
		internalKey, combined[:],
	)
	pkScript, err := txscript.PayToTaprootScript(outputKey)
	if err != nil {
		return commitmentPreview{}, nil, err
	}

	return commitmentPreview{
		logicalOutputID:   output.ID,
		anchorOutputIndex: output.AnchorOutputIndex,
		assetRoot:         assetRoot,
		merkleRoot:        tapsdk.Hash(combined),
	}, pkScript, nil
}

// testBitcoinInput returns one standard, asset-free VTXO suitable for carrier
// top-ups in mixed Taproot Asset preparation tests.
func testBitcoinInput(t *testing.T, operator *btcec.PublicKey,
	amount btcutil.Amount) oor.TransferInput {

	t.Helper()
	owner := testPrivateKey(t, 9)
	policy, err := arkscript.NewVTXOPolicy(owner.PubKey(), operator, 10)
	require.NoError(t, err)
	template, err := policy.Template.Encode()
	require.NoError(t, err)
	pkScript, err := policy.Template.PkScript()
	require.NoError(t, err)
	legacyTapScript, err := arkscript.VTXOTapScript(
		owner.PubKey(), operator, 10,
	)
	require.NoError(t, err)

	return oor.TransferInput{
		VTXO: &vtxo.Descriptor{
			Outpoint: wire.OutPoint{
				Hash: chainhash.Hash(
					sha256Bytes(
						[]byte("bitcoin-input"),
					),
				),
				Index: 2,
			},
			Amount:   amount,
			PkScript: pkScript,
			ClientKey: keychain.KeyDescriptor{
				KeyLocator: keychain.KeyLocator{
					Family: 1,
					Index:  9,
				},
				PubKey: owner.PubKey(),
			},
			OperatorKey:    operator,
			TapScript:      legacyTapScript,
			RelativeExpiry: 10,
			Status:         vtxo.VTXOStatusLive,
		},
		VTXOPolicyTemplate: template,
	}
}
