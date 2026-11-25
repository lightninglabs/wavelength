package oor

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/stretchr/testify/require"
)

// assetRecipient returns an asset output paying the standard policy of owner
// composed with root.
func assetRecipient(t *testing.T, owner, operator *btcec.PublicKey,
	root chainhash.Hash) RecipientOutput {

	t.Helper()

	encoded, err := arkscript.EncodeStandardVTXOTemplate(
		owner, operator, 144,
	)
	require.NoError(t, err)
	template, err := arkscript.DecodePolicyTemplate(encoded)
	require.NoError(t, err)
	policy, err := template.Compile()
	require.NoError(t, err)
	composed, err := arkscript.ComposeWithSiblingRoot(policy, root)
	require.NoError(t, err)
	pkScript, err := txscript.PayToTaprootScript(composed.OutputKey())
	require.NoError(t, err)

	return RecipientOutput{
		PkScript:           pkScript,
		Value:              1_000,
		VTXOPolicyTemplate: encoded,
		Asset: &RecipientAsset{
			Root:   root,
			Ref:    "asset",
			Amount: 60,
		},
	}
}

// TestRecipientAsset checks that an asset output must commit to its policy
// and asset root, and routes through its uncomposed policy script.
func TestRecipientAsset(t *testing.T) {
	t.Parallel()

	owner, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operator, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	output := assetRecipient(
		t, owner.PubKey(), operator.PubKey(), chainhash.Hash{0x01},
	)
	require.NoError(t, output.ValidateAsset())

	_, policyScript, err := arkscript.EncodeStandardVTXOArtifacts(
		owner.PubKey(), operator.PubKey(), 144,
	)
	require.NoError(t, err)
	routing, err := output.RoutingPkScript()
	require.NoError(t, err)
	require.Equal(t, policyScript, routing)
	require.NotEqual(t, policyScript, output.PkScript)

	plain := RecipientOutput{PkScript: policyScript, Value: 1_000}
	require.NoError(t, plain.ValidateAsset())
	routing, err = plain.RoutingPkScript()
	require.NoError(t, err)
	require.Equal(t, policyScript, routing)

	tests := []struct {
		name   string
		mutate func(output *RecipientOutput)
		err    string
	}{{
		name: "other root",
		mutate: func(output *RecipientOutput) {
			output.Asset.Root[0] ^= 2
		},
		err: "does not commit",
	}, {
		name: "uncomposed script",
		mutate: func(output *RecipientOutput) {
			output.PkScript = policyScript
		},
		err: "does not commit",
	}, {
		name: "no policy",
		mutate: func(output *RecipientOutput) {
			output.VTXOPolicyTemplate = nil
		},
		err: "policy must be provided",
	}, {
		name: "no amount",
		mutate: func(output *RecipientOutput) {
			output.Asset.Amount = 0
		},
		err: "amount must be positive",
	}, {
		name: "zero root",
		mutate: func(output *RecipientOutput) {
			output.Asset.Root = chainhash.Hash{}
		},
		err: "root must not be zero",
	}, {
		name: "no ref",
		mutate: func(output *RecipientOutput) {
			output.Asset.Ref = ""
		},
		err: "ref must be provided",
	}, {
		name: "long ref",
		mutate: func(output *RecipientOutput) {
			output.Asset.Ref = string(
				make([]byte, MaxRecipientAssetRefBytes+1),
			)
		},
		err: "ref exceeds",
	}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			output := assetRecipient(
				t, owner.PubKey(), operator.PubKey(),
				chainhash.Hash{0x01},
			)
			test.mutate(&output)
			require.ErrorContains(
				t, output.ValidateAsset(), test.err,
			)
		})
	}
}

// TestBuildArkPSBTValidatesAssets checks that the Ark builder refuses an
// asset output whose script does not commit to its asset root.
func TestBuildArkPSBTValidatesAssets(t *testing.T) {
	t.Parallel()

	owner, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operator, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	checkpoint := CheckpointOutput{
		Txid: chainhash.Hash{
			0x02,
		},
		Output: &wire.TxOut{
			Value:    1_000,
			PkScript: randomP2TRScript(t),
		},
	}
	output := assetRecipient(
		t, owner.PubKey(), operator.PubKey(), chainhash.Hash{0x01},
	)

	_, err = BuildArkPSBT(
		[]CheckpointOutput{checkpoint}, []RecipientOutput{output},
	)
	require.NoError(t, err)

	output.Asset.Root[0] ^= 2
	_, err = BuildArkPSBT(
		[]CheckpointOutput{checkpoint}, []RecipientOutput{output},
	)
	require.ErrorContains(t, err, "does not commit")
}
