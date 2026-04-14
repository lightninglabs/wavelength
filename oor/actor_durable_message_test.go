package oor

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/vtxo"
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

	payload.Recipients[0].VTXOPolicyTemplate = []byte{}

	require.Equal(t, payload.OperatorPubKey, decoded.OperatorPubKey)
	require.Equal(t, payload.CSVDelay, decoded.CSVDelay)
	require.Equal(t, payload.Recipients, decoded.Recipients)
	require.Len(t, decoded.Inputs, 1)
	require.Equal(t, payload.Inputs[0], decoded.Inputs[0])
}

// TestStartTransferPayloadTLVRoundTripCustomInput asserts custom spend-path
// transfer inputs encode records canonically when owner-leaf policy metadata
// is present alongside lower-numbered optional fields.
func TestStartTransferPayloadTLVRoundTripCustomInput(t *testing.T) {
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
				AmountSat:          1000,
				ClientKeyFamily:    1,
				ClientKeyIndex:     9,
				ClientPubKey:       []byte{2, 4, 6},
				OperatorPubKey:     []byte{3, 5, 7},
				ExitDelay:          72,
				OwnerLeafScript:    []byte{0x51},
				OwnerLeafPolicy:    []byte{0x01, 0x02},
				VTXOPolicyTemplate: []byte{0x03, 0x04},
				PkScript:           []byte{0x51, 0x20, 0x01},
				SpendWitnessScript: []byte{0x20, 0x01, 0x87},
				SpendControlBlock:  []byte{0xc0, 0x01, 0x02},
				ConditionWitness: [][]byte{
					{0xaa, 0xbb},
				},
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

// TestDriveEventRequestRoundTripIncomingTransferEvent asserts
// DriveEventRequest TLV Encode/Decode round-trips IncomingTransferEvent
// correctly.
func TestDriveEventRequestRoundTripIncomingTransferEvent(t *testing.T) {
	t.Parallel()

	arkPSBT, checkpoints, _, _, _, _ :=
		buildTestIncomingMaterialization(t)

	sessionID := SessionID(arkPSBT.UnsignedTx.TxHash())
	msg := &DriveEventRequest{
		SessionID: sessionID,
		Event: &IncomingTransferEvent{
			SessionID:            sessionID,
			ArkPSBT:              arkPSBT,
			FinalCheckpointPSBTs: checkpoints,
		},
	}

	var buf bytes.Buffer
	require.NoError(t, msg.Encode(&buf))

	decoded := &DriveEventRequest{}
	require.NoError(t, decoded.Decode(&buf))

	require.Equal(t, sessionID, decoded.SessionID)

	incomingEvt, ok := decoded.Event.(*IncomingTransferEvent)
	require.True(t, ok)
	require.Equal(t, sessionID, incomingEvt.SessionID)
	require.NotNil(t, incomingEvt.ArkPSBT)
	require.Len(t, incomingEvt.FinalCheckpointPSBTs, len(checkpoints))
}

// TestDriveEventRequestRoundTripIncomingHandledEvent asserts
// DriveEventRequest TLV Encode/Decode round-trips IncomingHandledEvent
// correctly using durable outpoint identifiers.
func TestDriveEventRequestRoundTripIncomingHandledEvent(t *testing.T) {
	t.Parallel()

	sessionID := SessionID(chainhash.Hash{8, 8, 8})
	outpoint := wire.OutPoint{
		Hash:  chainhash.Hash{9, 9, 9},
		Index: 2,
	}
	msg := &DriveEventRequest{
		SessionID: sessionID,
		Event: &IncomingHandledEvent{
			MaterializedVTXOs: []*vtxo.Descriptor{{
				Outpoint: outpoint,
			}},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, msg.Encode(&buf))

	decoded := &DriveEventRequest{}
	require.NoError(t, decoded.Decode(&buf))

	require.Equal(t, sessionID, decoded.SessionID)

	handledEvt, ok := decoded.Event.(*IncomingHandledEvent)
	require.True(t, ok)
	require.Len(t, handledEvt.MaterializedOutpoints, 1)
	require.Equal(t, outpoint, handledEvt.MaterializedOutpoints[0])
	require.Empty(t, handledEvt.MaterializedVTXOs)
}

// TestDriveEventRequestRoundTripIncomingAckSentEvent asserts
// DriveEventRequest TLV Encode/Decode round-trips IncomingAckSentEvent
// correctly.
func TestDriveEventRequestRoundTripIncomingAckSentEvent(t *testing.T) {
	t.Parallel()

	sessionID := SessionID(chainhash.Hash{6, 6, 6})
	msg := &DriveEventRequest{
		SessionID: sessionID,
		Event:     &IncomingAckSentEvent{},
	}

	var buf bytes.Buffer
	require.NoError(t, msg.Encode(&buf))

	decoded := &DriveEventRequest{}
	require.NoError(t, decoded.Decode(&buf))

	require.Equal(t, sessionID, decoded.SessionID)
	require.IsType(t, &IncomingAckSentEvent{}, decoded.Event)
}

// TestResolveIncomingTransferRequestRoundTrip asserts
// ResolveIncomingTransferRequest TLV Encode/Decode round-trips the incoming
// notification hint fields correctly.
func TestResolveIncomingTransferRequestRoundTrip(t *testing.T) {
	t.Parallel()

	sessionID := SessionID(chainhash.Hash{4, 5, 6})
	msg := &ResolveIncomingTransferRequest{
		SessionID:         sessionID,
		RecipientPkScript: []byte{0x51, 0x20, 0x01, 0x02},
		RecipientEventID:  7,
	}

	var buf bytes.Buffer
	require.NoError(t, msg.Encode(&buf))

	decoded := &ResolveIncomingTransferRequest{}
	require.NoError(t, decoded.Decode(&buf))

	require.Equal(t, sessionID, decoded.SessionID)
	require.Equal(t, msg.RecipientPkScript, decoded.RecipientPkScript)
	require.Equal(t, msg.RecipientEventID, decoded.RecipientEventID)
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
