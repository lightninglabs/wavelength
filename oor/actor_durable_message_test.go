package oor

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/stretchr/testify/require"
)

// TestStartTransferPayloadTLVRoundTrip asserts start-transfer payload TLV
// encoding/decoding preserves all fields.
func TestStartTransferPayloadTLVRoundTrip(t *testing.T) {
	t.Parallel()

	payload := startTransferPayload{
		OperatorPubKey: []byte{
			2,
			1,
			2,
			3,
		},
		CSVDelay: 144,
		Inputs: []*TransferInputSnapshot{
			{
				Outpoint: wire.OutPoint{
					Hash: chainhash.Hash{
						1,
						2,
						3,
					},
					Index: 7,
				},
				AmountSat:       1000,
				ClientKeyFamily: 1,
				ClientKeyIndex:  9,
				ClientPubKey: []byte{
					2,
					4,
					6,
				},
				OperatorPubKey: []byte{
					3,
					5,
					7,
				},
				ExitDelay: 72,
				OwnerLeafScript: []byte{
					0x51,
				},
			},
		},
		Recipients: []recipientPayload{
			{
				PkScript: []byte{
					0x51,
					0x20,
				},
				ValueSat: 321,
			},
		},
		IdempotencyKey: "funding-key-1",
	}

	raw, err := encodeStartTransferPayload(payload)
	require.NoError(t, err)

	decoded, err := decodeStartTransferPayload(raw)
	require.NoError(t, err)

	payload.Recipients[0].VTXOPolicyTemplate = []byte{}

	require.Equal(t, payload.OperatorPubKey, decoded.OperatorPubKey)
	require.Equal(t, payload.CSVDelay, decoded.CSVDelay)
	require.Equal(t, payload.Recipients, decoded.Recipients)
	require.Equal(t, payload.IdempotencyKey, decoded.IdempotencyKey)
	require.Len(t, decoded.Inputs, 1)
	require.Equal(t, payload.Inputs[0], decoded.Inputs[0])
}

// TestListSessionsRequestRoundTrip verifies the durable status-query message
// preserves its filters through TLV encoding.
func TestListSessionsRequestRoundTrip(t *testing.T) {
	t.Parallel()

	msg := &ListSessionsRequest{
		Direction:   SessionDirectionIncoming,
		PendingOnly: true,
	}

	var buf bytes.Buffer
	require.NoError(t, msg.Encode(&buf))

	var decoded ListSessionsRequest
	require.NoError(t, decoded.Decode(&buf))
	require.Equal(t, msg.Direction, decoded.Direction)
	require.Equal(t, msg.PendingOnly, decoded.PendingOnly)
}

// TestListSessionsRequestRejectsUnknownDirection asserts corrupt direction
// filters fail during decode instead of silently returning misleading output.
func TestListSessionsRequestRejectsUnknownDirection(t *testing.T) {
	t.Parallel()

	raw, err := encodeListSessionsPayload(SessionDirection(99), false)
	require.NoError(t, err)

	var decoded ListSessionsRequest
	err = decoded.Decode(bytes.NewReader(raw))
	require.ErrorContains(t, err, "unknown session direction")
}

