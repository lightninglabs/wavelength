package oor

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	clientdb "github.com/lightninglabs/darepo-client/db"
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
	resolveIncomingPayloadSessionIDRecordType tlv.Type = 1
	resolveIncomingPayloadPkScriptRecordType  tlv.Type = 2
	resolveIncomingPayloadEventIDRecordType   tlv.Type = 3
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
	eventPayloadOutpointsRecordType       tlv.Type = 11
	eventPayloadMetadataMatchesRecordType tlv.Type = 13
)

const (
	eventKindSubmitAccepted    uint64 = 1
	eventKindCheckpointsSigned uint64 = 2
	eventKindFinalizeAccepted  uint64 = 3
	eventKindInputsMarkedSpent uint64 = 4
	eventKindFail              uint64 = 5
	eventKindRetryDue          uint64 = 6
	eventKindIncomingTransfer  uint64 = 7
	eventKindIncomingHandled   uint64 = 8
	eventKindIncomingAckSent   uint64 = 9
	eventKindIncomingMetadata  uint64 = 10
)

const (
	transferInputOutpointRecordType        tlv.Type = 1
	transferInputAmountSatRecordType       tlv.Type = 2
	transferInputClientFamilyRecordType    tlv.Type = 3
	transferInputClientIndexRecordType     tlv.Type = 4
	transferInputClientPubKeyRecordType    tlv.Type = 5
	transferInputOperatorPubKeyRecordType  tlv.Type = 6
	transferInputExitDelayRecordType       tlv.Type = 7
	transferInputOwnerLeafScriptRecordType     tlv.Type = 8
	transferInputPkScriptRecordType            tlv.Type = 9
	transferInputSpendWitnessScriptRecordType  tlv.Type = 10
	transferInputSpendControlBlockRecordType   tlv.Type = 11
	transferInputConditionWitnessRecordType    tlv.Type = 12
)

const (
	recipientPkScriptRecordType tlv.Type = 1
	recipientValueSatRecordType tlv.Type = 2
)

