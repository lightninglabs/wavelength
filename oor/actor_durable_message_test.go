package oor

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
)

// TestStartTransferPayloadTLVRoundTrip asserts start-transfer payload TLV
// encoding/decoding preserves all fields.
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

// TestSessionPayloadTLVRoundTrip asserts session payload TLV encoding/decoding
// preserves the session identifier.
func TestSessionPayloadTLVRoundTrip(t *testing.T) {
	t.Parallel()

	id := SessionID(chainhash.Hash{9, 8, 7, 6})
	raw, err := encodeSessionPayload(id)
	require.NoError(t, err)

	decoded, err := decodeSessionPayload(raw)
	require.NoError(t, err)
	require.Equal(t, id, decoded)
}

// TestDecodeLengthPrefixedBlobListRejectsTrailingBytes asserts decode rejects
// payloads with trailing bytes.
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

// TestDriveEventRequestRoundTripFailEvent asserts DriveEventRequest TLV
// Encode/Decode round-trips FailEvent correctly.
func TestDriveEventRequestRoundTripFailEvent(t *testing.T) {
	t.Parallel()

	sessionID := SessionID(chainhash.Hash{7, 7, 7})
	msg := &DriveEventRequest{
		SessionID: sessionID,
		Event: &FailEvent{
			Reason: "transport timeout",
		},
	}

	var buf bytes.Buffer
	require.NoError(t, msg.Encode(&buf))

	decoded := &DriveEventRequest{}
	require.NoError(t, decoded.Decode(&buf))

	require.Equal(t, sessionID, decoded.SessionID)

	failEvt, ok := decoded.Event.(*FailEvent)
	require.True(t, ok)
	require.Equal(t, "transport timeout", failEvt.Reason)
}

// TestDriveEventRequestRoundTripSubmitAcceptedEvent asserts DriveEventRequest
// TLV Encode/Decode round-trips SubmitAcceptedEvent correctly.
func TestDriveEventRequestRoundTripSubmitAcceptedEvent(t *testing.T) {
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

	var buf bytes.Buffer
	require.NoError(t, msg.Encode(&buf))

	decoded := &DriveEventRequest{}
	require.NoError(t, decoded.Decode(&buf))

	require.Equal(t, sessionID, decoded.SessionID)

	submitEvt, ok := decoded.Event.(*SubmitAcceptedEvent)
	require.True(t, ok)
	require.Equal(t, sessionID, submitEvt.SessionID)
	require.NotNil(t, submitEvt.ArkPSBT)
	require.Len(t, submitEvt.CoSignedCheckpointPSBTs, 1)

	decodedTxID := submitEvt.ArkPSBT.UnsignedTx.TxHash()
	require.Equal(t, chainhash.Hash(sessionID), decodedTxID)
}

// TestDriveEventRequestRoundTripRetryDueEvent asserts DriveEventRequest TLV
// Encode/Decode round-trips RetryDueEvent correctly.
func TestDriveEventRequestRoundTripRetryDueEvent(t *testing.T) {
	t.Parallel()

	sessionID := SessionID(chainhash.Hash{3, 3, 3})
	msg := &DriveEventRequest{
		SessionID: sessionID,
		Event:     &RetryDueEvent{},
	}

	var buf bytes.Buffer
	require.NoError(t, msg.Encode(&buf))

	decoded := &DriveEventRequest{}
	require.NoError(t, decoded.Decode(&buf))

	require.Equal(t, sessionID, decoded.SessionID)
	require.IsType(t, &RetryDueEvent{}, decoded.Event)
}

// TestDriveEventPayloadRequiresEvent asserts drive-event payload encoding
// rejects nil events.
func TestDriveEventPayloadRequiresEvent(t *testing.T) {
	t.Parallel()

	_, err := encodeDriveEventRequestPayload(
		SessionID(chainhash.Hash{1, 2, 3}), nil,
	)
	require.ErrorContains(t, err, "event must be provided")
}
