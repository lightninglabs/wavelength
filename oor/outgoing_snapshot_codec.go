package oor

import (
	"bytes"
	"fmt"
	"math"
	"time"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/tlv"
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

	// snapshotFirstRejectNanosRecordType stores the Unix-nanosecond start
	// of the bounded transient submit-reject retry window (snapshot version
	// 5+). A pre-v5 snapshot omits this record, so it decodes to 0 (a fresh
	// window) rather than failing.
	snapshotFirstRejectNanosRecordType tlv.Type = 23
)

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
	firstRejectNanos := uint64(snapshot.FirstRejectUnixNanos)

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
		tlv.MakePrimitiveRecord(
			snapshotFirstRejectNanosRecordType, &firstRejectNanos,
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
		firstRejectNanos   uint64
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
		tlv.MakePrimitiveRecord(
			snapshotFirstRejectNanosRecordType, &firstRejectNanos,
		),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, err
	}

	if _, err := decodeBoundedStream(stream, raw); err != nil {
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

	decodedFirstReject, err := uint64ToInt64(
		firstRejectNanos, "snapshot first_reject nanos",
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
		FirstRejectUnixNanos:   decodedFirstReject,
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

func decodeUint64ToDuration(value uint64, field string) (time.Duration, error) {
	if value > math.MaxInt64 {
		return 0, fmt.Errorf("%s overflows time.Duration: %d", field,
			value)
	}

	return time.Duration(int64(value)), nil
}
