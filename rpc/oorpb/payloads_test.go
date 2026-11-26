package oorpb

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
	"github.com/stretchr/testify/require"
)

// TestSubmitPackageRequestRoundTrip verifies submit request conversion through
// typed proto payloads.
func TestSubmitPackageRequestRoundTrip(t *testing.T) {
	t.Parallel()

	ark := mustTestPSBT(t, 11)
	checkpoints := []*psbt.Packet{
		mustTestPSBT(t, 21),
		mustTestPSBT(t, 22),
	}

	hashHex := "0f555f77697777895555121212121212" +
		"12121212121212121212121212121212"
	descs := []SigningDescriptor{{
		Outpoint: wire.OutPoint{
			Hash:  mustHash(t, hashHex),
			Index: 7,
		},
		VTXOPolicyTemplate: []byte{
			0x11,
			0x22,
			0x33,
		},
		SpendPath: []byte{
			0x44,
			0x55,
		},
		OwnerLeafPolicy: []byte{
			0x01,
			0x02,
			0x03,
		},
	}}
	recipients := []oortx.RecipientOutput{{
		PkScript: []byte{
			0x51,
			0x20,
			0x01,
		},
		Value: 12345,
		VTXOPolicyTemplate: []byte{
			0xaa,
			0xbb,
		},
	}}

	req, err := NewSubmitPackageRequest(
		ark, checkpoints, descs, recipients,
	)
	require.NoError(t, err)

	decArk, decCheckpoints, decDescs, decRecipients, err :=
		ParseSubmitPackageRequest(req)
	require.NoError(t, err)
	require.Equal(t, 1, len(decDescs))
	require.Equal(t, descs[0].Outpoint, decDescs[0].Outpoint)
	require.Equal(
		t, descs[0].VTXOPolicyTemplate, decDescs[0].VTXOPolicyTemplate,
	)
	require.Equal(t, descs[0].SpendPath, decDescs[0].SpendPath)
	require.Equal(
		t, descs[0].OwnerLeafPolicy, decDescs[0].OwnerLeafPolicy,
	)
	require.Equal(t, recipients, decRecipients)

	require.True(
		t,
		bytes.Equal(
			mustSerializePSBT(t, ark), mustSerializePSBT(t, decArk),
		),
	)
	require.Equal(t, len(checkpoints), len(decCheckpoints))
	for i := range checkpoints {
		require.True(
			t,
			bytes.Equal(
				mustSerializePSBT(t, checkpoints[i]),
				mustSerializePSBT(t, decCheckpoints[i]),
			),
		)
	}
}