const (
	incomingMetadataMatchOutputIndexRecordType    tlv.Type = 1
	incomingMetadataMatchRoundIDRecordType        tlv.Type = 3
	incomingMetadataMatchCommitmentTxIDRecordType tlv.Type = 5
	incomingMetadataMatchBatchExpiryRecordType    tlv.Type = 7
	incomingMetadataMatchTreeDepthRecordType      tlv.Type = 9
	incomingMetadataMatchChainDepthRecordType     tlv.Type = 11
	incomingMetadataMatchCreatedHeightRecordType  tlv.Type = 13
	incomingMetadataMatchTreePathRecordType       tlv.Type = 15
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

func encodeOutPointList(outpoints []wire.OutPoint) ([]byte, error) {
	blobs := make([][]byte, 0, len(outpoints))

	for _, outpoint := range outpoints {
		blobs = append(blobs, outPointBytes(outpoint))
	}

	return encodeLengthPrefixedBlobList(blobs)
}

func decodeOutPointList(raw []byte) ([]wire.OutPoint, error) {
	blobs, err := decodeLengthPrefixedBlobList(raw)
	if err != nil {
		return nil, err
	}

	outpoints := make([]wire.OutPoint, 0, len(blobs))
	for _, blob := range blobs {
		outpoint, err := parseOutPointBytes(blob)
		if err != nil {
			return nil, err
		}

		outpoints = append(outpoints, outpoint)
	}

	return outpoints, nil
}

func encodeIncomingMetadataMatches(matches []IncomingMetadataMatch) ([]byte,
	error) {

	blobs := make([][]byte, 0, len(matches))
	for i := range matches {
		raw, err := encodeIncomingMetadataMatch(matches[i])
		if err != nil {
			return nil, err
		}

		blobs = append(blobs, raw)
	}

	return encodeLengthPrefixedBlobList(blobs)
}

func decodeIncomingMetadataMatches(
	raw []byte) ([]IncomingMetadataMatch, error) {

	blobs, err := decodeLengthPrefixedBlobList(raw)
	if err != nil {
		return nil, err
	}

	matches := make([]IncomingMetadataMatch, 0, len(blobs))
	for i := range blobs {
		match, err := decodeIncomingMetadataMatch(blobs[i])
		if err != nil {
			return nil, err
		}

		matches = append(matches, match)
	}

	return matches, nil
}

func encodeIncomingMetadataMatch(match IncomingMetadataMatch) ([]byte, error) {
	outputIndex := match.OutputIndex
	roundID := []byte(match.Metadata.RoundID)
	commitmentTxID := match.Metadata.CommitmentTxID[:]
	batchExpiry := uint32(match.Metadata.BatchExpiry)
	treeDepth := uint32(match.Metadata.TreeDepth)
	chainDepth := uint32(match.Metadata.ChainDepth)
	createdHeight := uint32(match.Metadata.CreatedHeight)

	treePath, err := clientdb.SerializeTree(match.Metadata.TreePath)
	if err != nil {
		return nil, err
	}

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			incomingMetadataMatchOutputIndexRecordType,
			&outputIndex,
		),
		tlv.MakePrimitiveRecord(
			incomingMetadataMatchRoundIDRecordType, &roundID,
		),
		tlv.MakePrimitiveRecord(
			incomingMetadataMatchCommitmentTxIDRecordType,
			&commitmentTxID,
		),
		tlv.MakePrimitiveRecord(
			incomingMetadataMatchBatchExpiryRecordType,
			&batchExpiry,
		),
		tlv.MakePrimitiveRecord(
			incomingMetadataMatchTreeDepthRecordType, &treeDepth,
		),
		tlv.MakePrimitiveRecord(
			incomingMetadataMatchChainDepthRecordType,
			&chainDepth,
		),
		tlv.MakePrimitiveRecord(
			incomingMetadataMatchCreatedHeightRecordType,
			&createdHeight,
		),
		tlv.MakePrimitiveRecord(
			incomingMetadataMatchTreePathRecordType, &treePath,
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

func decodeIncomingMetadataMatch(raw []byte) (IncomingMetadataMatch, error) {
	var (
		outputIndex    uint32
		roundID        []byte
		commitmentTxID []byte
		batchExpiry    uint32
		treeDepth      uint32
		chainDepth     uint32
		createdHeight  uint32
		treePath       []byte
	)

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			incomingMetadataMatchOutputIndexRecordType,
			&outputIndex,
		),
		tlv.MakePrimitiveRecord(
			incomingMetadataMatchRoundIDRecordType, &roundID,
		),
		tlv.MakePrimitiveRecord(
			incomingMetadataMatchCommitmentTxIDRecordType,
			&commitmentTxID,
		),
		tlv.MakePrimitiveRecord(
			incomingMetadataMatchBatchExpiryRecordType,
			&batchExpiry,
		),
		tlv.MakePrimitiveRecord(
			incomingMetadataMatchTreeDepthRecordType, &treeDepth,
		),
		tlv.MakePrimitiveRecord(
			incomingMetadataMatchChainDepthRecordType,
			&chainDepth,
		),
		tlv.MakePrimitiveRecord(
			incomingMetadataMatchCreatedHeightRecordType,
			&createdHeight,
		),
		tlv.MakePrimitiveRecord(
			incomingMetadataMatchTreePathRecordType, &treePath,
		),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return IncomingMetadataMatch{}, err
	}

	reader := bytes.NewReader(raw)
	if _, err := stream.DecodeWithParsedTypes(reader); err != nil {
		return IncomingMetadataMatch{}, err
	}

	if len(commitmentTxID) != chainhash.HashSize {
		return IncomingMetadataMatch{}, fmt.Errorf(
			"incoming metadata commitment txid " +
				"must be provided",
		)
	}

	decodedBatchExpiry, err := uint32ToInt32(
		batchExpiry, "incoming batch expiry",
	)
	if err != nil {
		return IncomingMetadataMatch{}, err
	}

	decodedTreeDepth, err := uint32ToInt32(
		treeDepth, "incoming tree depth",
	)
	if err != nil {
		return IncomingMetadataMatch{}, err
	}

	decodedChainDepth, err := uint32ToInt32(
		chainDepth, "incoming chain depth",
	)
	if err != nil {
		return IncomingMetadataMatch{}, err
	}

	decodedCreatedHeight, err := uint32ToInt32(
		createdHeight, "incoming created height",
	)
	if err != nil {
		return IncomingMetadataMatch{}, err
	}

	decodedTreePath, err := clientdb.DeserializeTree(treePath)
	if err != nil {
		return IncomingMetadataMatch{}, err
	}

	var decodedCommitmentTxID chainhash.Hash
	copy(decodedCommitmentTxID[:], commitmentTxID)

	return IncomingMetadataMatch{
		OutputIndex: outputIndex,
		Metadata: IncomingVTXOMetadata{
			RoundID:        string(roundID),
			CommitmentTxID: decodedCommitmentTxID,
			BatchExpiry:    decodedBatchExpiry,
			TreeDepth:      int(decodedTreeDepth),
			ChainDepth:     int(decodedChainDepth),
			CreatedHeight:  decodedCreatedHeight,
			TreePath:       decodedTreePath,
		},
	}, nil
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

	// Optional custom spend path fields.
	pkScript := input.PkScript
	if len(pkScript) > 0 {
		records = append(records, tlv.MakePrimitiveRecord(
			transferInputPkScriptRecordType, &pkScript,
		))
	}

	witnessScript := input.SpendWitnessScript
	if len(witnessScript) > 0 {
		records = append(records, tlv.MakePrimitiveRecord(
			transferInputSpendWitnessScriptRecordType,
			&witnessScript,
		))
	}

	controlBlock := input.SpendControlBlock
	if len(controlBlock) > 0 {
		records = append(records, tlv.MakePrimitiveRecord(
			transferInputSpendControlBlockRecordType,
			&controlBlock,
		))
	}

	if len(input.ConditionWitness) > 0 {
		condBlob, condErr := encodeConditionWitness(
			input.ConditionWitness,
		)
		if condErr != nil {
			return nil, condErr
		}

		records = append(records, tlv.MakePrimitiveRecord(
			transferInputConditionWitnessRecordType,
			&condBlob,
		))
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
		pkScript        []byte
		witnessScript   []byte
		controlBlock    []byte
		condBlob        []byte
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
		tlv.MakePrimitiveRecord(
			transferInputPkScriptRecordType, &pkScript,
		),
		tlv.MakePrimitiveRecord(
			transferInputSpendWitnessScriptRecordType,
			&witnessScript,
		),
		tlv.MakePrimitiveRecord(
			transferInputSpendControlBlockRecordType,
			&controlBlock,
		),
		tlv.MakePrimitiveRecord(
			transferInputConditionWitnessRecordType,
			&condBlob,
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

	snap := &TransferInputSnapshot{
		Outpoint:           outpoint,
		AmountSat:          decodedAmountSat,
		ClientKeyFamily:    decodedClientFamily,
		ClientKeyIndex:     clientIndex,
		ClientPubKey:       clientPubKey,
		OperatorPubKey:     operatorPubKey,
		ExitDelay:          exitDelay,
		OwnerLeafScript:    ownerLeafScript,
		PkScript:           pkScript,
		SpendWitnessScript: witnessScript,
		SpendControlBlock:  controlBlock,
	}

	if len(condBlob) > 0 {
		items, condErr := decodeConditionWitness(condBlob)
		if condErr != nil {
			return nil, condErr
		}

		snap.ConditionWitness = items
	}

	return snap, nil
}

// encodeConditionWitness serializes a list of witness items as
// count-prefixed length-prefixed byte strings.
func encodeConditionWitness(items [][]byte) ([]byte, error) {
	var buf bytes.Buffer

	if err := wire.WriteVarInt(&buf, 0, uint64(len(items))); err != nil {
		return nil, err
	}

	for _, item := range items {
		if err := wire.WriteVarBytes(&buf, 0, item); err != nil {
			return nil, err
		}
	}

	return buf.Bytes(), nil
}

// decodeConditionWitness deserializes a list of witness items.
func decodeConditionWitness(raw []byte) ([][]byte, error) {
	r := bytes.NewReader(raw)

	count, err := wire.ReadVarInt(r, 0)
	if err != nil {
		return nil, err
	}

	items := make([][]byte, count)
	for i := uint64(0); i < count; i++ {
		item, readErr := wire.ReadVarBytes(
			r, 0, 10000, "condition witness item",
		)
		if readErr != nil {
			return nil, readErr
		}

		items[i] = item
	}

	return items, nil
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

func encodeResolveIncomingTransferPayload(sessionID SessionID,
	recipientPkScript []byte, recipientEventID uint64) ([]byte, error) {

	sessionBytes := sessionIDBytes(sessionID)
	pkScript := recipientPkScript
	eventID := recipientEventID

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			resolveIncomingPayloadSessionIDRecordType,
			&sessionBytes,
		),
		tlv.MakePrimitiveRecord(
			resolveIncomingPayloadPkScriptRecordType, &pkScript,
		),
		tlv.MakePrimitiveRecord(
			resolveIncomingPayloadEventIDRecordType, &eventID,
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

func decodeResolveIncomingTransferPayload(raw []byte) (SessionID, []byte,
	uint64, error) {

	var (
		sessionBytes      []byte
		recipientPkScript []byte
		recipientEventID  uint64
	)

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			resolveIncomingPayloadSessionIDRecordType,
			&sessionBytes,
		),
		tlv.MakePrimitiveRecord(
			resolveIncomingPayloadPkScriptRecordType,
			&recipientPkScript,
		),
		tlv.MakePrimitiveRecord(
			resolveIncomingPayloadEventIDRecordType,
			&recipientEventID,
		),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return SessionID{}, nil, 0, err
	}

	reader := bytes.NewReader(raw)
	if _, err := stream.DecodeWithParsedTypes(reader); err != nil {
		return SessionID{}, nil, 0, err
	}

	sessionID, err := parseSessionID(sessionBytes)
	if err != nil {
		return SessionID{}, nil, 0, err
	}

	// TODO(oor-receive): The maxPkScriptLen limit guards against
	// unbounded allocations from a corrupted or malicious TLV payload.
	// Standard Bitcoin pk_scripts are well under 10 000 bytes; raise
	// this constant via a tracked issue if a new script type requires
	// a longer encoding.
	const maxPkScriptLen = 10_000
	if len(recipientPkScript) > maxPkScriptLen {
		return SessionID{}, nil, 0, fmt.Errorf(
			"recipient pk_script length %d exceeds limit %d",
			len(recipientPkScript), maxPkScriptLen,
		)
	}

	return sessionID, recipientPkScript, recipientEventID, nil
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

	// Validation of SubmitAcceptedEvent identity (ArkPSBT, session ID
	// match) is deferred to the processing layer (handleDriveEvent)
	// rather than the serialization layer. This allows server-push
	// events dispatched via the EventRouter to be persisted to the
	// durable mailbox with nil ArkPSBT, which the actor enriches from
	// session state before validation.

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

	if incoming, ok := event.(*IncomingTransferEvent); ok {
		incoming.SessionID = sessionID
	}

	// Validation of SubmitAcceptedEvent identity is deferred to the
	// processing layer (handleDriveEvent), not the deserialization
	// layer. See encodeDriveEventRequestPayload for rationale.

	return sessionID, event, nil
}

func encodeEventPayload(event Event) ([]byte, error) {
	var (
		eventKind       uint64
		submitSession   []byte
		arkPSBT         []byte
		checkpointPSBT  []byte
		reason          []byte
		outpointPayload []byte
		metadataPayload []byte
		err             error
	)

	switch evt := event.(type) {
	case *SubmitAcceptedEvent:
		eventKind = eventKindSubmitAccepted
		submitSession = sessionIDBytes(evt.SessionID)

		// ArkPSBT may be nil for server-push events dispatched
		// via the EventRouter. The actor layer enriches it from
		// session state before processing. Encode empty bytes
		// when nil so the TLV stream is well-formed.
		if evt.ArkPSBT != nil {
			arkPSBT, err = psbtutil.Serialize(evt.ArkPSBT)
			if err != nil {
				return nil, err
			}
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

	case *IncomingTransferEvent:
		eventKind = eventKindIncomingTransfer

		if evt.ArkPSBT == nil {
			return nil, fmt.Errorf(
				"incoming transfer event " +
					"ark psbt must be provided",
			)
		}

		arkPSBT, err = psbtutil.Serialize(evt.ArkPSBT)
		if err != nil {
			return nil, err
		}

		checkpoints, err := serializePSBTSlice(
			evt.FinalCheckpointPSBTs,
		)
		if err != nil {
			return nil, err
		}

		checkpointPSBT, err = encodeLengthPrefixedBlobList(
			checkpoints,
		)
		if err != nil {
			return nil, err
		}

	case *IncomingHandledEvent:
		eventKind = eventKindIncomingHandled

		outpoints := evt.MaterializedOutpoints
		if len(outpoints) == 0 && len(evt.MaterializedVTXOs) > 0 {
			outpoints = make([]wire.OutPoint, 0,
				len(evt.MaterializedVTXOs))
			for _, desc := range evt.MaterializedVTXOs {
				if desc == nil {
					continue
				}

				outpoints = append(outpoints, desc.Outpoint)
			}
		}

		outpointPayload, err = encodeOutPointList(outpoints)
		if err != nil {
			return nil, err
		}

	case *IncomingMetadataResolvedEvent:
		eventKind = eventKindIncomingMetadata

		metadataPayload, err = encodeIncomingMetadataMatches(
			evt.Matches,
		)
		if err != nil {
			return nil, err
		}

	case *IncomingAckSentEvent:
		eventKind = eventKindIncomingAckSent

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
		tlv.MakePrimitiveRecord(
			eventPayloadOutpointsRecordType, &outpointPayload,
		),
		tlv.MakePrimitiveRecord(
			eventPayloadMetadataMatchesRecordType, &metadataPayload,
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

func decodeEventPayload(raw []byte) (Event, error) {
	var (
		eventKind       uint64
		submitSession   []byte
		arkPSBT         []byte
		checkpointPSBT  []byte
		reason          []byte
		outpointPayload []byte
		metadataPayload []byte
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
		tlv.MakePrimitiveRecord(
			eventPayloadOutpointsRecordType, &outpointPayload,
		),
		tlv.MakePrimitiveRecord(
			eventPayloadMetadataMatchesRecordType, &metadataPayload,
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

	switch eventKind {
	case eventKindSubmitAccepted:
		sessionID, err := parseSessionID(submitSession)
		if err != nil {
			return nil, err
		}

		// ArkPSBT may be empty for server-push events where the
		// proto response does not echo it back. The actor layer
		// enriches it from session state before processing.
		var ark *psbt.Packet
		if len(arkPSBT) > 0 {
			ark, err = psbtutil.Parse(arkPSBT)
			if err != nil {
				return nil, err
			}
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

	case eventKindIncomingTransfer:
		if len(arkPSBT) == 0 {
			return nil, fmt.Errorf(
				"incoming transfer event " +
					"ark psbt must be provided",
			)
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

		return &IncomingTransferEvent{
			ArkPSBT:              ark,
			FinalCheckpointPSBTs: checkpoints,
		}, nil

	case eventKindIncomingHandled:
		outpoints, err := decodeOutPointList(outpointPayload)
		if err != nil {
			return nil, err
		}

		return &IncomingHandledEvent{
			MaterializedOutpoints: outpoints,
		}, nil

	case eventKindIncomingMetadata:
		matches, err := decodeIncomingMetadataMatches(
			metadataPayload,
		)
		if err != nil {
			return nil, err
		}

		return &IncomingMetadataResolvedEvent{
			Matches: matches,
		}, nil

	case eventKindIncomingAckSent:
		return &IncomingAckSentEvent{}, nil

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

	// TODO(oor-receive): The maxBlobListCount limit is a pragmatic
	// upper bound to prevent unbounded slice allocation from a
	// malformed or malicious TLV payload. Raise this constant via a
	// tracked issue if any blob-list field legitimately needs more
	// entries.
	const maxBlobListCount = 10_000
	if count > maxBlobListCount {
		return nil, fmt.Errorf(
			"blob list count %d exceeds limit %d",
			count, maxBlobListCount,
		)
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
