package oor

import (
	"bytes"
	"math"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

func TestStartTransferPayloadTLVRoundTrip(t *testing.T) {
	t.Parallel()

	payload := startTransferPayload{
		OperatorPubKey: []byte{2, 1, 2, 3},
		CSVDelay:       144,
		Inputs: []*TransferInputSnapshot{
			{
				Outpoint: wire.OutPoint{
					Hash:  chainhash.Hash{1, 2, 3},
					Index: 7,
				},
				AmountSat:       1000,
				ClientKeyFamily: 1,
				ClientKeyIndex:  9,
				ClientPubKey:    []byte{2, 4, 6},
				OperatorPubKey:  []byte{3, 5, 7},
				ExitDelay:       72,
				OwnerLeafScript: []byte{0x51},
			},
		},
		Recipients: []recipientPayload{
			{
				PkScript: []byte{0x51, 0x20},
				ValueSat: 321,
			},
		},
	}

	raw, err := encodeStartTransferPayload(payload)
	require.NoError(t, err)

	decoded, err := decodeStartTransferPayload(raw)
	require.NoError(t, err)

	require.Equal(t, payload.OperatorPubKey, decoded.OperatorPubKey)
	require.Equal(t, payload.CSVDelay, decoded.CSVDelay)
	require.Equal(t, payload.Recipients, decoded.Recipients)
	require.Len(t, decoded.Inputs, 1)
	require.Equal(t, payload.Inputs[0], decoded.Inputs[0])
}

func TestSessionPayloadTLVRoundTrip(t *testing.T) {
	t.Parallel()

	id := SessionID(chainhash.Hash{9, 8, 7, 6})
	raw, err := encodeSessionPayload(id)
	require.NoError(t, err)

	decoded, err := decodeSessionPayload(raw)
	require.NoError(t, err)
	require.Equal(t, id, decoded)
}

func TestDecodeLengthPrefixedBlobListRejectsTrailingBytes(t *testing.T) {
	t.Parallel()

	raw, err := encodeLengthPrefixedBlobList(
		[][]byte{{1, 2, 3}},
	)
	require.NoError(t, err)

	raw = append(raw, 0xff)
	_, err = decodeLengthPrefixedBlobList(raw)
	require.ErrorContains(t, err, "trailing payload bytes")
}

func TestDriveEventCommandRoundTripFailEvent(t *testing.T) {
	t.Parallel()

	sessionID := SessionID(chainhash.Hash{7, 7, 7})
	msg := &DriveEventRequest{
		SessionID: sessionID,
		Event: &FailEvent{
			Reason: "transport timeout",
		},
	}

	cmd, err := durableCommandFromActorMsg(msg)
	require.NoError(t, err)
	require.Equal(t, oorCommandDriveEvent, cmd.Command)

	decoded, err := actorMsgFromDurableCommand(cmd)
	require.NoError(t, err)

	decodedReq, ok := decoded.(*DriveEventRequest)
	require.True(t, ok)
	require.Equal(t, sessionID, decodedReq.SessionID)

	failEvt, ok := decodedReq.Event.(*FailEvent)
	require.True(t, ok)
	require.Equal(t, "transport timeout", failEvt.Reason)
}

func TestDriveEventCommandRoundTripSubmitAcceptedEvent(t *testing.T) {
	t.Parallel()

	ark, checkpoints := testOutboxPSBTPair(t)
	sessionID, err := sessionIDFromArk(ark)
	require.NoError(t, err)

	msg := &DriveEventRequest{
		SessionID: sessionID,
		Event: &SubmitAcceptedEvent{
			SessionID:               sessionID,
			ArkPSBT:                 ark,
			CoSignedCheckpointPSBTs: checkpoints,
		},
	}

	cmd, err := durableCommandFromActorMsg(msg)
	require.NoError(t, err)
	require.Equal(t, oorCommandDriveEvent, cmd.Command)

	decoded, err := actorMsgFromDurableCommand(cmd)
	require.NoError(t, err)

	decodedReq, ok := decoded.(*DriveEventRequest)
	require.True(t, ok)
	require.Equal(t, sessionID, decodedReq.SessionID)

	submitEvt, ok := decodedReq.Event.(*SubmitAcceptedEvent)
	require.True(t, ok)
	require.Equal(t, sessionID, submitEvt.SessionID)
	require.NotNil(t, submitEvt.ArkPSBT)
	require.Len(t, submitEvt.CoSignedCheckpointPSBTs, 1)

	decodedTxID := submitEvt.ArkPSBT.UnsignedTx.TxHash()
	require.Equal(t, chainhash.Hash(sessionID), decodedTxID)
}

func TestDecodeRecipientPayloadRejectsInt64Overflow(t *testing.T) {
	t.Parallel()

	valueSat := uint64(math.MaxInt64) + 1
	pkScript := []byte{0x51}

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(recipientPkScriptRecordType, &pkScript),
		tlv.MakePrimitiveRecord(recipientValueSatRecordType, &valueSat),
	}

	stream, err := tlv.NewStream(records...)
	require.NoError(t, err)

	var raw bytes.Buffer
	require.NoError(t, stream.Encode(&raw))

	_, err = decodeRecipientPayload(raw.Bytes())
	require.ErrorContains(t, err, "recipient value sat overflows int64")
}