// TestSubmitPackageRequestWithAssetsRoundTrip verifies asset metadata on
// descriptors and recipients plus the sealed TaprootAssetTransfer container.
func TestSubmitPackageRequestWithAssetsRoundTrip(t *testing.T) {
	t.Parallel()

	ark := mustTestPSBT(t, 11)
	checkpoints := []*psbt.Packet{
		mustTestPSBT(t, 21),
		mustTestPSBT(t, 22),
	}

	inRoot := chainhash.Hash{0xaa, 0xbb}
	outRoot := chainhash.Hash{0xcc, 0xdd}
	descs := []SigningDescriptor{
		{
			Outpoint: wire.OutPoint{
				Hash: chainhash.Hash{
					0x01,
				},
				Index: 0,
			},
			VTXOPolicyTemplate: []byte{
				0x11,
				0x22,
			},
			SpendPath: []byte{
				0x33,
				0x44,
			},
			OwnerLeafPolicy: []byte{
				0x55,
				0x66,
			},
			TaprootAssetRoot:   &inRoot,
			TaprootAssetRef:    "group:deadbeef",
			TaprootAssetAmount: 500,
		},
		{
			// Second input models the operator-funded float input
			// carrying no asset.
			Outpoint: wire.OutPoint{
				Hash: chainhash.Hash{
					0x02,
				},
				Index: 1,
			},
			VTXOPolicyTemplate: []byte{
				0x77,
				0x88,
			},
			SpendPath: []byte{
				0x99,
			},
			OwnerLeafPolicy: []byte{
				0xaa,
			},
		},
	}
	recipients := []oortx.RecipientOutput{
		{
			PkScript: []byte{
				0x51,
				0x20,
				0x01,
			},
			Value: 1_000,
			VTXOPolicyTemplate: []byte{
				0xab,
				0xcd,
			},
			Asset: &oortx.RecipientAsset{
				Root:   outRoot,
				Ref:    "group:deadbeef",
				Amount: 500,
			},
		},
		{
			PkScript: []byte{
				0x51,
				0x20,
				0x02,
			},
			Value: 4_000,
		},
	}
	assetTransfer := &oortx.TaprootAssetTransfer{
		Version: oortx.TaprootAssetTransferVersion,
		CheckpointPackages: [][]byte{
			[]byte("sealed-checkpoint-0"),
			nil,
		},
		ArkPackage: []byte("sealed-ark"),
	}

	req, err := NewSubmitPackageRequestWithAssets(
		ark, checkpoints, descs, recipients, assetTransfer,
	)
	require.NoError(t, err)
	require.NotEmpty(t, req.TaprootAssetTransfer)

	gotArk, gotCheckpoints, gotDescs, gotRecipients, gotTransfer, err :=
		ParseSubmitPackageRequestWithAssets(req)
	require.NoError(t, err)
	require.Equal(
		t, mustSerializePSBT(t, ark), mustSerializePSBT(t, gotArk),
	)
	require.Len(t, gotCheckpoints, 2)
	require.Equal(t, descs, gotDescs)
	require.Equal(t, recipients, gotRecipients)
	require.Equal(t, assetTransfer, gotTransfer)

	// Legacy parser fails closed on asset submits so non-asset callers
	// never silently drop TaprootAssetTransfer or accept asset fields.
	_, _, _, _, err = ParseSubmitPackageRequest(req)
	require.ErrorContains(t, err, "carries taproot asset transfer")

	badReq := &SubmitPackageRequest{
		ArkPsbt:              req.ArkPsbt,
		CheckpointPsbts:      req.CheckpointPsbts,
		SigningDescriptors:   req.SigningDescriptors,
		RecipientOutputs:     req.RecipientOutputs,
		TaprootAssetTransfer: []byte("corrupt-container"),
	}
	_, _, _, _, _, err = ParseSubmitPackageRequestWithAssets(badReq)
	require.ErrorContains(t, err, "taproot asset transfer")
	_, _, _, _, err = ParseSubmitPackageRequest(badReq)
	require.ErrorContains(t, err, "taproot asset transfer")

	// Cross-check: asset descriptors or recipients without a container are
	// rejected on both encode and decode.
	_, err = NewSubmitPackageRequest(ark, checkpoints, descs, recipients)
	require.ErrorContains(t, err, "require a taproot asset transfer")

	noContainerReq := &SubmitPackageRequest{
		ArkPsbt:            req.ArkPsbt,
		CheckpointPsbts:    req.CheckpointPsbts,
		SigningDescriptors: req.SigningDescriptors,
		RecipientOutputs:   req.RecipientOutputs,
	}
	_, _, _, _, _, err = ParseSubmitPackageRequestWithAssets(noContainerReq)
	require.ErrorContains(t, err, "require a taproot asset transfer")

	// Cross-check: container with swapped checkpoint slots vs descriptors
	// is rejected.
	swappedTransfer := &oortx.TaprootAssetTransfer{
		Version: oortx.TaprootAssetTransferVersion,
		CheckpointPackages: [][]byte{
			nil,
			[]byte("sealed-checkpoint-1"),
		},
		ArkPackage: []byte("sealed-ark"),
	}
	_, err = NewSubmitPackageRequestWithAssets(
		ark, checkpoints, descs, recipients, swappedTransfer,
	)
	require.ErrorContains(
		t, err, "does not match checkpoint package slot",
	)

	// Cross-check: container with no asset-bearing recipients is rejected.
	plainRecipients := []oortx.RecipientOutput{
		{
			PkScript: []byte{
				0x51,
				0x20,
				0x02,
			},
			Value: 5_000,
		},
	}
	_, err = NewSubmitPackageRequestWithAssets(
		ark, checkpoints, descs, plainRecipients, assetTransfer,
	)
	require.ErrorContains(t, err, "at least one asset-bearing recipient")
}

