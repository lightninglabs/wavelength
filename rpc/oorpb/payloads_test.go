package oorpb

import (
	"bytes"
	"strings"
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
	inputAssetRoot := chainhash.Hash{1, 2, 3}
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
		TaprootAssetRoot:   &inputAssetRoot,
		TaprootAssetRef:    "asset:input",
		TaprootAssetAmount: ^uint64(0),
	}}
	recipientAssetRoot := chainhash.Hash{4, 5, 6}
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
		TaprootAssetRoot:   &recipientAssetRoot,
		TaprootAssetRef:    "asset:recipient",
		TaprootAssetAmount: 800,
	}}
	assetTransfer := &oortx.TaprootAssetTransfer{
		Version: oortx.TaprootAssetTransferVersion,
		CheckpointPackages: [][]byte{
			[]byte("checkpoint-0"),
			[]byte("checkpoint-1"),
		},
		ArkPackage: []byte("ark"),
	}

	req, err := NewSubmitPackageRequestWithAssets(
		ark, checkpoints, descs, recipients, assetTransfer,
	)
	require.NoError(t, err)

	decArk, decCheckpoints, decDescs, decRecipients, decAssets, err :=
		ParseSubmitPackageRequestWithAssets(req)
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
	require.Equal(
		t, descs[0].TaprootAssetRoot, decDescs[0].TaprootAssetRoot,
	)
	require.Equal(
		t, descs[0].TaprootAssetRef, decDescs[0].TaprootAssetRef,
	)
	require.Equal(
		t, descs[0].TaprootAssetAmount, decDescs[0].TaprootAssetAmount,
	)
	require.Equal(t, recipients, decRecipients)
	require.Equal(t, assetTransfer, decAssets)

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

// TestSubmitPackageRequestRejectsAssetMetadataShapes verifies both encoding
// and parsing fail closed when signing-descriptor or recipient metadata is
// partial or exceeds the bounded opaque asset-reference size.
func TestSubmitPackageRequestRejectsAssetMetadataShapes(t *testing.T) {
	t.Parallel()

	root := chainhash.HashH([]byte("wire-asset-root"))
	oversizedRef := strings.Repeat(
		"a", oortx.MaxTaprootAssetRefBytes+1,
	)
	encodeTests := []struct {
		name       string
		descs      []SigningDescriptor
		recipients []oortx.RecipientOutput
		want       string
	}{
		{
			name: "signing descriptor partial",
			descs: []SigningDescriptor{{
				TaprootAssetRoot: &root,
				TaprootAssetRef:  "asset:partial",
			}},
			want: "asset ref and amount must both be provided",
		},
		{
			name: "signing descriptor oversized reference",
			descs: []SigningDescriptor{{
				TaprootAssetRoot:   &root,
				TaprootAssetRef:    oversizedRef,
				TaprootAssetAmount: 1,
			}},
			want: "asset ref exceeds",
		},
		{
			name: "recipient partial",
			recipients: []oortx.RecipientOutput{{
				TaprootAssetRoot:   &root,
				TaprootAssetAmount: 1,
			}},
			want: "asset ref and amount must both be provided",
		},
		{
			name: "recipient oversized reference",
			recipients: []oortx.RecipientOutput{{
				TaprootAssetRoot:   &root,
				TaprootAssetRef:    oversizedRef,
				TaprootAssetAmount: 1,
			}},
			want: "asset ref exceeds",
		},
	}
	for _, test := range encodeTests {
		test := test
		t.Run("encode "+test.name, func(t *testing.T) {
			t.Parallel()

			_, err := NewSubmitPackageRequest(
				mustTestPSBT(t, 0x71), nil, test.descs,
				test.recipients,
			)
			require.ErrorContains(t, err, test.want)
		})
	}

	decodeTests := []struct {
		name   string
		mutate func(*SubmitPackageRequest)
		want   string
	}{
		{
			name: "signing descriptor partial",
			mutate: func(req *SubmitPackageRequest) {
				assetRoot := root.CloneBytes()
				desc := &OORSigningDescriptor{
					Outpoint: encodeOutPoint(
						wire.OutPoint{},
					),
					TaprootAssetRoot: assetRoot,
					TaprootAssetRef:  "a",
				}
				descs := []*OORSigningDescriptor{
					desc,
				}
				req.SigningDescriptors = descs
			},
			want: "asset ref and amount must both be provided",
		},
		{
			name: "signing descriptor oversized reference",
			mutate: func(req *SubmitPackageRequest) {
				assetRoot := root.CloneBytes()
				assetRef := oversizedRef
				desc := &OORSigningDescriptor{
					Outpoint: encodeOutPoint(
						wire.OutPoint{},
					),
					TaprootAssetRoot:   assetRoot,
					TaprootAssetRef:    assetRef,
					TaprootAssetAmount: 1,
				}
				descs := []*OORSigningDescriptor{
					desc,
				}
				req.SigningDescriptors = descs
			},
			want: "asset ref exceeds",
		},
		{
			name: "recipient partial",
			mutate: func(req *SubmitPackageRequest) {
				req.RecipientOutputs = []*OORRecipientOutput{{
					TaprootAssetRoot:   root.CloneBytes(),
					TaprootAssetAmount: 1,
				}}
			},
			want: "asset ref and amount must both be provided",
		},
		{
			name: "recipient oversized reference",
			mutate: func(req *SubmitPackageRequest) {
				req.RecipientOutputs = []*OORRecipientOutput{{
					TaprootAssetRoot:   root.CloneBytes(),
					TaprootAssetRef:    oversizedRef,
					TaprootAssetAmount: 1,
				}}
			},
			want: "asset ref exceeds",
		},
	}
	for _, test := range decodeTests {
		test := test
		t.Run("decode "+test.name, func(t *testing.T) {
			t.Parallel()

			req, err := NewSubmitPackageRequest(
				mustTestPSBT(t, 0x72), nil, nil, nil,
			)
			require.NoError(t, err)
			test.mutate(req)

			_, _, _, _, err = ParseSubmitPackageRequest(req)
			require.ErrorContains(t, err, test.want)
		})
	}
}

func TestParseSubmitPackageRequestRejectsInvalidAssetMetadata(t *testing.T) {
	t.Parallel()

	ark := mustTestPSBT(t, 11)
	checkpoints := []*psbt.Packet{mustTestPSBT(t, 21)}
	assetTransfer := &oortx.TaprootAssetTransfer{
		Version: oortx.TaprootAssetTransferVersion,
		CheckpointPackages: [][]byte{
			[]byte("checkpoint"),
		},
		ArkPackage: []byte("ark"),
	}
	req, err := NewSubmitPackageRequestWithAssets(
		ark, checkpoints, nil, nil, assetTransfer,
	)
	require.NoError(t, err)

	req.SigningDescriptors = []*OORSigningDescriptor{{
		Outpoint: &OOROutPoint{
			Txid: make([]byte, chainhash.HashSize),
		},
		TaprootAssetRoot: []byte{
			1,
		},
	}}
	_, _, _, _, _, err = ParseSubmitPackageRequestWithAssets(req)
	require.ErrorContains(t, err, "taproot asset root length")

	req.SigningDescriptors = nil
	req.TaprootAssetTransfer[len(req.TaprootAssetTransfer)-1] ^= 1
	_, _, _, _, _, err = ParseSubmitPackageRequestWithAssets(req)
	require.ErrorContains(t, err, "checksum mismatch")
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
