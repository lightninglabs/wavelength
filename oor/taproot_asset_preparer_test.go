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
// float, the sender's carrier returns whole, and the float residual returns
// to the operator.
func TestTaprootAssetOORCarrierAllocation(t *testing.T) {
	t.Parallel()

	newRequest := func(leaseValue, inputCarrier btcutil.Amount,
		recipientUnits uint64) *TaprootAssetOORPrepareRequest {

		return &TaprootAssetOORPrepareRequest{
			Inputs: []TransferInput{
				{
					VTXO: &vtxo.Descriptor{
						Amount: inputCarrier,
						TaprootAssetRef: "tapr1" +
							"asset",
						TaprootAssetAmount: 21,
					},
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
			name:    "full send",
			request: newRequest(25_000, 5_000, 0),
			wantPlan: TaprootAssetCarrierPlan{
				SenderChange:   5_000,
				OperatorChange: 24_000,
			},
		},
		{
			name:    "partial send",
			request: newRequest(25_000, 5_000, 13),
			wantPlan: TaprootAssetCarrierPlan{
				AssetChange:    1_000,
				SenderChange:   5_000,
				OperatorChange: 23_000,
			},
		},
		{
			name:    "lease consumed exactly",
			request: newRequest(2_000, 5_000, 13),
			wantPlan: TaprootAssetCarrierPlan{
				AssetChange:  1_000,
				SenderChange: 5_000,
			},
		},
		{
			name:    "lease below the floors",
			request: newRequest(1_500, 5_000, 13),
			wantErr: "below the 2000 sat of new asset-leaf floors",
		},
		{
			name: "missing lease",
			request: func() *TaprootAssetOORPrepareRequest {
				request := newRequest(25_000, 5_000, 0)
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

// testOperatorFundedPreparation builds a complete operator-funded full-send
// preparation: an asset input, a leased float input, a floor-valued composed
// receiver, the sender's returned carrier, and the operator's float residual.
func testOperatorFundedPreparation(t *testing.T) (
	*TaprootAssetOORPrepareRequest, *TaprootAssetOORPreparation) {

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
		VTXOPolicyTemplate: inputPolicyRaw,
		TaprootAssetRoot:   &inputAssetRoot,
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

	recipients := []oortx.RecipientOutput{
		receiver,
		{
			PkScript:           senderChangePkScript,
			Value:              assetCarrier,
			VTXOPolicyTemplate: senderChangePolicy,
		},
		{
			PkScript:           bytes.Clone(lease.PkScript),
			Value:              leaseValue - outputFloor,
			VTXOPolicyTemplate: bytes.Clone(lease.PolicyTemplate),
		},
	}

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
			InputVTXOOutpoint: assetInput.VTXO.Outpoint,
			AssetRef:          "asset-id:010203",
			AssetAmount:       21,
			ProofFile:         []byte("confirmed-proof"),
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

	request, preparation := testOperatorFundedPreparation(t)
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
	mismatchedInput.Intent.InputVTXOOutpoint.Index++
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
}