// TestSubmitPackageRequestRejectsMalformedAssetFields checks all-or-none, root
// length, zero root, and container checkpoint-count mismatch rejections.
func TestSubmitPackageRequestRejectsMalformedAssetFields(t *testing.T) {
	t.Parallel()

	ark := mustTestPSBT(t, 11)
	checkpoints := []*psbt.Packet{mustTestPSBT(t, 21)}
	validRoot := chainhash.Hash{0x05}
	zeroRoot := chainhash.Hash{}

	// Encode rejects half-populated descriptor asset fields.
	_, err := NewSubmitPackageRequest(
		ark, checkpoints, []SigningDescriptor{{
			TaprootAssetRef:    "asset",
			TaprootAssetAmount: 10,
		}}, nil,
	)
	require.ErrorContains(t, err, "commitment root")

	_, err = NewSubmitPackageRequest(
		ark, checkpoints, []SigningDescriptor{{
			TaprootAssetRoot:   &zeroRoot,
			TaprootAssetRef:    "asset",
			TaprootAssetAmount: 10,
		}}, nil,
	)
	require.ErrorContains(t, err, "root must not be zero")

	// Encode rejects malformed recipient asset metadata.
	_, err = NewSubmitPackageRequest(
		ark, checkpoints, nil, []oortx.RecipientOutput{{
			Asset: &oortx.RecipientAsset{
				Root:   validRoot,
				Ref:    "asset",
				Amount: 0,
			},
		}},
	)
	require.ErrorContains(t, err, "amount must be positive")

	// Encode rejects mismatched checkpoint count in TaprootAssetTransfer.
	_, err = NewSubmitPackageRequestWithAssets(
		ark, checkpoints, nil, nil, &oortx.TaprootAssetTransfer{
			Version: oortx.TaprootAssetTransferVersion,
			CheckpointPackages: [][]byte{
				[]byte("cp-0"),
				[]byte("cp-1"),
			},
			ArkPackage: []byte("ark"),
		},
	)
	require.ErrorContains(t, err, "does not match checkpoint count")

	// Decode rejects invalid asset root length on descriptor and recipient.
	baseReq, err := NewSubmitPackageRequest(ark, checkpoints, nil, nil)
	require.NoError(t, err)

	badDescReq := &SubmitPackageRequest{
		ArkPsbt:         baseReq.ArkPsbt,
		CheckpointPsbts: baseReq.CheckpointPsbts,
		SigningDescriptors: []*OORSigningDescriptor{{
			Outpoint: &OOROutPoint{
				Txid: make([]byte, chainhash.HashSize),
			},
			TaprootAssetRoot: []byte{
				0x01,
				0x02,
			},
			TaprootAssetRef:    "asset",
			TaprootAssetAmount: 10,
		}},
	}
	_, _, _, _, err = ParseSubmitPackageRequest(badDescReq)
	require.ErrorContains(t, err, "invalid taproot asset root length")

	badRecReq := &SubmitPackageRequest{
		ArkPsbt:         baseReq.ArkPsbt,
		CheckpointPsbts: baseReq.CheckpointPsbts,
		RecipientOutputs: []*OORRecipientOutput{{
			TaprootAssetRoot:   validRoot.CloneBytes(),
			TaprootAssetRef:    "",
			TaprootAssetAmount: 10,
		}},
	}
	_, _, _, _, err = ParseSubmitPackageRequest(badRecReq)
	require.ErrorContains(t, err, "ref must be provided")
}

// TestSubmitPackageResponseRoundTrip verifies submit response conversion
// through typed proto payloads.
func TestSubmitPackageResponseRoundTrip(t *testing.T) {
	t.Parallel()

	submitSessionIDHex := "8f555f77697777895555121212121212" +
		"12121212121212121212121212121212"
	sessionID := mustHash(
		t,
		submitSessionIDHex,
	)
	checkpoints := []*psbt.Packet{
		mustTestPSBT(t, 31),
		mustTestPSBT(t, 32),
	}
	ark := mustTestPSBT(t, 30)

	resp, err := NewSubmitPackageResponse(sessionID, ark, checkpoints)
	require.NoError(t, err)

	decSessionID, decArk, decCheckpoints, err := ParseSubmitPackageResponse(
		resp,
	)
	require.NoError(t, err)
	require.Equal(t, sessionID, decSessionID)
	require.True(
		t,
		bytes.Equal(
			mustSerializePSBT(t, ark), mustSerializePSBT(t, decArk),
		),
	)
	require.Equal(t, len(checkpoints), len(decCheckpoints))

	for i := range checkpoints {
		require.True(
			t,
			bytes.Equal(
				mustSerializePSBT(t, checkpoints[i]),
				mustSerializePSBT(t, decCheckpoints[i]),
			),
		)
	}
}

