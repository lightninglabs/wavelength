package oor

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/stretchr/testify/require"
)

// TestTaprootAssetOORIntentValidate pins the SDK-neutral checks that run before
// a caller can reach the authenticated tap-sdk/tapd boundary.
func TestTaprootAssetOORIntentValidate(t *testing.T) {
	t.Parallel()

	recipientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	valid := TaprootAssetOORIntent{
		AssetRef:           "tapr1asset",
		AssetAmount:        21,
		ProofFile:          []byte("confirmed-proof"),
		RecipientScriptKey: recipientKey.PubKey().SerializeCompressed(),
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
			name: "missing proof",
			mutate: func(intent *TaprootAssetOORIntent) {
				intent.ProofFile = nil
			},
			wantErr: "input proof is required",
		},
		{
			name: "invalid recipient script key",
			mutate: func(intent *TaprootAssetOORIntent) {
				intent.RecipientScriptKey = []byte{
					1,
					2,
					3,
				}
			},
			wantErr: "recipient script key",
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

// TestTaprootAssetOORPreparationBindsRequest proves an adapter cannot change
// BTC value or Ark policy while inserting Taproot Asset roots.
func TestTaprootAssetOORPreparationBindsRequest(t *testing.T) {
	t.Parallel()

	policy, inputs, preparedRecipients, prepared :=
		testPreparedSubmitPackage(t)
	recipientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	requestRecipients := cloneRecipientOutputs(preparedRecipients)
	requestRecipients[0].TaprootAssetRoot = nil
	template, err := arkscript.DecodePolicyTemplate(
		requestRecipients[0].VTXOPolicyTemplate,
	)
	require.NoError(t, err)
	requestRecipients[0].PkScript, err = template.PkScript()
	require.NoError(t, err)

	request := &TaprootAssetOORPrepareRequest{
		RequestID:  "asset-request-1",
		Policy:     policy,
		Inputs:     inputs,
		Recipients: requestRecipients,
		Intent: TaprootAssetOORIntent{
			AssetRef:    "tapr1asset",
			AssetAmount: 21,
			ProofFile:   []byte("confirmed-proof"),
			RecipientScriptKey: recipientKey.PubKey().
				SerializeCompressed(),
		},
	}
	preparation := &TaprootAssetOORPreparation{
		PreparedSubmit: prepared,
		Recipients:     preparedRecipients,
	}
	require.NoError(t, preparation.Validate(request))

	valueChanged := *preparation
	valueChanged.Recipients = cloneRecipientOutputs(
		preparation.Recipients,
	)
	valueChanged.Recipients[0].Value++
	require.ErrorContains(
		t, valueChanged.Validate(request),
		"recipient 0 value changed",
	)

	policyChanged := *preparation
	policyChanged.Recipients = cloneRecipientOutputs(
		preparation.Recipients,
	)
	policyChanged.Recipients[0].VTXOPolicyTemplate = []byte("changed")
	require.ErrorContains(
		t, policyChanged.Validate(request),
		"recipient 0 policy changed",
	)
}
