package oor

import (
	"bytes"
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// TestTaprootAssetOORIntentValidate pins the SDK-neutral checks that run before
// a caller can reach the authenticated tap-sdk/tapd boundary.
func TestTaprootAssetOORIntentValidate(t *testing.T) {
	t.Parallel()

	valid := TaprootAssetOORIntent{
		AssetRef:    "tapr1asset",
		AssetAmount: 21,
		ProofFile:   []byte("confirmed-proof"),
	}
	require.NoError(t, valid.Validate())

	tests := []struct {
		name    string
		mutate  func(*TaprootAssetOORIntent)
		wantErr string
	}{
		{
			name: "missing asset ref",
			mutate: func(intent *TaprootAssetOORIntent) {
				intent.AssetRef = ""
			},
			wantErr: "asset ref is required",
		},
		{
			name: "zero asset amount",
			mutate: func(intent *TaprootAssetOORIntent) {
				intent.AssetAmount = 0
			},
			wantErr: "asset amount is required",
		},
		{
			name: "deprecated recipient script key",
			mutate: func(intent *TaprootAssetOORIntent) {
				intent.RecipientScriptKey = []byte{
					1,
				}
			},
			wantErr: "deprecated and must be empty",
		},
		{
			name: "nonzero change carrier",
			mutate: func(intent *TaprootAssetOORIntent) {
				intent.RecipientAssetAmount = 1
				intent.AssetChangeCarrierValueSat = 1_000
			},
			wantErr: "carriers are operator-funded",
		},
		{
			name: "malformed proof courier",
			mutate: func(intent *TaprootAssetOORIntent) {
				intent.ProofCourierAddress = "://missing-scheme"
			},
			wantErr: "proof courier address is invalid",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			intent := valid
			intent.ProofFile = bytes.Clone(valid.ProofFile)
			intent.RecipientScriptKey = bytes.Clone(
				valid.RecipientScriptKey,
			)
			test.mutate(&intent)

			require.ErrorContains(
				t, intent.Validate(), test.wantErr,
			)
		})
	}
}

// TestTaprootAssetOORCarrierAllocation pins the operator-funded carrier
// arithmetic: every new asset leaf is the operator floor out of the leased
// float, a round-created input's carrier returns to the sender, and an
// OOR-created input's carrier is reclaimed into the operator change on top
// of the float residual.
func TestTaprootAssetOORCarrierAllocation(t *testing.T) {
	t.Parallel()

	newRequest := func(leaseValue, inputCarrier btcutil.Amount,
		recipientUnits uint64,
		roundCreated bool) *TaprootAssetOORPrepareRequest {

		return &TaprootAssetOORPrepareRequest{
			Inputs: []TransferInput{
				{
					VTXO: &vtxo.Descriptor{
						Amount: inputCarrier,
						TaprootAssetRef: "tapr1" +
							"asset",
						TaprootAssetAmount: 21,
					},
					TaprootAssetRoundCreated: roundCreated,
				},
				{
					VTXO: &vtxo.Descriptor{
						Amount: leaseValue,
					},
					OperatorFunded: true,
				},
			},
			Recipients: []oortx.RecipientOutput{{
				Value: 1_000,
			}},
			OutputFloor: 1_000,
			Intent: TaprootAssetOORIntent{
				AssetRef:             "tapr1asset",
				AssetAmount:          21,
				RecipientAssetAmount: recipientUnits,
			},
			Lease: &OORCarrierLease{
				Outpoint: wire.OutPoint{
					Hash: [32]byte{
						0xf0,
					},
					Index: 0,
				},
				Value: leaseValue,
				PolicyTemplate: []byte{
					0x01,
				},
				PkScript: []byte{
					0x02,
				},
			},
		}
	}

	tests := []struct {
		name     string
		request  *TaprootAssetOORPrepareRequest
		wantPlan TaprootAssetCarrierPlan
		wantErr  string
	}{
		{
			name:    "full send of a round-created leaf",
			request: newRequest(25_000, 5_000, 0, true),
			wantPlan: TaprootAssetCarrierPlan{
				SenderChange:   5_000,
				OperatorChange: 24_000,
			},
		},
		{
			name:    "partial send of a round-created leaf",
			request: newRequest(25_000, 5_000, 13, true),
			wantPlan: TaprootAssetCarrierPlan{
				AssetChange:    1_000,
				SenderChange:   5_000,
				OperatorChange: 23_000,
			},
		},
		{
			name:    "full send reclaims an OOR-created carrier",
			request: newRequest(25_000, 5_000, 0, false),
			wantPlan: TaprootAssetCarrierPlan{
				OperatorChange: 29_000,
			},
		},
		{
			name:    "partial send reclaims an OOR-created carrier",
			request: newRequest(25_000, 5_000, 13, false),
			wantPlan: TaprootAssetCarrierPlan{
				AssetChange:    1_000,
				OperatorChange: 28_000,
			},
		},
		{
			name:    "lease consumed exactly",
			request: newRequest(2_000, 5_000, 13, true),
			wantPlan: TaprootAssetCarrierPlan{
				AssetChange:  1_000,
				SenderChange: 5_000,
			},
		},
		{
			name:    "lease below the floors",
			request: newRequest(1_500, 5_000, 13, true),
			wantErr: "below the 2000 sat of new asset-leaf floors",
		},
		{
			name: "missing lease",
			request: func() *TaprootAssetOORPrepareRequest {
				request := newRequest(25_000, 5_000, 0, true)
				request.Lease = nil

				return request
			}(),
			wantErr: "carrier lease must be provided",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			plan, err := test.request.CarrierAllocation()
			if test.wantErr != "" {
				require.ErrorContains(t, err, test.wantErr)

				return
			}
			require.NoError(t, err)
			require.Equal(t, test.wantPlan, plan)
		})
	}
}