func TestDecodeTransferInputSnapshotRejectsAmountOverflow(t *testing.T) {
	t.Parallel()

	outpointRaw := outPointBytes(wire.OutPoint{
		Hash:  chainhash.Hash{1, 2, 3},
		Index: 4,
	})
	amountSat := uint64(math.MaxInt64) + 1
	clientFamily := uint32(1)
	clientIndex := uint32(7)
	clientPubKey := []byte{2, 3, 4}
	operatorPubKey := []byte{2, 5, 6}
	exitDelay := uint32(144)
	ownerLeafScript := []byte{0x51}

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			transferInputOutpointRecordType, &outpointRaw,
		),
		tlv.MakePrimitiveRecord(
			transferInputAmountSatRecordType, &amountSat,
		),
		tlv.MakePrimitiveRecord(
			transferInputClientFamilyRecordType, &clientFamily,
		),
		tlv.MakePrimitiveRecord(
			transferInputClientIndexRecordType, &clientIndex,
		),
		tlv.MakePrimitiveRecord(
			transferInputClientPubKeyRecordType, &clientPubKey,
		),
		tlv.MakePrimitiveRecord(
			transferInputOperatorPubKeyRecordType, &operatorPubKey,
		),
		tlv.MakePrimitiveRecord(
			transferInputExitDelayRecordType, &exitDelay,
		),
		tlv.MakePrimitiveRecord(
			transferInputOwnerLeafScriptRecordType,
			&ownerLeafScript,
		),
	}

	stream, err := tlv.NewStream(records...)
	require.NoError(t, err)

	var raw bytes.Buffer
	require.NoError(t, stream.Encode(&raw))

	_, err = decodeTransferInputSnapshot(raw.Bytes())
	require.ErrorContains(t, err, "amount sat overflows int64")
}

func TestDecodeTransferInputSnapshotRejectsFamilyOverflow(t *testing.T) {
	t.Parallel()

	outpointRaw := outPointBytes(wire.OutPoint{
		Hash:  chainhash.Hash{9, 8, 7},
		Index: 6,
	})
	amountSat := uint64(100)
	clientFamily := uint32(math.MaxInt32) + 1
	clientIndex := uint32(7)
	clientPubKey := []byte{2, 3, 4}
	operatorPubKey := []byte{2, 5, 6}
	exitDelay := uint32(144)
	ownerLeafScript := []byte{0x51}

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			transferInputOutpointRecordType, &outpointRaw,
		),
		tlv.MakePrimitiveRecord(
			transferInputAmountSatRecordType, &amountSat,
		),
		tlv.MakePrimitiveRecord(
			transferInputClientFamilyRecordType, &clientFamily,
		),
		tlv.MakePrimitiveRecord(
			transferInputClientIndexRecordType, &clientIndex,
		),
		tlv.MakePrimitiveRecord(
			transferInputClientPubKeyRecordType, &clientPubKey,
		),
		tlv.MakePrimitiveRecord(
			transferInputOperatorPubKeyRecordType, &operatorPubKey,
		),
		tlv.MakePrimitiveRecord(
			transferInputExitDelayRecordType, &exitDelay,
		),
		tlv.MakePrimitiveRecord(
			transferInputOwnerLeafScriptRecordType,
			&ownerLeafScript,
		),
	}

	stream, err := tlv.NewStream(records...)
	require.NoError(t, err)

	var raw bytes.Buffer
	require.NoError(t, stream.Encode(&raw))

	_, err = decodeTransferInputSnapshot(raw.Bytes())
	require.ErrorContains(t, err, "client key family overflows int32")
}
