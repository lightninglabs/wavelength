package oor

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/tx/psbtutil"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	startPayloadOperatorKeyRecordType tlv.Type = 1
	startPayloadCSVDelayRecordType    tlv.Type = 2
	startPayloadInputsRecordType      tlv.Type = 3
	startPayloadRecipientsRecordType  tlv.Type = 4
)

const (
	sessionPayloadSessionIDRecordType tlv.Type = 1
)

const (
	restorePayloadSnapshotRecordType tlv.Type = 1
)

const (
	driveEventPayloadSessionIDRecordType tlv.Type = 1
	driveEventPayloadEventRecordType     tlv.Type = 3
)

const (
	eventPayloadKindRecordType            tlv.Type = 1
	eventPayloadSubmitSessionIDRecordType tlv.Type = 3
	eventPayloadArkPSBTRecordType         tlv.Type = 5
	eventPayloadCheckpointPSBTsRecordType tlv.Type = 7
	eventPayloadReasonRecordType          tlv.Type = 9
)

const (
	eventKindSubmitAccepted    uint64 = 1
	eventKindCheckpointsSigned uint64 = 2
	eventKindFinalizeAccepted  uint64 = 3
	eventKindInputsMarkedSpent uint64 = 4
	eventKindFail              uint64 = 5
	eventKindRetryDue          uint64 = 6
)

const (
	transferInputOutpointRecordType        tlv.Type = 1
	transferInputAmountSatRecordType       tlv.Type = 2
	transferInputClientFamilyRecordType    tlv.Type = 3
	transferInputClientIndexRecordType     tlv.Type = 4
	transferInputClientPubKeyRecordType    tlv.Type = 5
	transferInputOperatorPubKeyRecordType  tlv.Type = 6
	transferInputExitDelayRecordType       tlv.Type = 7
	transferInputOwnerLeafScriptRecordType tlv.Type = 8
)

const (
	recipientPkScriptRecordType tlv.Type = 1
	recipientValueSatRecordType tlv.Type = 2
)

type startTransferPayload struct {
	OperatorPubKey []byte
	CSVDelay       uint32
	Inputs         []*TransferInputSnapshot
	Recipients     []recipientPayload
}

type recipientPayload struct {
	PkScript []byte
	ValueSat int64
}

func encodeStartTransferPayload(payload startTransferPayload) ([]byte, error) {
	inputs, err := encodeTransferInputSnapshots(payload.Inputs)
	if err != nil {
		return nil, err
	}

	recipients, err := encodeRecipientPayloads(payload.Recipients)
	if err != nil {
		return nil, err
	}

	operatorKey := payload.OperatorPubKey
	csvDelay := payload.CSVDelay

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			startPayloadOperatorKeyRecordType, &operatorKey,
		),
		tlv.MakePrimitiveRecord(
			startPayloadCSVDelayRecordType, &csvDelay,
		),
		tlv.MakePrimitiveRecord(
			startPayloadInputsRecordType, &inputs,
		),
		tlv.MakePrimitiveRecord(
			startPayloadRecipientsRecordType, &recipients,
		),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if err := stream.Encode(&buf); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func decodeStartTransferPayload(raw []byte) (startTransferPayload, error) {
	var (
		operatorKey []byte
		csvDelay    uint32
		inputsRaw   []byte
		recipients  []byte
	)

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			startPayloadOperatorKeyRecordType, &operatorKey,
		),
		tlv.MakePrimitiveRecord(
			startPayloadCSVDelayRecordType, &csvDelay,
		),
		tlv.MakePrimitiveRecord(
			startPayloadInputsRecordType, &inputsRaw,
		),
		tlv.MakePrimitiveRecord(
			startPayloadRecipientsRecordType, &recipients,
		),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return startTransferPayload{}, err
	}

	reader := bytes.NewReader(raw)
	if _, err := stream.DecodeWithParsedTypes(reader); err != nil {
		return startTransferPayload{}, err
	}

	inputs, err := decodeTransferInputSnapshots(inputsRaw)
	if err != nil {
		return startTransferPayload{}, err
	}

	recipientsPayload, err := decodeRecipientPayloads(recipients)
	if err != nil {
		return startTransferPayload{}, err
	}

	return startTransferPayload{
		OperatorPubKey: operatorKey,
		CSVDelay:       csvDelay,
		Inputs:         inputs,
		Recipients:     recipientsPayload,
	}, nil
}