// TestTaprootAssetOORCarrierAllocationMultiInput proves the carrier plan sums
// per-input origins across an atomic multi-input send: round-created carriers
// return to the sender, OOR-created carriers are reclaimed by the operator.
func TestTaprootAssetOORCarrierAllocationMultiInput(t *testing.T) {
	t.Parallel()

	request := &TaprootAssetOORPrepareRequest{
		Inputs: []TransferInput{
			{
				VTXO: &vtxo.Descriptor{
					Amount:             5_000,
					TaprootAssetRef:    "tapr1asset",
					TaprootAssetAmount: 200,
				},
				TaprootAssetRoundCreated: true,
			},
			{
				VTXO: &vtxo.Descriptor{
					Amount:             4_000,
					TaprootAssetRef:    "tapr1asset",
					TaprootAssetAmount: 50,
				},
			},
			{
				VTXO: &vtxo.Descriptor{
					Amount: 25_000,
				},
				OperatorFunded: true,
			},
		},
		Recipients: []oortx.RecipientOutput{{
			Value: 1_000,
		}},
		OutputFloor: 1_000,
		Intent: TaprootAssetOORIntent{
			AssetRef:             "tapr1asset",
			AssetAmount:          250,
			RecipientAssetAmount: 240,
		},
		Lease: &OORCarrierLease{
			Outpoint: wire.OutPoint{
				Hash: [32]byte{
					0xf0,
				},
			},
			Value: 25_000,
			PolicyTemplate: []byte{
				0x01,
			},
			PkScript: []byte{
				0x02,
			},
		},
	}

	plan, err := request.CarrierAllocation()
	require.NoError(t, err)
	require.Equal(t, TaprootAssetCarrierPlan{
		AssetChange:    1_000,
		SenderChange:   5_000,
		OperatorChange: 25_000 - 2_000 + 4_000,
	}, plan)
}

