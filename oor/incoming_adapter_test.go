package oor

import (
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/stretchr/testify/require"
)

// TestIsIncomingResolveCorrelationID verifies only durable incoming-transfer
// resolution query correlation IDs match the OOR resolve route prefix.
func TestIsIncomingResolveCorrelationID(t *testing.T) {
	t.Parallel()

	var sessionID SessionID
	sessionID[0] = 1

	require.True(
		t,
		IsIncomingResolveCorrelationID(
			IncomingResolveCorrelationID(sessionID, 7),
		),
	)
	require.False(t, IsIncomingResolveCorrelationID(""))
	require.False(
		t, IsIncomingResolveCorrelationID("00aa8bfb11f09881bbd2"),
	)
	require.False(
		t, IsIncomingResolveCorrelationID(
			incomingResolveCorrelationPrefix,
		),
	)
}

// TestIncomingRecipientsFromEventBindsTaprootAssetRoot verifies the offline
// receive adapter rejects roots that do not derive the announced Ark output.
func TestIncomingRecipientsFromEventBindsTaprootAssetRoot(t *testing.T) {
	t.Parallel()

	arkPSBT, _, recipients, _, recipientKey, operatorKey :=
		buildTestIncomingMaterialization(t)
	template, err := arkscript.EncodeStandardVTXOTemplate(
		recipientKey.PubKey(), operatorKey, 10,
	)
	require.NoError(t, err)
	assetRoot := chainhash.Hash{0x71, 0x72, 0x73}
	assetDesc := &vtxo.Descriptor{
		PolicyTemplate:   template,
		TaprootAssetRoot: &assetRoot,
	}
	assetPkScript, err := assetDesc.EffectivePkScript()
	require.NoError(t, err)
	arkPSBT.UnsignedTx.TxOut[recipients[0].OutputIndex].PkScript =
		assetPkScript

	evt := &arkrpc.OORRecipientEvent{
		RecipientPkScript:  assetPkScript,
		OutputIndex:        recipients[0].OutputIndex,
		Value:              uint64(recipients[0].Value),
		VtxoPolicyTemplate: template,
		TaprootAssetRoot:   assetRoot[:],
		TaprootAssetRef:    "asset-id:010203",
		TaprootAssetAmount: 21,
	}
	decoded, err := incomingRecipientsFromEvent(arkPSBT, evt)
	require.NoError(t, err)
	require.Equal(t, &assetRoot,
		decoded[0].TaprootAssetRoot)
	require.Equal(t, "asset-id:010203", decoded[0].TaprootAssetRef)
	require.Equal(t, uint64(21), decoded[0].TaprootAssetAmount)

	wrongRoot := chainhash.Hash{0xff}
	evt.TaprootAssetRoot = wrongRoot[:]
	_, err = incomingRecipientsFromEvent(arkPSBT, evt)
	require.ErrorContains(t, err, "root and pkscript mismatch")

	evt.TaprootAssetRoot = assetRoot[:]
	evt.TaprootAssetAmount = 0
	_, err = incomingRecipientsFromEvent(arkPSBT, evt)
	require.ErrorContains(t, err, "ref and amount must both be provided")

	evt.TaprootAssetAmount = 21
	evt.TaprootAssetRef = string(
		make(
			[]byte, oortx.MaxTaprootAssetRefBytes+1,
		),
	)
	_, err = incomingRecipientsFromEvent(arkPSBT, evt)
	require.ErrorContains(t, err, "asset ref exceeds")
}
