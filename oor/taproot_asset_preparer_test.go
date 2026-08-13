package oor

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	"github.com/lightninglabs/wavelength/vtxo"
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

// TestTaprootAssetOORCarrierAllocation pins the split between the asset
// change carrier and plain Bitcoin change, including the fold of a sub-floor
// remainder into the asset change carrier.
func TestTaprootAssetOORCarrierAllocation(t *testing.T) {
	t.Parallel()

	newRequest := func(carrier uint64,
		inputAmounts ...btcutil.Amount) *TaprootAssetOORPrepareRequest {

		inputs := make([]TransferInput, len(inputAmounts))
		for idx := range inputAmounts {
			inputs[idx] = TransferInput{
				VTXO: &vtxo.Descriptor{
					Amount: inputAmounts[idx],
				},
			}
		}

		return &TaprootAssetOORPrepareRequest{
			Inputs: inputs,
			Recipients: []oortx.RecipientOutput{{
				Value: 10_000,
			}},
			OutputFloor: 1_000,
			Intent: TaprootAssetOORIntent{
				AssetChangeCarrierValueSat: carrier,
			},
		}
	}

	tests := []struct {
		name              string
		request           *TaprootAssetOORPrepareRequest
		wantAssetChange   btcutil.Amount
		wantBitcoinChange btcutil.Amount
		wantErr           string
	}{
		{
			name:    "exact full send",
			request: newRequest(0, 10_000),
		},
		{
			name:              "partial send returns the remainder",
			request:           newRequest(1_000, 10_000, 90_000),
			wantAssetChange:   1_000,
			wantBitcoinChange: 89_000,
		},
		{
			name:            "sub-floor remainder folds in",
			request:         newRequest(1_000, 10_000, 1_500),
			wantAssetChange: 1_500,
		},
		{
			name:    "sub-floor full-send remainder is refused",
			request: newRequest(0, 10_500),
			wantErr: "Bitcoin change is below the output floor",
		},
		{
			name:    "insufficient carrier funding",
			request: newRequest(1_000, 10_000),
			wantErr: "carrier funding is insufficient",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			assetChange, bitcoinChange, err := test.request.
				CarrierAllocation()
			if test.wantErr != "" {
				require.ErrorContains(t, err, test.wantErr)

				return
			}
			require.NoError(t, err)
			require.Equal(t, test.wantAssetChange, assetChange)
			require.Equal(t, test.wantBitcoinChange, bitcoinChange)
		})
	}
}

// TestTaprootAssetOORPreparationBindsRequest proves an adapter cannot change
// BTC value or Ark policy while inserting Taproot Asset roots.
func TestTaprootAssetOORPreparationBindsRequest(t *testing.T) {
	t.Parallel()

	policy, inputs, preparedRecipients, prepared :=
		testPreparedSubmitPackage(t)
	requestRecipients := cloneRecipientOutputs(preparedRecipients)
	requestRecipients[0].TaprootAssetRoot = nil
	requestRecipients[0].TaprootAssetRef = ""
	requestRecipients[0].TaprootAssetAmount = 0
	template, err := arkscript.DecodePolicyTemplate(
		requestRecipients[0].VTXOPolicyTemplate,
	)
	require.NoError(t, err)
	requestRecipients[0].PkScript, err = template.PkScript()
	require.NoError(t, err)

	request := &TaprootAssetOORPrepareRequest{
		RequestID:   "asset-request-1",
		Policy:      policy,
		Inputs:      inputs,
		Recipients:  requestRecipients,
		OutputFloor: 1_000,
		Intent: TaprootAssetOORIntent{
			InputVTXOOutpoint: inputs[0].VTXO.Outpoint,
			AssetRef:          "asset-id:010203",
			AssetAmount:       21,
			ProofFile:         []byte("confirmed-proof"),
		},
	}
	preparation := &TaprootAssetOORPreparation{
		PreparedSubmit: prepared,
		Recipients:     preparedRecipients,
		Receiver:       preparedRecipients[0],
	}
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
}