func encodeRecipientPayloads(payloads []recipientPayload) ([]byte, error) {
	blobs := make([][]byte, 0, len(payloads))
	for i := range payloads {
		raw, err := encodeRecipientPayload(payloads[i])
		if err != nil {
			return nil, err
		}

		blobs = append(blobs, raw)
	}

	return encodeLengthPrefixedBlobList(blobs)
}

func decodeRecipientPayloads(raw []byte) ([]recipientPayload, error) {
	blobs, err := decodeLengthPrefixedBlobList(raw)
	if err != nil {
		return nil, err
	}

	payloads := make([]recipientPayload, 0, len(blobs))
	for i := range blobs {
		payload, err := decodeRecipientPayload(blobs[i])
		if err != nil {
			return nil, err
		}

		payloads = append(payloads, payload)
	}

	return payloads, nil
}

func encodeRecipientPayload(payload recipientPayload) ([]byte, error) {
	pkScript := payload.PkScript
	if payload.ValueSat < 0 {
		return nil, fmt.Errorf("recipient value must be non-negative")
	}
	valueSat := uint64(payload.ValueSat)

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(recipientPkScriptRecordType, &pkScript),
		tlv.MakePrimitiveRecord(recipientValueSatRecordType, &valueSat),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if err := stream.Encode(&buf); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func decodeRecipientPayload(raw []byte) (recipientPayload, error) {
	var (
		pkScript []byte
		valueSat uint64
	)

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(recipientPkScriptRecordType, &pkScript),
		tlv.MakePrimitiveRecord(recipientValueSatRecordType, &valueSat),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return recipientPayload{}, err
	}

	reader := bytes.NewReader(raw)
	if _, err := stream.DecodeWithParsedTypes(reader); err != nil {
		return recipientPayload{}, err
	}

	decodedValueSat, err := uint64ToInt64(
		valueSat, "recipient value sat",
	)
	if err != nil {
		return recipientPayload{}, err
	}

	return recipientPayload{
		PkScript: pkScript,
		ValueSat: decodedValueSat,
	}, nil
}

func encodeTransferInputSnapshots(inputs []*TransferInputSnapshot) ([]byte,
	error) {

	blobs := make([][]byte, 0, len(inputs))
	for i := range inputs {
		raw, err := encodeTransferInputSnapshot(inputs[i])
		if err != nil {
			return nil, err
		}

		blobs = append(blobs, raw)
	}

	return encodeLengthPrefixedBlobList(blobs)
}

func decodeTransferInputSnapshots(raw []byte) ([]*TransferInputSnapshot,
	error) {

	blobs, err := decodeLengthPrefixedBlobList(raw)
	if err != nil {
		return nil, err
	}

	inputs := make([]*TransferInputSnapshot, 0, len(blobs))
	for i := range blobs {
		input, err := decodeTransferInputSnapshot(blobs[i])
		if err != nil {
			return nil, err
		}

		inputs = append(inputs, input)
	}

	return inputs, nil
}

