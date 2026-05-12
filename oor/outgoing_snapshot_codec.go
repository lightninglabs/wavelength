package oor

import (
	"bytes"
	"fmt"
	"math"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	checkpointVersionRecordType           tlv.Type = 1
	checkpointSnapshotsRecordType         tlv.Type = 3
	checkpointIncomingSnapshotsRecordType tlv.Type = 5
)

const (
	snapshotVersionRecordType         tlv.Type = 1
	snapshotSessionIDRecordType       tlv.Type = 3
	snapshotPhaseRecordType           tlv.Type = 5
	snapshotArkPSBTRecordType         tlv.Type = 7
	snapshotCheckpointPSBTsRecordType tlv.Type = 9
	snapshotTransferInputsRecordType  tlv.Type = 11
	snapshotRetryAfterNanosRecordType tlv.Type = 15
	snapshotFailReasonRecordType      tlv.Type = 19

	// snapshotIdempotencyKeyRecordType stores the optional caller-provided
	// OOR send idempotency key.
	snapshotIdempotencyKeyRecordType tlv.Type = 21
)

type sessionsCheckpoint struct {
	Version           int
	OutgoingSnapshots []*OutgoingSnapshot
	IncomingSnapshots []*IncomingSnapshot
}

func encodeSessionsCheckpoint(checkpoint sessionsCheckpoint) ([]byte, error) {
	outgoingBlobs := make([][]byte, 0, len(checkpoint.OutgoingSnapshots))
	for i := range checkpoint.OutgoingSnapshots {
		raw, err := encodeOutgoingSnapshot(
			checkpoint.OutgoingSnapshots[i],
		)
		if err != nil {
			return nil, err
		}

		outgoingBlobs = append(outgoingBlobs, raw)
	}

	outgoingRaw, err := encodeLengthPrefixedBlobList(outgoingBlobs)
	if err != nil {
		return nil, err
	}

	incomingBlobs := make([][]byte, 0, len(checkpoint.IncomingSnapshots))
	for i := range checkpoint.IncomingSnapshots {
		raw, err := encodeIncomingSnapshot(
			checkpoint.IncomingSnapshots[i],
		)
		if err != nil {
			return nil, err
		}

		incomingBlobs = append(incomingBlobs, raw)
	}

	incomingRaw, err := encodeLengthPrefixedBlobList(incomingBlobs)
	if err != nil {
		return nil, err
	}

	version := uint64(checkpoint.Version)
	records := []tlv.Record{
		tlv.MakePrimitiveRecord(checkpointVersionRecordType, &version),
		tlv.MakePrimitiveRecord(
			checkpointSnapshotsRecordType, &outgoingRaw,
		),
		tlv.MakePrimitiveRecord(
			checkpointIncomingSnapshotsRecordType, &incomingRaw,
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

func decodeSessionsCheckpoint(raw []byte) (sessionsCheckpoint, error) {
	return decodeSessionsCheckpointWithLimits(
		raw, ReceiveLimits{},
	)
}

// decodeSessionsCheckpointWithLimits decodes a checkpoint and applies receive
// limits to all nested session snapshot lists.
func decodeSessionsCheckpointWithLimits(raw []byte,
	limits ReceiveLimits) (sessionsCheckpoint, error) {

	var (
		version     uint64
		outgoingRaw []byte
		incomingRaw []byte
	)

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(checkpointVersionRecordType, &version),
		tlv.MakePrimitiveRecord(
			checkpointSnapshotsRecordType, &outgoingRaw,
		),
		tlv.MakePrimitiveRecord(
			checkpointIncomingSnapshotsRecordType, &incomingRaw,
		),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return sessionsCheckpoint{}, err
	}

	reader := bytes.NewReader(raw)
	if _, err := stream.DecodeWithParsedTypes(reader); err != nil {
		return sessionsCheckpoint{}, err
	}

	outgoingBlobs, err := decodeLengthPrefixedBlobListWithLimits(
		outgoingRaw, limits,
	)
	if err != nil {
		return sessionsCheckpoint{}, err
	}

	outgoingSnapshots := make([]*OutgoingSnapshot, 0,
		len(outgoingBlobs))
	for i := range outgoingBlobs {
		snapshot, err := decodeOutgoingSnapshotWithLimits(
			outgoingBlobs[i], limits,
		)
		if err != nil {
			return sessionsCheckpoint{}, err
		}

		outgoingSnapshots = append(outgoingSnapshots, snapshot)
	}

	incomingSnapshots := make([]*IncomingSnapshot, 0)
	if len(incomingRaw) != 0 {
		incomingBlobs, err := decodeLengthPrefixedBlobListWithLimits(
			incomingRaw, limits,
		)
		if err != nil {
			return sessionsCheckpoint{}, err
		}

		incomingSnapshots = make(
			[]*IncomingSnapshot, 0, len(incomingBlobs),
		)
		for i := range incomingBlobs {
			snapshot, err := decodeIncomingSnapshotWithLimits(
				incomingBlobs[i], limits,
			)
			if err != nil {
				return sessionsCheckpoint{}, err
			}

			incomingSnapshots = append(
				incomingSnapshots, snapshot,
			)
		}
	}

	decodedVersion, err := decodeUint64ToInt(version, "checkpoint version")
	if err != nil {
		return sessionsCheckpoint{}, err
	}

	return sessionsCheckpoint{
		Version:           decodedVersion,
		OutgoingSnapshots: outgoingSnapshots,
		IncomingSnapshots: incomingSnapshots,
	}, nil
}

func encodeOutgoingSessionsCheckpoint(checkpoint outgoingSessionsCheckpoint) (
	[]byte, error) {

	return encodeSessionsCheckpoint(sessionsCheckpoint{
		Version:           checkpoint.Version,
		OutgoingSnapshots: checkpoint.Snapshots,
	})
}

func decodeOutgoingSessionsCheckpoint(raw []byte) (outgoingSessionsCheckpoint,
	error) {

	checkpoint, err := decodeSessionsCheckpoint(raw)
	if err != nil {
		return outgoingSessionsCheckpoint{}, err
	}

	return outgoingSessionsCheckpoint{
		Version:   checkpoint.Version,
		Snapshots: checkpoint.OutgoingSnapshots,
	}, nil
}

func encodeOutgoingSnapshot(snapshot *OutgoingSnapshot) ([]byte, error) {
	if snapshot == nil {
		return nil, fmt.Errorf("snapshot must be provided")
	}

	sessionBytes := sessionIDBytes(snapshot.SessionID)
	phaseBytes := []byte(snapshot.Phase)
	arkPSBT := snapshot.ArkPSBT
	checkpointPSBTs, err := encodeLengthPrefixedBlobList(
		snapshot.CheckpointPSBTs,
	)
	if err != nil {
		return nil, err
	}

	inputSnapshots, err := encodeTransferInputSnapshots(
		snapshot.TransferInputSnapshots,
	)
	if err != nil {
		return nil, err
	}

	retryAfterNanos := uint64(snapshot.RetryAfter)
	failReason := []byte(snapshot.FailReason)
	idempotencyKey := []byte(snapshot.IdempotencyKey)

	version := uint64(snapshot.Version)
	records := []tlv.Record{
		tlv.MakePrimitiveRecord(snapshotVersionRecordType, &version),
		tlv.MakePrimitiveRecord(
			snapshotSessionIDRecordType, &sessionBytes,
		),
		tlv.MakePrimitiveRecord(snapshotPhaseRecordType, &phaseBytes),
		tlv.MakePrimitiveRecord(snapshotArkPSBTRecordType, &arkPSBT),
		tlv.MakePrimitiveRecord(
			snapshotCheckpointPSBTsRecordType, &checkpointPSBTs,
		),
		tlv.MakePrimitiveRecord(
			snapshotTransferInputsRecordType, &inputSnapshots,
		),
		tlv.MakePrimitiveRecord(
			snapshotRetryAfterNanosRecordType, &retryAfterNanos,
		),
		tlv.MakePrimitiveRecord(
			snapshotFailReasonRecordType, &failReason,
		),
		tlv.MakePrimitiveRecord(
			snapshotIdempotencyKeyRecordType, &idempotencyKey,
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

func decodeOutgoingSnapshot(raw []byte) (*OutgoingSnapshot, error) {
	return decodeOutgoingSnapshotWithLimits(raw, ReceiveLimits{})
}

// decodeOutgoingSnapshotWithLimits decodes one outgoing snapshot and applies
// receive limits to nested checkpoint and transfer-input lists.
func decodeOutgoingSnapshotWithLimits(raw []byte,
	limits ReceiveLimits) (*OutgoingSnapshot, error) {

	var (
		version            uint64
		sessionBytes       []byte
		phaseBytes         []byte
		arkPSBT            []byte
		checkpointPSBTsRaw []byte
		inputSnapshotsRaw  []byte
		retryAfterNanos    uint64
		failReasonRaw      []byte
		idempotencyKeyRaw  []byte
	)

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(snapshotVersionRecordType, &version),
		tlv.MakePrimitiveRecord(
			snapshotSessionIDRecordType, &sessionBytes,
		),
		tlv.MakePrimitiveRecord(snapshotPhaseRecordType, &phaseBytes),
		tlv.MakePrimitiveRecord(snapshotArkPSBTRecordType, &arkPSBT),
		tlv.MakePrimitiveRecord(
			snapshotCheckpointPSBTsRecordType, &checkpointPSBTsRaw,
		),
		tlv.MakePrimitiveRecord(
			snapshotTransferInputsRecordType, &inputSnapshotsRaw,
		),
		tlv.MakePrimitiveRecord(
			snapshotRetryAfterNanosRecordType, &retryAfterNanos,
		),
		tlv.MakePrimitiveRecord(
			snapshotFailReasonRecordType, &failReasonRaw,
		),
		tlv.MakePrimitiveRecord(
			snapshotIdempotencyKeyRecordType, &idempotencyKeyRaw,
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

	sessionID, err := parseSessionID(sessionBytes)
	if err != nil {
		return nil, err
	}

	checkpointPSBTs, err := decodeLengthPrefixedBlobListWithLimits(
		checkpointPSBTsRaw, limits,
	)
	if err != nil {
		return nil, err
	}
	if len(checkpointPSBTs) == 0 {
		checkpointPSBTs = nil
	}

	inputSnapshots, err := decodeTransferInputSnapshotsWithLimits(
		inputSnapshotsRaw, limits,
	)
	if err != nil {
		return nil, err
	}
	if len(inputSnapshots) == 0 {
		inputSnapshots = nil
	}

	if len(arkPSBT) == 0 {
		arkPSBT = nil
	}

	decodedVersion, err := decodeUint64ToUint8(version, "snapshot version")
	if err != nil {
		return nil, err
	}

	decodedRetryAfter, err := decodeUint64ToDuration(
		retryAfterNanos, "snapshot retry_after nanos",
	)
	if err != nil {
		return nil, err
	}

	return &OutgoingSnapshot{
		Version:                decodedVersion,
		SessionID:              sessionID,
		Phase:                  OutgoingPhase(phaseBytes),
		ArkPSBT:                arkPSBT,
		CheckpointPSBTs:        checkpointPSBTs,
		TransferInputSnapshots: inputSnapshots,
		RetryAfter:             decodedRetryAfter,
		FailReason:             string(failReasonRaw),
		IdempotencyKey:         string(idempotencyKeyRaw),
	}, nil
}

func encodeOutpoints(outpoints []wire.OutPoint) ([]byte, error) {
	blobs := make([][]byte, 0, len(outpoints))
	for i := range outpoints {
		blobs = append(blobs, outPointBytes(outpoints[i]))
	}

	return encodeLengthPrefixedBlobList(blobs)
}

func decodeUint64ToUint8(value uint64, field string) (uint8, error) {
	if value > math.MaxUint8 {
		return 0, fmt.Errorf("%s overflows uint8: %d", field, value)
	}

	return uint8(value), nil
}

func decodeUint64ToInt(value uint64, field string) (int, error) {
	maxInt := uint64(^uint(0) >> 1)
	if value > maxInt {
		return 0, fmt.Errorf("%s overflows int: %d", field, value)
	}

	return int(value), nil
}

func decodeUint64ToDuration(value uint64, field string) (time.Duration, error) {
	if value > math.MaxInt64 {
		return 0, fmt.Errorf("%s overflows time.Duration: %d", field,
			value)
	}

	return time.Duration(int64(value)), nil
}
