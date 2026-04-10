package oor

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// serializePSBTForAssert serializes a PSBT so comparisons are deterministic.
func serializePSBTForAssert(t *testing.T, pkt *psbt.Packet) []byte {
	t.Helper()

	var buf bytes.Buffer
	err := pkt.Serialize(&buf)
	require.NoError(t, err)

	return buf.Bytes()
}

// TestSubmitOORRequestRoundTrip verifies submit request TLV encode/decode
// round-trip behavior.
func TestSubmitOORRequestRoundTrip(t *testing.T) {
	t.Parallel()

	var outHash chainhash.Hash
	outHash[0] = 0xAA

	ownerPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	operatorPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	vtxoPolicyTemplate, err := arkscript.EncodeStandardVTXOTemplate(
		ownerPriv.PubKey(), operatorPriv.PubKey(), 144,
	)
	require.NoError(t, err)

	vtxoPolicy, err := arkscript.NewVTXOPolicy(
		ownerPriv.PubKey(), operatorPriv.PubKey(), 144,
	)
	require.NoError(t, err)

	spendInfo, err := vtxoPolicy.CollabSpendInfo()
	require.NoError(t, err)

	spendPath, err := (&arkscript.SpendPath{
		SpendInfo: spendInfo,
	}).Encode()
	require.NoError(t, err)

	original := &SubmitOORRequest{
		ClientID: clientconn.ClientID("test-client-submit"),
		ArkPSBT:  makeTestPSBT(t, 1),
		CheckpointPSBTs: []*psbt.Packet{
			makeTestPSBT(t, 2),
			makeTestPSBT(t, 3),
		},
		VTXOSigningDescriptors: []VTXOSigningDescriptor{
			{
				Outpoint: wire.OutPoint{
					Hash:  outHash,
					Index: 7,
				},
				VTXOPolicyTemplate: vtxoPolicyTemplate,
				SpendPath:          spendPath,
				OwnerLeafPolicy:    []byte{0x01, 0x02, 0x03},
			},
		},
	}

	var encoded bytes.Buffer
	err = original.Encode(&encoded)
	require.NoError(t, err)

	var decoded SubmitOORRequest
	err = decoded.Decode(bytes.NewReader(encoded.Bytes()))
	require.NoError(t, err)

	require.Equal(t, original.ClientID, decoded.ClientID)
	require.Equal(
		t, serializePSBTForAssert(t, original.ArkPSBT),
		serializePSBTForAssert(t, decoded.ArkPSBT),
	)
	require.Len(t, decoded.CheckpointPSBTs, 2)
	require.Equal(
		t,
		serializePSBTForAssert(t, original.CheckpointPSBTs[0]),
		serializePSBTForAssert(t, decoded.CheckpointPSBTs[0]),
	)
	require.Equal(
		t,
		serializePSBTForAssert(t, original.CheckpointPSBTs[1]),
		serializePSBTForAssert(t, decoded.CheckpointPSBTs[1]),
	)
	require.Len(t, decoded.VTXOSigningDescriptors, 1)
	require.Equal(
		t, original.VTXOSigningDescriptors[0].Outpoint,
		decoded.VTXOSigningDescriptors[0].Outpoint,
	)
	require.Equal(
		t, original.VTXOSigningDescriptors[0].VTXOPolicyTemplate,
		decoded.VTXOSigningDescriptors[0].VTXOPolicyTemplate,
	)
	require.Equal(
		t, original.VTXOSigningDescriptors[0].SpendPath,
		decoded.VTXOSigningDescriptors[0].SpendPath,
	)
	require.Equal(
		t, original.VTXOSigningDescriptors[0].OwnerLeafPolicy,
		decoded.VTXOSigningDescriptors[0].OwnerLeafPolicy,
	)
}

// TestFinalizeOORRequestRoundTrip verifies finalize request TLV encode/decode
// round-trip behavior.
func TestFinalizeOORRequestRoundTrip(t *testing.T) {
	t.Parallel()

	var sessionHash chainhash.Hash
	sessionHash[0] = 0xFE

	original := &FinalizeOORRequest{
		ClientID: clientconn.ClientID(
			"test-client-finalize",
		),
		SessionID:            SessionID(sessionHash),
		FinalCheckpointPSBTs: []*psbt.Packet{makeTestPSBT(t, 9)},
	}

	var encoded bytes.Buffer
	err := original.Encode(&encoded)
	require.NoError(t, err)

	var decoded FinalizeOORRequest
	err = decoded.Decode(bytes.NewReader(encoded.Bytes()))
	require.NoError(t, err)

	require.Equal(t, original.ClientID, decoded.ClientID)
	require.Equal(t, original.SessionID, decoded.SessionID)
	require.Len(t, decoded.FinalCheckpointPSBTs, 1)
	require.Equal(
		t,
		serializePSBTForAssert(t, original.FinalCheckpointPSBTs[0]),
		serializePSBTForAssert(t, decoded.FinalCheckpointPSBTs[0]),
	)
}

// TestSubmitOORRequestDecodeRequiresArkPSBT verifies decode rejection when
// required submit records are missing.
func TestSubmitOORRequestDecodeRequiresArkPSBT(t *testing.T) {
	t.Parallel()

	checkpointBlob, err := encodeTLVByteList(nil)
	require.NoError(t, err)

	signingBlob, err := encodeTLVByteList(nil)
	require.NoError(t, err)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			submitCheckpointPSBTsRecordType, &checkpointBlob,
		),
		tlv.MakePrimitiveRecord(
			submitSigningDescriptorsRecordType, &signingBlob,
		),
	)
	require.NoError(t, err)

	var encoded bytes.Buffer
	err = stream.Encode(&encoded)
	require.NoError(t, err)

	var decoded SubmitOORRequest
	err = decoded.Decode(bytes.NewReader(encoded.Bytes()))
	require.ErrorContains(t, err, "ark psbt must be provided")
}

// TestDecodeTLVByteListRejectsNonCanonicalTypes verifies list decoding rejects
// non-contiguous TLV record type sequences.
func TestDecodeTLVByteListRejectsNonCanonicalTypes(t *testing.T) {
	t.Parallel()

	first := []byte{0x01}
	third := []byte{0x03}
	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(0, &first),
		tlv.MakePrimitiveRecord(2, &third),
	)
	require.NoError(t, err)

	var encoded bytes.Buffer
	err = stream.Encode(&encoded)
	require.NoError(t, err)

	_, err = decodeTLVByteList(encoded.Bytes())
	require.ErrorContains(t, err, "non-canonical list record type")
}