// TestTaprootAssetOORAssetInputIndices pins the ordered multi-input contract:
// the intent's outpoint list names every asset input, spine first, in the
// inputs' own relative order, and the per-input amounts sum to the total.
func TestTaprootAssetOORAssetInputIndices(t *testing.T) {
	t.Parallel()

	request, _ := testOperatorFundedPreparation(t, true)
	second := request.Inputs[0]
	secondVTXO := *second.VTXO
	secondVTXO.Outpoint.Index++
	secondVTXO.TaprootAssetAmount = 13
	second.VTXO = &secondVTXO
	second.TaprootAssetRoundCreated = false

	// Inputs are [spine, co, float]; the fixture's float stays last.
	request.Inputs = []TransferInput{
		request.Inputs[0], second, request.Inputs[1],
	}
	request.Intent.InputVTXOOutpoints = []wire.OutPoint{
		request.Inputs[0].VTXO.Outpoint,
		second.VTXO.Outpoint,
	}
	request.Intent.AssetAmount = 21 + 13

	// Multi-input sends resolve every input from its sealed local package;
	// the caller-supplied proof file stays a single-input affordance.
	request.Intent.ProofFile = nil
	require.NoError(t, request.Validate())

	indices, err := request.AssetInputIndices()
	require.NoError(t, err)
	require.Equal(t, []int{0, 1}, indices)

	reversed := *request
	reversed.Intent.InputVTXOOutpoints = []wire.OutPoint{
		second.VTXO.Outpoint,
		request.Inputs[0].VTXO.Outpoint,
	}
	_, err = reversed.AssetInputIndices()
	require.ErrorContains(t, err, "not in intent order")

	missing := *request
	missing.Intent.InputVTXOOutpoints = []wire.OutPoint{
		request.Inputs[0].VTXO.Outpoint,
	}
	_, err = missing.AssetInputIndices()
	require.ErrorContains(t, err, "asset inputs, the intent names")

	sumDrift := *request
	sumDrift.Intent.AssetAmount++
	_, err = sumDrift.AssetInputIndices()
	require.ErrorContains(t, err, "input amount does not match")

	proofFile := *request
	proofFile.Intent.ProofFile = []byte("confirmed-proof")
	require.ErrorContains(
		t, proofFile.Validate(),
		"requires a single pinned input",
	)

	duplicated := *request
	duplicated.Intent.InputVTXOOutpoints = []wire.OutPoint{
		request.Inputs[0].VTXO.Outpoint,
		request.Inputs[0].VTXO.Outpoint,
	}
	require.ErrorContains(t, duplicated.Validate(), "is duplicated")

	overCap := *request
	overCap.Intent.InputVTXOOutpoints = make(
		[]wire.OutPoint, MaxTaprootAssetInputs+1,
	)
	for idx := range overCap.Intent.InputVTXOOutpoints {
		overCap.Intent.InputVTXOOutpoints[idx] = wire.OutPoint{
			Index: uint32(idx),
		}
	}
	require.ErrorContains(t, overCap.Validate(), "consolidate first")

	bitcoinInput := *request
	plainVTXO := *second.VTXO
	plainVTXO.Outpoint.Index += 10
	plainVTXO.TaprootAssetRef = ""
	plainVTXO.TaprootAssetAmount = 0
	plainVTXO.TaprootAssetRoot = nil
	plain := second
	plain.VTXO = &plainVTXO
	plain.TaprootAssetRoot = nil
	bitcoinInput.Inputs = append(
		append(
			[]TransferInput(nil), request.Inputs...,
		),
		plain,
	)
	_, err = bitcoinInput.AssetInputIndices()
	require.ErrorContains(
		t, err, "neither an asset input nor the operator float",
	)
}