// TestParseSubmitPackageResponseEmptyCoSignedArkPsbt covers wire
// backward-compat: older operators that have not been upgraded to populate
// co_signed_ark_psbt return success with an empty bytes field. The parser
// must treat that as "operator did not include the artifact" rather than
// failing every submit response in a rolling-upgrade window.
func TestParseSubmitPackageResponseEmptyCoSignedArkPsbt(t *testing.T) {
	t.Parallel()

	submitSessionIDHex := "8f555f77697777895555121212121212" +
		"12121212121212121212121212121212"
	sessionID := mustHash(t, submitSessionIDHex)

	resp := &SubmitPackageResponse{
		Result: &SubmitPackageResponse_Success{
			Success: &SubmitPackageSuccess{
				SessionId:               sessionID[:],
				CoSignedArkPsbt:         nil,
				CoSignedCheckpointPsbts: nil,
			},
		},
	}

	decSessionID, decArk, decCheckpoints, err := ParseSubmitPackageResponse(
		resp,
	)
	require.NoError(t, err)
	require.Equal(t, sessionID, decSessionID)
	require.Nil(t, decArk)
	require.Empty(t, decCheckpoints)
}

// TestFinalizePackageRoundTrip verifies finalize request/response conversion
// through typed proto payloads.
func TestFinalizePackageRoundTrip(t *testing.T) {
	t.Parallel()

	finalizeSessionIDHex := "af555f77697777895555121212121212" +
		"12121212121212121212121212121212"
	sessionID := mustHash(
		t,
		finalizeSessionIDHex,
	)
	finalCheckpoints := []*psbt.Packet{
		mustTestPSBT(t, 41),
	}

	req, err := NewFinalizePackageRequest(sessionID, finalCheckpoints)
	require.NoError(t, err)

	decSessionID, decFinalCheckpoints, err := ParseFinalizePackageRequest(
		req,
	)
	require.NoError(t, err)
	require.Equal(t, sessionID, decSessionID)
	require.Equal(t, len(finalCheckpoints), len(decFinalCheckpoints))
	require.True(
		t,
		bytes.Equal(
			mustSerializePSBT(t, finalCheckpoints[0]),
			mustSerializePSBT(t, decFinalCheckpoints[0]),
		),
	)

	resp := NewFinalizePackageResponse(sessionID)
	respSessionID, err := ParseFinalizePackageResponse(resp)
	require.NoError(t, err)
	require.Equal(t, sessionID, respSessionID)
}

// TestParseFinalizePackageResponseRejectsInvalidSessionLength verifies session
// id validation on typed finalize responses.
func TestParseFinalizePackageResponseRejectsInvalidSessionLength(t *testing.T) {
	t.Parallel()

	_, err := ParseFinalizePackageResponse(&FinalizePackageResponse{
		SessionId: []byte{1, 2, 3},
	})
	require.ErrorContains(t, err, "invalid session id length")
}

// mustHash parses a chain hash string for tests.
func mustHash(t *testing.T, hash string) chainhash.Hash {
	t.Helper()

	parsed, err := chainhash.NewHashFromStr(hash)
	require.NoError(t, err)

	return *parsed
}

// mustTestPSBT builds a minimal serializable PSBT packet for tests.
func mustTestPSBT(t *testing.T, marker byte) *psbt.Packet {
	t.Helper()

	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash: chainhash.Hash{marker},
		},
	})
	tx.AddTxOut(&wire.TxOut{
		Value:    1000,
		PkScript: []byte{txscript.OP_TRUE},
	})

	packet, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(t, err)

	return packet
}

// mustSerializePSBT serializes a PSBT packet in tests.
func mustSerializePSBT(t *testing.T, packet *psbt.Packet) []byte {
	t.Helper()

	raw, err := psbtutil.Serialize(packet)
	require.NoError(t, err)

	return raw
}
