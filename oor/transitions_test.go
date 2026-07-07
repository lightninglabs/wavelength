package oor

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	oortx "github.com/lightninglabs/darepo-client/lib/tx/oor"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// TestBuildSubmitPackagePreservesCustomSpendTxContext asserts that custom OOR
// spends propagate their locktime and CLTV sequence fallback to each tx.
func TestBuildSubmitPackagePreservesCustomSpendTxContext(t *testing.T) {
	t.Parallel()

	senderKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	receiverKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

	preimage := lntypes.Preimage{1, 2, 3}
	refundLocktime := uint32(500)
	vhtlcPolicy, err := arkscript.NewVHTLCPolicy(arkscript.VHTLCOpts{
		Sender:                               senderKey.PubKey(),
		Receiver:                             receiverKey.PubKey(),
		Server:                               operatorKey.PubKey(),
		PreimageHash:                         preimage.Hash(),
		RefundLocktime:                       refundLocktime,
		UnilateralClaimDelay:                 144,
		UnilateralRefundDelay:                144,
		UnilateralRefundWithoutReceiverDelay: 144,
	})
	require.NoError(t, err)

	refundPath, err := vhtlcPolicy.RefundWithoutReceiverPath()
	require.NoError(t, err)
	require.Equal(t, refundLocktime, refundPath.RequiredLockTime)
	require.Equal(t, wire.MaxTxInSequenceNum-1,
		refundPath.RequiredSequence)

	policyTemplate, err := vhtlcPolicy.Template.Encode()
	require.NoError(t, err)

	pkScript, err := vhtlcPolicy.PkScript()
	require.NoError(t, err)

	ownerLeafPolicy := findOwnerLeafPolicyForSpendPath(
		t, vhtlcPolicy.Template, refundPath,
	)

	locktimeOnlyRefundPath := *refundPath
	locktimeOnlyRefundPath.RequiredSequence = 0
	locktimeOnlyWitness := locktimeOnlyRefundPath.WitnessScript
	expectedSequence := wire.MaxTxInSequenceNum - 1

	inputs := []TransferInput{
		{
			VTXO: &vtxo.Descriptor{
				Outpoint: wire.OutPoint{
					Hash: chainhash.Hash{
						1,
					},
					Index: 0,
				},
				Amount:   btcutil.Amount(50_000),
				PkScript: pkScript,
				ClientKey: keychain.KeyDescriptor{
					PubKey: senderKey.PubKey(),
				},
				OperatorKey:    operatorKey.PubKey(),
				PolicyTemplate: policyTemplate,
				RelativeExpiry: 10,
			},
			VTXOPolicyTemplate: policyTemplate,
			OwnerLeafScript:    locktimeOnlyWitness,
			OwnerLeafPolicy:    ownerLeafPolicy,
			CustomSpend:        &locktimeOnlyRefundPath,
		},
	}

	arkPSBT, checkpointPSBTs, err := buildSubmitPackage(
		policy, inputs, []oortx.RecipientOutput{
			{
				PkScript: newTestTaprootPkScript(
					t, senderKey.PubKey(),
				),
				Value: btcutil.Amount(50_000),
			},
		},
	)
	require.NoError(t, err)
	require.Len(t, checkpointPSBTs, 1)
	require.Equal(
		t, refundPath.RequiredLockTime,
		checkpointPSBTs[0].UnsignedTx.LockTime,
	)
	require.Len(t, checkpointPSBTs[0].UnsignedTx.TxIn, 1)
	require.Equal(
		t, expectedSequence,
		checkpointPSBTs[0].UnsignedTx.TxIn[0].Sequence,
	)
	require.Equal(
		t, refundPath.RequiredLockTime, arkPSBT.UnsignedTx.LockTime,
	)
	require.Len(t, arkPSBT.UnsignedTx.TxIn, 1)
	require.Equal(t, expectedSequence,
		arkPSBT.UnsignedTx.TxIn[0].Sequence)
}

// findOwnerLeafPolicyForSpendPath returns the semantic leaf policy whose
// compiled script matches a spend path's witness script.
func findOwnerLeafPolicyForSpendPath(t *testing.T,
	template *arkscript.PolicyTemplate,
	spendPath *arkscript.SpendPath) []byte {

	t.Helper()

	for _, leaf := range template.Leaves {
		script, err := leaf.Script()
		require.NoError(t, err)

		if !bytes.Equal(script, spendPath.WitnessScript) {
			continue
		}

		encoded, err := leaf.Encode()
		require.NoError(t, err)

		return encoded
	}

	t.Fatalf("owner leaf policy not found")

	return nil
}