// TestStartTransferPayloadTLVRoundTripCustomInput asserts custom spend-path
// transfer inputs encode records canonically when owner-leaf policy metadata
// is present alongside lower-numbered optional fields.
func TestStartTransferPayloadTLVRoundTripCustomInput(t *testing.T) {
	t.Parallel()

	payload := startTransferPayload{
		OperatorPubKey: []byte{
			2,
			1,
			2,
			3,
		},
		CSVDelay: 144,
		Inputs: []*TransferInputSnapshot{
			{
				Outpoint: wire.OutPoint{
					Hash: chainhash.Hash{
						1,
						2,
						3,
					},
					Index: 7,
				},
				AmountSat:       1000,
				ClientKeyFamily: 1,
				ClientKeyIndex:  9,
				ClientPubKey: []byte{
					2,
					4,
					6,
				},
				OperatorPubKey: []byte{
					3,
					5,
					7,
				},
				ExitDelay: 72,
				OwnerLeafScript: []byte{
					0x51,
				},
				OwnerLeafPolicy: []byte{
					0x01,
					0x02,
				},
				VTXOPolicyTemplate: []byte{
					0x03,
					0x04,
				},
				PkScript: []byte{
					0x51,
					0x20,
					0x01,
				},
				SpendWitnessScript: []byte{
					0x20,
					0x01,
					0x87,
				},
				SpendControlBlock: []byte{
					0xc0,
					0x01,
					0x02,
				},
				RequiredSequence: wire.MaxTxInSequenceNum - 1,
				RequiredLockTime: 113,
				ConditionWitness: [][]byte{
					{
						0xaa,
						0xbb,
					},
				},
			},
		},
		Recipients: []recipientPayload{
			{
				PkScript: []byte{
					0x51,
					0x20,
				},
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

// TestExternalSignaturesTLVRoundTrip verifies the durable encoding used for
// custom-input signatures preserves each signature's signer key, witness
// script, raw Schnorr signature bytes, and sighash flag. Cooperative refunds
// rely on these records when a client restarts after obtaining a server
// signature but before finalizing the custom OOR spend; a field swap or missing
// sighash would only surface later as an invalid checkpoint witness.
func TestExternalSignaturesTLVRoundTrip(t *testing.T) {
	t.Parallel()

	_, key1 := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x01}, 32))
	_, key2 := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x02}, 32))
	sigs := []ExternalTaprootScriptSignature{
		{
			PubKey: key1,
			WitnessScript: []byte{
				0x51,
				0x20,
				0x01,
			},
			Signature: []byte{
				0x11,
				0x12,
			},
			SigHash: txscript.SigHashDefault,
		},
		{
			PubKey: key2,
			WitnessScript: []byte{
				0x20,
				0x02,
				0xac,
			},
			Signature: []byte{
				0x21,
				0x22,
				0x23,
			},
			SigHash: txscript.SigHashAll,
		},
	}

	raw, err := encodeExternalSignatures(sigs)
	require.NoError(t, err)

	decoded, err := decodeExternalSignatures(raw)
	require.NoError(t, err)
	require.Len(t, decoded, len(sigs))
	for i := range sigs {
		require.Equal(
			t, sigs[i].PubKey.SerializeCompressed(),
			decoded[i].PubKey.SerializeCompressed(),
		)
		require.Equal(
			t, sigs[i].WitnessScript, decoded[i].WitnessScript,
		)
		require.Equal(t, sigs[i].Signature, decoded[i].Signature)
		require.Equal(t, sigs[i].SigHash, decoded[i].SigHash)
	}
}

// TestExternalSignaturesTLVRejectsMalformedPayloads pins the defensive decode
// rules around the external-signature durable blob. The decoder must reject
// oversize signature lists before allocation, corrupted public keys before they
// enter signing state, and trailing bytes that could otherwise mask partially
// parsed data from a future or corrupt format.
func TestExternalSignaturesTLVRejectsMalformedPayloads(t *testing.T) {
	t.Parallel()

	tooMany := make(
		[]ExternalTaprootScriptSignature, maxExternalSignatures+1,
	)
	_, err := encodeExternalSignatures(tooMany)
	require.ErrorContains(t, err, "external signature count")

	var corruptKey bytes.Buffer
	require.NoError(t, wire.WriteVarInt(&corruptKey, 0, 1))
	require.NoError(t, wire.WriteVarBytes(&corruptKey, 0, []byte{0x02}))
	_, err = decodeExternalSignatures(corruptKey.Bytes())
	require.ErrorContains(t, err, "parse external signature pubkey")

	_, key := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x03}, 32))
	raw, err := encodeExternalSignatures([]ExternalTaprootScriptSignature{
		{
			PubKey:        key,
			WitnessScript: []byte{0x51},
			Signature:     []byte{0x01},
			SigHash:       txscript.SigHashDefault,
		},
	})
	require.NoError(t, err)

	raw = append(raw, 0xff)
	_, err = decodeExternalSignatures(raw)
	require.ErrorContains(t, err, "trailing bytes")
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
		Hash: chainhash.Hash{
			9,
			9,
			9,
		},
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