// testOperatorFundedPreparation builds a complete operator-funded full-send
// preparation: an asset input, a leased float input, a floor-valued composed
// receiver, and the operator's change. A round-created input adds the
// sender's returned carrier; an OOR-created input reclaims it into the
// operator change instead.
func testOperatorFundedPreparation(t *testing.T,
	roundCreated bool) (*TaprootAssetOORPrepareRequest,
	*TaprootAssetOORPreparation) {

	t.Helper()

	ownerKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	floatKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	const (
		outputFloor  = btcutil.Amount(1_000)
		assetCarrier = btcutil.Amount(5_000)
		leaseValue   = btcutil.Amount(3_000)
	)

	// The asset input mirrors the wallet-managed descriptor shape.
	inputPolicy, err := arkscript.NewVTXOPolicy(
		ownerKey.PubKey(), operatorKey.PubKey(), 10,
	)
	require.NoError(t, err)
	inputPolicyRaw, err := inputPolicy.Template.Encode()
	require.NoError(t, err)
	inputAssetRoot := chainhash.Hash{1, 2, 3}
	inputComposed, err := arkscript.ComposeWithSiblingRoot(
		inputPolicy.CompiledPolicy, inputAssetRoot,
	)
	require.NoError(t, err)
	inputPkScript, err := txscript.PayToTaprootScript(
		inputComposed.OutputKey(),
	)
	require.NoError(t, err)
	inputTapScript, err := arkscript.VTXOTapScript(
		ownerKey.PubKey(), operatorKey.PubKey(), 10,
	)
	require.NoError(t, err)

	assetInput := TransferInput{
		VTXO: &vtxo.Descriptor{
			Outpoint: wire.OutPoint{
				Hash: chainhash.Hash{
					9,
					8,
					7,
				},
				Index: 1,
			},
			Amount:   assetCarrier,
			PkScript: inputPkScript,
			ClientKey: keychain.KeyDescriptor{
				KeyLocator: keychain.KeyLocator{
					Family: 1,
					Index:  2,
				},
				PubKey: ownerKey.PubKey(),
			},
			OperatorKey:        operatorKey.PubKey(),
			TapScript:          inputTapScript,
			RelativeExpiry:     10,
			Status:             vtxo.VTXOStatusLive,
			TaprootAssetRoot:   &inputAssetRoot,
			TaprootAssetRef:    "asset-id:010203",
			TaprootAssetAmount: 21,
		},
		VTXOPolicyTemplate:       inputPolicyRaw,
		TaprootAssetRoot:         &inputAssetRoot,
		TaprootAssetRoundCreated: roundCreated,
	}

	floatInput := newTestOperatorFundedInput(
		t, floatKey, operatorKey.PubKey(), wire.OutPoint{
			Hash:  chainhash.Hash{0xf0},
			Index: 0,
		},
		leaseValue,
	)
	lease := &OORCarrierLease{
		Outpoint:       floatInput.VTXO.Outpoint,
		Value:          leaseValue,
		PolicyTemplate: bytes.Clone(floatInput.VTXOPolicyTemplate),
		PkScript:       bytes.Clone(floatInput.VTXO.PkScript),
	}

	inputs := []TransferInput{assetInput, floatInput}
	require.NoError(t, NormalizeCheckpointOwnerLeaves(policy, inputs))

	// The receiver is a floor-valued asset leaf; the request carries its
	// uncomposed shape and the preparation its composed one.
	recipientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	recipientPolicy, err := arkscript.NewVTXOPolicy(
		recipientKey.PubKey(), operatorKey.PubKey(), 10,
	)
	require.NoError(t, err)
	recipientPolicyRaw, err := recipientPolicy.Template.Encode()
	require.NoError(t, err)
	recipientAssetRoot := chainhash.Hash{4, 5, 6}
	recipientComposed, err := arkscript.ComposeWithSiblingRoot(
		recipientPolicy.CompiledPolicy, recipientAssetRoot,
	)
	require.NoError(t, err)
	recipientPkScript, err := txscript.PayToTaprootScript(
		recipientComposed.OutputKey(),
	)
	require.NoError(t, err)
	receiver := oortx.RecipientOutput{
		PkScript:           recipientPkScript,
		Value:              outputFloor,
		VTXOPolicyTemplate: recipientPolicyRaw,
		TaprootAssetRoot:   &recipientAssetRoot,
		TaprootAssetRef:    "asset-id:010203",
		TaprootAssetAmount: 21,
	}
	requestReceiver := receiver
	requestReceiver.TaprootAssetRoot = nil
	requestReceiver.TaprootAssetRef = ""
	requestReceiver.TaprootAssetAmount = 0
	requestReceiver.PkScript, err = recipientPolicy.Template.PkScript()
	require.NoError(t, err)

	senderChangeKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	senderChangePolicy, senderChangePkScript, err := arkscript.
		EncodeStandardVTXOArtifacts(
			senderChangeKey.PubKey(), operatorKey.PubKey(), 10,
		)
	require.NoError(t, err)

	operatorChange := leaseValue - outputFloor
	if !roundCreated {
		operatorChange += assetCarrier
	}
	recipients := []oortx.RecipientOutput{receiver}
	if roundCreated {
		recipients = append(recipients, oortx.RecipientOutput{
			PkScript:           senderChangePkScript,
			Value:              assetCarrier,
			VTXOPolicyTemplate: senderChangePolicy,
		})
	}
	recipients = append(recipients, oortx.RecipientOutput{
		PkScript:           bytes.Clone(lease.PkScript),
		Value:              operatorChange,
		VTXOPolicyTemplate: bytes.Clone(lease.PolicyTemplate),
	})

	ark, checkpoints, err := BuildSubmitPackage(policy, inputs, recipients)
	require.NoError(t, err)
	checkpointPackages := make([][]byte, len(inputs))
	checkpointPackages[0] = []byte("checkpoint-package")
	prepared := &PreparedSubmitPackage{
		ArkPSBT:         ark,
		CheckpointPSBTs: checkpoints,
		TaprootAssetTransfer: &oortx.TaprootAssetTransfer{
			Version:            oortx.TaprootAssetTransferVersion,
			CheckpointPackages: checkpointPackages,
			ArkPackage:         []byte("ark-package"),
		},
	}

	request := &TaprootAssetOORPrepareRequest{
		RequestID: "asset-request-1",
		Policy:    policy,
		Inputs:    inputs,
		Recipients: []oortx.RecipientOutput{
			requestReceiver,
		},
		OutputFloor: outputFloor,
		BuildChangeRecipient: func(_ context.Context,
			change btcutil.Amount) (oortx.RecipientOutput, error) {

			return oortx.RecipientOutput{
				PkScript:           senderChangePkScript,
				Value:              change,
				VTXOPolicyTemplate: senderChangePolicy,
			}, nil
		},
		Intent: TaprootAssetOORIntent{
			InputVTXOOutpoints: []wire.OutPoint{
				assetInput.VTXO.Outpoint,
			},
			AssetRef:    "asset-id:010203",
			AssetAmount: 21,
			ProofFile:   []byte("confirmed-proof"),
		},
		Lease: lease,
	}
	preparation := &TaprootAssetOORPreparation{
		PreparedSubmit: prepared,
		Recipients:     recipients,
		Receiver:       recipients[0],
	}

	return request, preparation
}

