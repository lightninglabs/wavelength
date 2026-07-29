package oor

import (
	"bytes"
	"context"
	"fmt"
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

func TestPreparedSubmitSurvivesStartSnapshotAndRetry(t *testing.T) {
	t.Parallel()

	policy, inputs, recipients, prepared := testPreparedSubmitPackage(t)

	var encodedStart bytes.Buffer
	start := &StartTransferRequest{
		Policy:         policy,
		Inputs:         inputs,
		Recipients:     recipients,
		IdempotencyKey: "asset-send-1",
		PreparedSubmit: prepared,
	}
	require.NoError(t, start.Encode(&encodedStart))
	var decodedStart StartTransferRequest
	require.NoError(t, decodedStart.Decode(&encodedStart))
	require.NotNil(t, decodedStart.PreparedSubmit)
	require.Equal(
		t, prepared.TaprootAssetTransfer,
		decodedStart.PreparedSubmit.TaprootAssetTransfer,
	)

	ctx := context.Background()
	session, outbox, err := NewPreparedSessionWithIdempotencyKey(
		ctx, policy, inputs, recipients, "asset-send-1", prepared,
	)
	require.NoError(t, err)
	t.Cleanup(session.FSM.Stop)
	require.Len(t, outbox, 1)
	_, ok := outbox[0].(*RequestArkSignatures)
	require.True(t, ok)

	result := session.FSM.AskEvent(ctx, &ArkSignedEvent{
		ArkPSBT: prepared.ArkPSBT,
	}).Await(ctx)
	require.NoError(t, result.Err())
	submitOutbox := result.UnwrapOr(nil)
	require.Len(t, submitOutbox, 2)
	submit, ok := submitOutbox[0].(*SendSubmitPackageRequest)
	require.True(t, ok)
	require.Equal(
		t, prepared.TaprootAssetTransfer, submit.TaprootAssetTransfer,
	)

	state, err := session.FSM.CurrentState()
	require.NoError(t, err)
	outgoingState, ok := state.(State)
	require.True(t, ok)
	snapshot, err := NewOutgoingSnapshot(session.ID, outgoingState)
	require.NoError(t, err)
	require.NotEmpty(t, snapshot.TaprootAssetTransfer)
	require.Equal(t, recipients, snapshot.RecipientOutputs)
	rawSnapshot, err := encodeOutgoingSnapshot(snapshot)
	require.NoError(t, err)
	decodedSnapshot, err := decodeOutgoingSnapshot(rawSnapshot)
	require.NoError(t, err)
	restored, err := OutgoingStateFromSnapshot(decodedSnapshot)
	require.NoError(t, err)
	retryOutbox, err := OutboxForState(restored)
	require.NoError(t, err)
	require.Len(t, retryOutbox, 2)
	retrySubmit, ok := retryOutbox[0].(*SendSubmitPackageRequest)
	require.True(t, ok)
	require.Equal(
		t, prepared.TaprootAssetTransfer,
		retrySubmit.TaprootAssetTransfer,
	)
	require.Equal(t, recipients, retrySubmit.Recipients)

	mismatchedTransfer := prepared.TaprootAssetTransfer.Clone()
	mismatchedTransfer.CheckpointPackages = append(
		mismatchedTransfer.CheckpointPackages, []byte("extra"),
	)
	decodedSnapshot.TaprootAssetTransfer, err =
		mismatchedTransfer.MarshalBinary()
	require.NoError(t, err)
	_, err = OutgoingStateFromSnapshot(decodedSnapshot)
	require.ErrorContains(t, err, "does not match checkpoint count")
}

func TestPreparedSubmitRejectsMismatchedAssetMetadata(t *testing.T) {
	t.Parallel()

	_, inputs, recipients, prepared := testPreparedSubmitPackage(t)

	wrongRoot := *inputs[0].TaprootAssetRoot
	wrongRoot[0] ^= 1
	inputs[0].TaprootAssetRoot = &wrongRoot
	inputs[0].VTXO.TaprootAssetRoot = &wrongRoot
	err := prepared.Validate(inputs, recipients)
	require.ErrorContains(t, err, "asset root and vtxo pkscript mismatch")

	_, inputs, recipients, prepared = testPreparedSubmitPackage(t)
	prepared.TaprootAssetTransfer.CheckpointPackages = append(
		prepared.TaprootAssetTransfer.CheckpointPackages,
		[]byte("extra"),
	)
	err = prepared.Validate(inputs, recipients)
	require.ErrorContains(t, err, "does not match checkpoint count")
}

// TestPreparedSubmitAcceptsMixedBitcoinInputs pins positional empty checkpoint
// slots for an asset input combined with zero, one, or several ordinary VTXOs.
func TestPreparedSubmitAcceptsMixedBitcoinInputs(t *testing.T) {
	t.Parallel()

	assetVersion := oortx.TaprootAssetTransferVersion
	for _, bitcoinInputs := range []int{0, 1, 3} {
		bitcoinInputs := bitcoinInputs
		t.Run(fmt.Sprintf("bitcoin_inputs_%d", bitcoinInputs),
			func(t *testing.T) {
				t.Parallel()

				policy, inputs, recipients, _ :=
					testPreparedSubmitPackage(t)
				inputs, recipients = appendBitcoinPreparedEdges(
					t, policy, inputs, recipients,
					bitcoinInputs,
				)
				ark, checkpoints, err := BuildSubmitPackage(
					policy, inputs, recipients,
				)
				require.NoError(t, err)

				slots := make([][]byte, len(inputs))
				slots[0] = []byte("asset-checkpoint")
				assetTransfer := &oortx.TaprootAssetTransfer{
					Version:            assetVersion,
					CheckpointPackages: slots,
					ArkPackage:         []byte("ark"),
				}
				prepared := &PreparedSubmitPackage{
					ArkPSBT:              ark,
					CheckpointPSBTs:      checkpoints,
					TaprootAssetTransfer: assetTransfer,
				}
				require.NoError(
					t,
					prepared.Validate(inputs, recipients),
				)

				if bitcoinInputs == 0 {
					return
				}
				prepared.TaprootAssetTransfer.
					CheckpointPackages[1] = []byte("wrong")
				err = prepared.Validate(inputs, recipients)
				require.ErrorContains(
					t, err, "package presence mismatch",
				)
			})
	}
}

func appendBitcoinPreparedEdges(t *testing.T, policy arkscript.CheckpointPolicy,
	inputs []TransferInput, recipients []oortx.RecipientOutput,
	count int) ([]TransferInput, []oortx.RecipientOutput) {

	t.Helper()
	for idx := range count {
		ownerKey, err := btcec.NewPrivateKey()
		require.NoError(t, err)
		inputPolicy, err := arkscript.NewVTXOPolicy(
			ownerKey.PubKey(), policy.OperatorKey, policy.CSVDelay,
		)
		require.NoError(t, err)
		inputPolicyRaw, err := inputPolicy.Template.Encode()
		require.NoError(t, err)
		inputPkScript, err := inputPolicy.Template.PkScript()
		require.NoError(t, err)
		inputTapScript, err := arkscript.VTXOTapScript(
			ownerKey.PubKey(), policy.OperatorKey, policy.CSVDelay,
		)
		require.NoError(t, err)
		value := btcutil.Amount(1_000 + idx)
		inputs = append(inputs, TransferInput{
			VTXO: &vtxo.Descriptor{
				Outpoint: wire.OutPoint{
					Hash:  chainhash.Hash{byte(idx + 20)},
					Index: uint32(idx),
				},
				Amount:   value,
				PkScript: inputPkScript,
				ClientKey: keychain.KeyDescriptor{
					PubKey: ownerKey.PubKey(),
				},
				OperatorKey:    policy.OperatorKey,
				TapScript:      inputTapScript,
				RelativeExpiry: policy.CSVDelay,
				Status:         vtxo.VTXOStatusLive,
			},
			VTXOPolicyTemplate: inputPolicyRaw,
		})

		recipientKey, err := btcec.NewPrivateKey()
		require.NoError(t, err)
		recipientPolicy, err := arkscript.NewVTXOPolicy(
			recipientKey.PubKey(), policy.OperatorKey,
			policy.CSVDelay,
		)
		require.NoError(t, err)
		recipientPolicyRaw, err := recipientPolicy.Template.Encode()
		require.NoError(t, err)
		recipientPkScript, err := recipientPolicy.Template.PkScript()
		require.NoError(t, err)
		recipients = append(recipients, oortx.RecipientOutput{
			PkScript:           recipientPkScript,
			Value:              value,
			VTXOPolicyTemplate: recipientPolicyRaw,
		})
	}

	require.NoError(t, NormalizeCheckpointOwnerLeaves(policy, inputs))

	return inputs, recipients
}

func testPreparedSubmitPackage(t *testing.T) (arkscript.CheckpointPolicy,
	[]TransferInput, []oortx.RecipientOutput, *PreparedSubmitPackage) {

	t.Helper()

	ownerKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	policy := arkscript.CheckpointPolicy{
		OperatorKey: operatorKey.PubKey(),
		CSVDelay:    10,
	}

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

	inputs := []TransferInput{{
		VTXO: &vtxo.Descriptor{
			Outpoint: wire.OutPoint{
				Hash: chainhash.Hash{
					9,
					8,
					7,
				},
				Index: 1,
			},
			Amount:   btcutil.Amount(5_000),
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
	}}
	require.NoError(t, NormalizeCheckpointOwnerLeaves(policy, inputs))

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
	recipients := []oortx.RecipientOutput{{
		PkScript:           recipientPkScript,
		Value:              5_000,
		VTXOPolicyTemplate: recipientPolicyRaw,
		TaprootAssetRoot:   &recipientAssetRoot,
		TaprootAssetRef:    "asset-id:010203",
		TaprootAssetAmount: 21,
	}}

	ark, checkpoints, err := BuildSubmitPackage(
		policy, inputs, recipients,
	)
	require.NoError(t, err)
	prepared := &PreparedSubmitPackage{
		ArkPSBT:         ark,
		CheckpointPSBTs: checkpoints,
		TaprootAssetTransfer: &oortx.TaprootAssetTransfer{
			Version: oortx.TaprootAssetTransferVersion,
			CheckpointPackages: [][]byte{
				[]byte("checkpoint"),
			},
			ArkPackage: []byte("ark"),
		},
	}
	require.NoError(t, prepared.Validate(inputs, recipients))

	return policy, inputs, recipients, prepared
}