func encodeTransferInputSnapshot(input *TransferInputSnapshot) ([]byte, error) {
	if input == nil {
		return nil, fmt.Errorf(
			"transfer input snapshot must be provided",
		)
	}

	outpoint := outPointBytes(input.Outpoint)
	amountSat := uint64(input.AmountSat)
	clientFamily := uint32(input.ClientKeyFamily)
	clientIndex := input.ClientKeyIndex
	clientPubKey := input.ClientPubKey
	operatorPubKey := input.OperatorPubKey
	exitDelay := input.ExitDelay
	ownerLeafScript := input.OwnerLeafScript

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			transferInputOutpointRecordType, &outpoint,
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
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if err := stream.Encode(&buf); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func decodeTransferInputSnapshot(raw []byte) (*TransferInputSnapshot, error) {
	var (
		outpointRaw     []byte
		amountSat       uint64
		clientFamily    uint32
		clientIndex     uint32
		clientPubKey    []byte
		operatorPubKey  []byte
		exitDelay       uint32
		ownerLeafScript []byte
	)

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
	if err != nil {
		return nil, err
	}

	reader := bytes.NewReader(raw)
	if _, err := stream.DecodeWithParsedTypes(reader); err != nil {
		return nil, err
	}

	outpoint, err := parseOutPointBytes(outpointRaw)
	if err != nil {
		return nil, err
	}

	decodedAmountSat, err := uint64ToInt64(amountSat, "amount sat")
	if err != nil {
		return nil, err
	}

	decodedClientFamily, err := uint32ToInt32(
		clientFamily, "client key family",
	)
	if err != nil {
		return nil, err
	}

	return &TransferInputSnapshot{
		Outpoint:        outpoint,
		AmountSat:       decodedAmountSat,
		ClientKeyFamily: decodedClientFamily,
		ClientKeyIndex:  clientIndex,
		ClientPubKey:    clientPubKey,
		OperatorPubKey:  operatorPubKey,
		ExitDelay:       exitDelay,
		OwnerLeafScript: ownerLeafScript,
	}, nil
}

func encodeSessionPayload(sessionID SessionID) ([]byte, error) {
	sessionBytes := sessionIDBytes(sessionID)
	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			sessionPayloadSessionIDRecordType, &sessionBytes,
		),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if err := stream.Encode(&buf); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func decodeSessionPayload(raw []byte) (SessionID, error) {
	var sessionBytes []byte
	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			sessionPayloadSessionIDRecordType, &sessionBytes,
		),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return SessionID{}, err
	}

	reader := bytes.NewReader(raw)
	if _, err := stream.DecodeWithParsedTypes(reader); err != nil {
		return SessionID{}, err
	}

	return parseSessionID(sessionBytes)
}

func encodeRestoreSnapshotPayload(snapshot *OutgoingSnapshot) ([]byte, error) {
	snapshotRaw, err := encodeOutgoingSnapshot(snapshot)
	if err != nil {
		return nil, err
	}

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			restorePayloadSnapshotRecordType, &snapshotRaw,
		),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if err := stream.Encode(&buf); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func decodeRestoreSnapshotPayload(raw []byte) (*OutgoingSnapshot, error) {
	var snapshotRaw []byte
	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			restorePayloadSnapshotRecordType, &snapshotRaw,
		),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, err
	}

	reader := bytes.NewReader(raw)
	if _, err := stream.DecodeWithParsedTypes(reader); err != nil {
		return nil, err
	}

	return decodeOutgoingSnapshot(snapshotRaw)
}

func encodeDriveEventRequestPayload(sessionID SessionID, event Event) ([]byte,
	error) {

	if event == nil {
		return nil, fmt.Errorf("event must be provided")
	}

	if submitAccepted, ok := event.(*SubmitAcceptedEvent); ok {
		if err := validateSubmitAcceptedIdentity(
			sessionID, submitAccepted,
		); err != nil {
			return nil, err
		}
	}

	sessionBytes := sessionIDBytes(sessionID)
	eventPayload, err := encodeEventPayload(event)
	if err != nil {
		return nil, err
	}

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			driveEventPayloadSessionIDRecordType, &sessionBytes,
		),
		tlv.MakePrimitiveRecord(
			driveEventPayloadEventRecordType, &eventPayload,
		),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if err := stream.Encode(&buf); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func decodeDriveEventRequestPayload(raw []byte) (SessionID, Event, error) {
	var (
		sessionBytes []byte
		eventPayload []byte
	)

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			driveEventPayloadSessionIDRecordType, &sessionBytes,
		),
		tlv.MakePrimitiveRecord(
			driveEventPayloadEventRecordType, &eventPayload,
		),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return SessionID{}, nil, err
	}

	reader := bytes.NewReader(raw)
	if _, err := stream.DecodeWithParsedTypes(reader); err != nil {
		return SessionID{}, nil, err
	}

	sessionID, err := parseSessionID(sessionBytes)
	if err != nil {
		return SessionID{}, nil, err
	}

	event, err := decodeEventPayload(eventPayload)
	if err != nil {
		return SessionID{}, nil, err
	}

	if submitAccepted, ok := event.(*SubmitAcceptedEvent); ok {
		if err := validateSubmitAcceptedIdentity(
			sessionID, submitAccepted,
		); err != nil {
			return SessionID{}, nil, err
		}
	}

	return sessionID, event, nil
}