// TestTaprootAssetOORPreparationBindsRequest proves an adapter cannot change
// carrier values, the operator's float residual, or Ark policy while
// inserting Taproot Asset roots.
func TestTaprootAssetOORPreparationBindsRequest(t *testing.T) {
	t.Parallel()

	request, preparation := testOperatorFundedPreparation(t, true)
	require.NoError(t, request.Validate())
	require.NoError(t, preparation.Validate(request))

	tooManyInputs := *request
	tooManyInputs.Inputs = make(
		[]TransferInput, oortx.MaxTaprootAssetCheckpointPackages+1,
	)
	for idx := range tooManyInputs.Inputs {
		tooManyInputs.Inputs[idx] = request.Inputs[0]
	}
	require.ErrorContains(
		t, tooManyInputs.Validate(),
		"input count 65 exceeds 64",
	)

	duplicateInput := *request
	duplicateInput.Inputs = append(
		[]TransferInput(nil), request.Inputs...,
	)
	duplicateInput.Inputs = append(
		duplicateInput.Inputs, request.Inputs[0],
	)
	require.ErrorContains(
		t, duplicateInput.Validate(),
		"duplicate outpoint",
	)

	mismatchedInput := *request
	mismatchedOutpoint := request.Inputs[0].VTXO.Outpoint
	mismatchedOutpoint.Index++
	mismatchedInput.Intent.InputVTXOOutpoints = []wire.OutPoint{
		mismatchedOutpoint,
	}
	require.ErrorContains(
		t, mismatchedInput.Validate(),
		"does not match",
	)

	receiverAboveFloor := *request
	receiverAboveFloor.Recipients = cloneRecipientOutputs(
		request.Recipients,
	)
	receiverAboveFloor.Recipients[0].Value++
	require.ErrorContains(
		t, receiverAboveFloor.Validate(),
		"must equal the operator floor",
	)

	missingFloat := *request
	missingFloat.Inputs = request.Inputs[:1]
	require.ErrorContains(
		t, missingFloat.Validate(),
		"requires an operator-funded float input",
	)

	leaseValueDrift := *request
	leaseValueDrift.Lease = request.Lease.Clone()
	leaseValueDrift.Lease.Value++
	require.ErrorContains(
		t, leaseValueDrift.Validate(),
		"float input value does not match the lease",
	)

	valueChanged := *preparation
	valueChanged.Recipients = cloneRecipientOutputs(
		preparation.Recipients,
	)
	valueChanged.Receiver.Value++
	require.ErrorContains(
		t, valueChanged.Validate(request),
		"recipient 0 value changed",
	)

	policyChanged := *preparation
	policyChanged.Recipients = cloneRecipientOutputs(
		preparation.Recipients,
	)
	policyChanged.Receiver.VTXOPolicyTemplate = []byte("changed")
	require.ErrorContains(
		t, policyChanged.Validate(request),
		"recipient 0 policy changed",
	)

	operatorChangeDrift := *preparation
	operatorChangeDrift.Recipients = cloneRecipientOutputs(
		preparation.Recipients,
	)
	operatorChangeDrift.Recipients[2].Value--
	require.ErrorContains(
		t, operatorChangeDrift.Validate(request),
		"operator change value mismatch",
	)

	senderChangeDrift := *preparation
	senderChangeDrift.Recipients = cloneRecipientOutputs(
		preparation.Recipients,
	)
	senderChangeDrift.Recipients[1].Value--
	require.ErrorContains(
		t, senderChangeDrift.Validate(request),
		"sender change value mismatch",
	)

	missingOperatorChange := *preparation
	missingOperatorChange.Recipients = cloneRecipientOutputs(
		preparation.Recipients[:2],
	)
	require.ErrorContains(
		t, missingOperatorChange.Validate(request),
		"operator change is not unique",
	)

	missingSenderChange := *preparation
	missingSenderChange.Recipients = cloneRecipientOutputs(
		[]oortx.RecipientOutput{
			preparation.Recipients[0],
			preparation.Recipients[2],
		},
	)
	require.ErrorContains(
		t, missingSenderChange.Validate(request),
		"sender change is not unique",
	)

	markedFloat := *request
	markedFloat.Inputs = append([]TransferInput(nil), request.Inputs...)
	markedFloat.Inputs[1].TaprootAssetRoundCreated = true
	require.ErrorContains(
		t, markedFloat.Validate(),
		"round-created marker requires an asset-bearing vtxo",
	)
}

