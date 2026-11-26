package oor

import (
	"fmt"
	"math"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/tlvutil"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
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

	// snapshotDispatchRequestDataRecordType retains the normalized caller
	// proof until the first submit-capable checkpoint.
	snapshotDispatchRequestDataRecordType tlv.Type = 25

	// snapshotFailedPrePONRRecordType stores whether a Failed snapshot
	// failed before the point of no return (snapshot version 6+). A
	// pre-v6 snapshot omits it, so it decodes to false.
	snapshotFailedPrePONRRecordType tlv.Type = 27

	// snapshotRecipientOutputsRecordType stores canonical submit recipient
	// outputs (including Taproot Asset metadata) for restart-safe retries
	// (snapshot version 7+).
	snapshotRecipientOutputsRecordType tlv.Type = 29

	// snapshotTaprootAssetTransferRecordType stores the encoded sealed
	// TaprootAssetTransfer container for asset-bearing transfers (snapshot
	// version 7+).
	snapshotTaprootAssetTransferRecordType tlv.Type = 31
)

// encodeOutgoingSnapshot serializes outgoing transfer state, including retry
// and admission evidence.
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
	dispatchRequestData := snapshot.DispatchRequestData
	var failedPrePONR uint8
	if snapshot.FailedPrePONR {
		failedPrePONR = 1
	}

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
		tlv.MakePrimitiveRecord(
			snapshotDispatchRequestDataRecordType,
			&dispatchRequestData,
		),
		tlv.MakePrimitiveRecord(
			snapshotFailedPrePONRRecordType, &failedPrePONR,
		),
	}

	if len(snapshot.RecipientOutputs) > 0 {
		recipientPayloads := make(
			[]recipientPayload, 0, len(snapshot.RecipientOutputs),
		)
		for i := range snapshot.RecipientOutputs {
			rec := snapshot.RecipientOutputs[i]
			recipientPayloads = append(
				recipientPayloads, recipientPayload{
					PkScript: rec.PkScript,
					ValueSat: int64(rec.Value),
					VTXOPolicyTemplate: rec.
						VTXOPolicyTemplate,
					Asset: rec.Asset.Clone(),
				},
			)
		}
		recipientsRaw, err := encodeRecipientPayloads(recipientPayloads)
		if err != nil {
			return nil, err
		}
		records = append(
			records, tlv.MakePrimitiveRecord(
				snapshotRecipientOutputsRecordType,
				&recipientsRaw,
			),
		)
	}

	if len(snapshot.TaprootAssetTransfer) > 0 {
		assetTransfer := snapshot.TaprootAssetTransfer
		records = append(
			records, tlv.MakePrimitiveRecord(
				snapshotTaprootAssetTransferRecordType,
				&assetTransfer,
			),
		)
	}

	encoded, err := tlvutil.EncodeRecordsToBytes(records...)
	if err != nil {
		return nil, err
	}
	if len(encoded) > maxOORMessageSize {
		return nil, fmt.Errorf("encoded outgoing snapshot %d bytes "+
			"exceeds max %d", len(encoded), maxOORMessageSize)
	}

	return encoded, nil
}

func decodeOutgoingSnapshot(raw []byte) (*OutgoingSnapshot, error) {
	return decodeOutgoingSnapshotWithLimits(raw, ReceiveLimits{})
}

// decodeOutgoingSnapshotWithLimits decodes one outgoing snapshot and applies
// receive limits to nested checkpoint and transfer-input lists.
func decodeOutgoingSnapshotWithLimits(raw []byte,
	limits ReceiveLimits) (*OutgoingSnapshot, error) {

	var (
		version             uint64
		sessionBytes        []byte
		phaseBytes          []byte
		arkPSBT             []byte
		checkpointPSBTsRaw  []byte
		inputSnapshotsRaw   []byte
		retryAfterNanos     uint64
		failReasonRaw       []byte
		idempotencyKeyRaw   []byte
		firstRejectNanos    uint64
		dispatchRequestData []byte
		failedPrePONR       uint8
		recipientsRaw       []byte
		assetTransferRaw    []byte
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
		tlv.MakePrimitiveRecord(
			snapshotDispatchRequestDataRecordType,
			&dispatchRequestData,
		),
		tlv.MakePrimitiveRecord(
			snapshotFailedPrePONRRecordType, &failedPrePONR,
		),
		tlv.MakePrimitiveRecord(
			snapshotRecipientOutputsRecordType, &recipientsRaw,
		),
		tlv.MakePrimitiveRecord(
			snapshotTaprootAssetTransferRecordType,
			&assetTransferRaw,
		),
	}

	if _, err := decodeBoundedRecords(
		raw, records...,
	); err != nil {
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
	if len(dispatchRequestData) == 0 {
		dispatchRequestData = nil
	}
	if len(assetTransferRaw) == 0 {
		assetTransferRaw = nil
	}

	var recipientOutputs []oortx.RecipientOutput
	if len(recipientsRaw) > 0 {
		decodedRecipients, err := decodeRecipientPayloadsWithLimits(
			recipientsRaw, limits,
		)
		if err != nil {
			return nil, err
		}
		if len(decodedRecipients) > 0 {
			recipientOutputs = make(
				[]oortx.RecipientOutput, 0,
				len(decodedRecipients),
			)
			for i := range decodedRecipients {
				rec := decodedRecipients[i]
				recipientOutputs = append(
					recipientOutputs,
					oortx.RecipientOutput{
						PkScript: rec.PkScript,
						Value: btcutil.Amount(
							rec.ValueSat,
						),
						VTXOPolicyTemplate: rec.
							VTXOPolicyTemplate,
						Asset: rec.Asset.Clone(),
					},
				)
			}
		}
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
		RecipientOutputs:       recipientOutputs,
		TaprootAssetTransfer:   assetTransferRaw,
		RetryAfter:             decodedRetryAfter,
		FailReason:             string(failReasonRaw),
		IdempotencyKey:         string(idempotencyKeyRaw),
		FirstRejectUnixNanos:   decodedFirstReject,
		DispatchRequestData:    dispatchRequestData,
		FailedPrePONR:          failedPrePONR == 1,
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