func encodeEventPayload(event Event) ([]byte, error) {
	var (
		eventKind      uint64
		submitSession  []byte
		arkPSBT        []byte
		checkpointPSBT []byte
		reason         []byte
		err            error
	)

	switch evt := event.(type) {
	case *SubmitAcceptedEvent:
		eventKind = eventKindSubmitAccepted
		submitSession = sessionIDBytes(evt.SessionID)
		arkPSBT, err = psbtutil.Serialize(evt.ArkPSBT)
		if err != nil {
			return nil, err
		}

		checkpoints, err := serializePSBTSlice(
			evt.CoSignedCheckpointPSBTs,
		)
		if err != nil {
			return nil, err
		}
		checkpointPSBT, err = encodeLengthPrefixedBlobList(checkpoints)
		if err != nil {
			return nil, err
		}

	case *CheckpointsSignedEvent:
		eventKind = eventKindCheckpointsSigned
		checkpoints, err := serializePSBTSlice(evt.FinalCheckpointPSBTs)
		if err != nil {
			return nil, err
		}
		checkpointPSBT, err = encodeLengthPrefixedBlobList(checkpoints)
		if err != nil {
			return nil, err
		}

	case *FinalizeAcceptedEvent:
		eventKind = eventKindFinalizeAccepted

	case *InputsMarkedSpentEvent:
		eventKind = eventKindInputsMarkedSpent

	case *FailEvent:
		eventKind = eventKindFail
		reason = []byte(evt.Reason)

	case *RetryDueEvent:
		eventKind = eventKindRetryDue

	default:
		return nil, fmt.Errorf("unsupported event type: %T", event)
	}

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(eventPayloadKindRecordType, &eventKind),
		tlv.MakePrimitiveRecord(
			eventPayloadSubmitSessionIDRecordType, &submitSession,
		),
		tlv.MakePrimitiveRecord(
			eventPayloadArkPSBTRecordType, &arkPSBT,
		),
		tlv.MakePrimitiveRecord(
			eventPayloadCheckpointPSBTsRecordType, &checkpointPSBT,
		),
		tlv.MakePrimitiveRecord(eventPayloadReasonRecordType, &reason),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if err := stream.Encode(&buf); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func decodeEventPayload(raw []byte) (Event, error) {
	var (
		eventKind      uint64
		submitSession  []byte
		arkPSBT        []byte
		checkpointPSBT []byte
		reason         []byte
	)

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(eventPayloadKindRecordType, &eventKind),
		tlv.MakePrimitiveRecord(
			eventPayloadSubmitSessionIDRecordType, &submitSession,
		),
		tlv.MakePrimitiveRecord(
			eventPayloadArkPSBTRecordType, &arkPSBT,
		),
		tlv.MakePrimitiveRecord(
			eventPayloadCheckpointPSBTsRecordType, &checkpointPSBT,
		),
		tlv.MakePrimitiveRecord(eventPayloadReasonRecordType, &reason),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, err
	}

	reader := bytes.NewReader(raw)
	if _, err := stream.DecodeWithParsedTypes(reader); err != nil {
		return nil, err
	}

	switch eventKind {
	case eventKindSubmitAccepted:
		sessionID, err := parseSessionID(submitSession)
		if err != nil {
			return nil, err
		}

		ark, err := psbtutil.Parse(arkPSBT)
		if err != nil {
			return nil, err
		}

		checkpointRaw, err := decodeLengthPrefixedBlobList(
			checkpointPSBT,
		)
		if err != nil {
			return nil, err
		}

		checkpoints, err := parsePSBTSlice(checkpointRaw)
		if err != nil {
			return nil, err
		}

		return &SubmitAcceptedEvent{
			SessionID:               sessionID,
			ArkPSBT:                 ark,
			CoSignedCheckpointPSBTs: checkpoints,
		}, nil

	case eventKindCheckpointsSigned:
		checkpointRaw, err := decodeLengthPrefixedBlobList(
			checkpointPSBT,
		)
		if err != nil {
			return nil, err
		}

		checkpoints, err := parsePSBTSlice(checkpointRaw)
		if err != nil {
			return nil, err
		}

		return &CheckpointsSignedEvent{
			FinalCheckpointPSBTs: checkpoints,
		}, nil

	case eventKindFinalizeAccepted:
		return &FinalizeAcceptedEvent{}, nil

	case eventKindInputsMarkedSpent:
		return &InputsMarkedSpentEvent{}, nil

	case eventKindFail:
		return &FailEvent{Reason: string(reason)}, nil

	case eventKindRetryDue:
		return &RetryDueEvent{}, nil

	default:
		return nil, fmt.Errorf("unknown event kind: %d", eventKind)
	}
}