// TestTaprootAssetOORPreparationReclaimsOORCarrier proves an OOR-created
// leaf's operator-funded carrier is reclaimed into the operator change, and
// that drift in either direction (wrong reclaimed value, or a sender output
// that must not exist) is rejected.
func TestTaprootAssetOORPreparationReclaimsOORCarrier(t *testing.T) {
	t.Parallel()

	request, preparation := testOperatorFundedPreparation(t, false)
	require.NoError(t, request.Validate())
	require.NoError(t, preparation.Validate(request))

	// The fixture's lease is 3_000 sat, one floor of 1_000 sat funds the
	// receiver leaf, and the 5_000 sat carrier is reclaimed.
	plan, err := request.CarrierAllocation()
	require.NoError(t, err)
	require.Zero(t, plan.SenderChange)
	require.Equal(t, btcutil.Amount(7_000), plan.OperatorChange)
	require.Len(t, preparation.Recipients, 2)
	require.Equal(
		t, plan.OperatorChange, preparation.Recipients[1].Value,
	)

	reclaimDrift := *preparation
	reclaimDrift.Recipients = cloneRecipientOutputs(
		preparation.Recipients,
	)
	reclaimDrift.Recipients[1].Value--
	require.ErrorContains(
		t, reclaimDrift.Validate(request),
		"operator change value mismatch",
	)

	straySenderChange := *preparation
	straySenderChange.Recipients = cloneRecipientOutputs(
		preparation.Recipients,
	)
	stray, err := request.BuildChangeRecipient(
		t.Context(), btcutil.Amount(5_000),
	)
	require.NoError(t, err)
	straySenderChange.Recipients = append(
		straySenderChange.Recipients, stray,
	)
	require.ErrorContains(
		t, straySenderChange.Validate(request),
		"sender change must be absent",
	)
}