// TestDriveEventRequestRoundTripIncomingMetadataResolvedEvent asserts
// DriveEventRequest TLV Encode/Decode round-trips resolved incoming metadata
// even when the indexer cannot return a singular tree path.
func TestDriveEventRequestRoundTripIncomingMetadataResolvedEvent(t *testing.T) {
	t.Parallel()

	sessionID := SessionID(chainhash.Hash{10, 10, 10})
	commitmentTxID := chainhash.Hash{11, 11, 11}
	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	msg := &DriveEventRequest{
		SessionID: sessionID,
		Event: &IncomingMetadataResolvedEvent{
			Matches: []IncomingMetadataMatch{{
				OutputIndex: 1,
				Metadata: IncomingVTXOMetadata{
					RoundID:        "mixed-lineage",
					CommitmentTxID: commitmentTxID,
					BatchExpiry:    144,
					ChainDepth:     2,
					CreatedHeight:  42,
					OperatorKey:    operatorKey.PubKey(),
					Ancestry: []vtxo.Ancestry{{
						CommitmentTxID: commitmentTxID,
						TreeDepth:      0,
					}},
				},
			}},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, msg.Encode(&buf))

	decoded := &DriveEventRequest{}
	require.NoError(t, decoded.Decode(&buf))

	require.Equal(t, sessionID, decoded.SessionID)

	metadataEvt, ok := decoded.Event.(*IncomingMetadataResolvedEvent)
	require.True(t, ok)
	require.Len(t, metadataEvt.Matches, 1)

	match := metadataEvt.Matches[0]
	require.EqualValues(t, 1, match.OutputIndex)
	require.Equal(t, "mixed-lineage", match.Metadata.RoundID)
	require.Equal(t, commitmentTxID, match.Metadata.CommitmentTxID)
	require.EqualValues(t, 144, match.Metadata.BatchExpiry)
	require.EqualValues(t, 2, match.Metadata.ChainDepth)
	require.EqualValues(t, 42, match.Metadata.CreatedHeight)
	require.True(
		t,
		match.Metadata.OperatorKey.IsEqual(
			operatorKey.PubKey(),
		),
	)
	require.Len(t, match.Metadata.Ancestry, 1)
	require.Equal(
		t, commitmentTxID, match.Metadata.Ancestry[0].CommitmentTxID,
	)
	require.Nil(t, match.Metadata.Ancestry[0].TreePath)
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
		SessionID: sessionID,
		RecipientPkScript: []byte{
			0x51,
			0x20,
			0x01,
			0x02,
		},
		RecipientEventID: 7,
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
		SessionID(
			chainhash.Hash{1, 2, 3},
		),
		nil,
	)
	require.ErrorContains(t, err, "event must be provided")
}

// TestEncodeConditionWitnessRejectsTooManyItems verifies the encode path
// refuses to emit a condition witness that exceeds the per-witness
// item-count cap, so in-memory state cannot diverge from what the
// decoder will accept.
func TestEncodeConditionWitnessRejectsTooManyItems(t *testing.T) {
	t.Parallel()

	items := make([][]byte, maxConditionWitnessItems+1)
	for i := range items {
		items[i] = []byte{0x01}
	}

	_, err := encodeConditionWitness(items)
	require.ErrorContains(t, err, "count")
}

// TestEncodeConditionWitnessRejectsOversizeItem verifies the encode path
// refuses to emit a witness item larger than Bitcoin's standard witness
// element size.
func TestEncodeConditionWitnessRejectsOversizeItem(t *testing.T) {
	t.Parallel()

	items := [][]byte{make([]byte, maxConditionWitnessItemBytes+1)}

	_, err := encodeConditionWitness(items)
	require.ErrorContains(t, err, "size")
}

// TestDecodeConditionWitnessRejectsTooManyItems verifies the decoder
// fails fast on a blob that claims more than maxConditionWitnessItems
// items, preventing a large make([][]byte, count) allocation.
func TestDecodeConditionWitnessRejectsTooManyItems(t *testing.T) {
	t.Parallel()

	// Hand-craft a blob with a varint claiming many items. We only
	// need the count prefix; the decoder must reject before touching
	// the (absent) items.
	var buf bytes.Buffer
	require.NoError(
		t,
		wire.WriteVarInt(
			&buf, 0, uint64(maxConditionWitnessItems+1),
		),
	)

	_, err := decodeConditionWitness(buf.Bytes())
	require.ErrorContains(t, err, "count")
}

// TestDecodeConditionWitnessRejectsOversizeItem verifies the decoder
// rejects a blob whose item size exceeds the standard witness element
// cap.
func TestDecodeConditionWitnessRejectsOversizeItem(t *testing.T) {
	t.Parallel()

	oversized := make([]byte, maxConditionWitnessItemBytes+1)
	_, err := encodeConditionWitness([][]byte{oversized})
	require.Error(t, err)

	// Hand-craft a blob that bypasses the encoder's check: one
	// witness item with a declared length above the decoder cap.
	var buf bytes.Buffer
	require.NoError(t, wire.WriteVarInt(&buf, 0, 1))
	require.NoError(
		t,
		wire.WriteVarInt(
			&buf, 0, uint64(maxConditionWitnessItemBytes+1),
		),
	)
	buf.Write(oversized)

	_, err = decodeConditionWitness(buf.Bytes())
	require.Error(t, err)
}

// TestEncodeDecodeConditionWitnessRoundTripAtCapBoundaries verifies a
// round-trip at exactly the allowed maximums still succeeds so the caps
// don't accidentally reject legitimate inputs at their boundary.
func TestEncodeDecodeConditionWitnessRoundTripAtCapBoundaries(t *testing.T) {
	t.Parallel()

	items := make([][]byte, maxConditionWitnessItems)
	for i := range items {
		items[i] = make([]byte, maxConditionWitnessItemBytes)
		items[i][0] = byte(i)
	}

	encoded, err := encodeConditionWitness(items)
	require.NoError(t, err)

	decoded, err := decodeConditionWitness(encoded)
	require.NoError(t, err)
	require.Equal(t, items, decoded)
}