func validateSubmitAcceptedIdentity(sessionID SessionID,
	event *SubmitAcceptedEvent) error {

	if event == nil {
		return fmt.Errorf("submit accepted event must be provided")
	}

	if event.SessionID != sessionID {
		return fmt.Errorf("submit accepted event session id mismatch")
	}

	if event.ArkPSBT == nil || event.ArkPSBT.UnsignedTx == nil {
		return fmt.Errorf(
			"submit accepted event ark psbt must be provided",
		)
	}

	arkSessionID, err := sessionIDFromArk(event.ArkPSBT)
	if err != nil {
		return err
	}

	if arkSessionID != sessionID {
		return fmt.Errorf("submit accepted event ark txid mismatch")
	}

	return nil
}

func outPointBytes(out wire.OutPoint) []byte {
	raw := make([]byte, chainhash.HashSize+4)
	copy(raw[:chainhash.HashSize], out.Hash[:])
	binary.LittleEndian.PutUint32(raw[chainhash.HashSize:], out.Index)

	return raw
}

func parseOutPointBytes(raw []byte) (wire.OutPoint, error) {
	if len(raw) != chainhash.HashSize+4 {
		return wire.OutPoint{}, fmt.Errorf("invalid outpoint length")
	}

	var out wire.OutPoint
	copy(out.Hash[:], raw[:chainhash.HashSize])
	out.Index = binary.LittleEndian.Uint32(raw[chainhash.HashSize:])

	return out, nil
}

func encodeLengthPrefixedBlobList(blobs [][]byte) ([]byte, error) {
	var (
		buf     bytes.Buffer
		scratch [8]byte
	)

	if err := tlv.WriteVarInt(
		&buf, uint64(len(blobs)), &scratch,
	); err != nil {
		return nil, err
	}

	for i := range blobs {
		element := blobs[i]

		if err := tlv.WriteVarInt(
			&buf, uint64(len(element)), &scratch,
		); err != nil {
			return nil, err
		}

		if _, err := buf.Write(element); err != nil {
			return nil, err
		}
	}

	return buf.Bytes(), nil
}

func decodeLengthPrefixedBlobList(raw []byte) ([][]byte, error) {
	var scratch [8]byte

	reader := bytes.NewReader(raw)
	count, err := tlv.ReadVarInt(reader, &scratch)
	if err != nil {
		return nil, err
	}

	blobs := make([][]byte, 0, count)
	for i := uint64(0); i < count; i++ {
		elementLen, err := tlv.ReadVarInt(reader, &scratch)
		if err != nil {
			return nil, err
		}

		element := make([]byte, elementLen)
		if _, err := io.ReadFull(reader, element); err != nil {
			return nil, err
		}

		blobs = append(blobs, element)
	}

	if reader.Len() != 0 {
		return nil, fmt.Errorf("trailing payload bytes")
	}

	return blobs, nil
}

func sessionIDBytes(sessionID SessionID) []byte {
	h := [32]byte(sessionID)
	raw := make([]byte, len(h))
	copy(raw, h[:])

	return raw
}

func parseSessionID(raw []byte) (SessionID, error) {
	if len(raw) != chainhash.HashSize {
		return SessionID{}, fmt.Errorf("invalid session id length")
	}

	var hash chainhash.Hash
	copy(hash[:], raw)

	return SessionID(hash), nil
}

func uint64ToInt64(value uint64, field string) (int64, error) {
	if value > math.MaxInt64 {
		return 0, fmt.Errorf("%s overflows int64: %d", field, value)
	}

	return int64(value), nil
}

func uint32ToInt32(value uint32, field string) (int32, error) {
	if value > math.MaxInt32 {
		return 0, fmt.Errorf("%s overflows int32: %d", field, value)
	}

	return int32(value), nil
}
