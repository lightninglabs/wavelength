package oor

import (
	"bytes"
	"math"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

func TestOutgoingSnapshotTLVRoundTrip(t *testing.T) {
	t.Parallel()

	snapshot := &OutgoingSnapshot{
		Version:   4,
		SessionID: SessionID(chainhash.Hash{1, 2, 3}),
		Phase:     OutgoingPhaseSubmitSent,
		ArkPSBT: []byte{
			1,
			2,
			3,
			4,
		},
		CheckpointPSBTs: [][]byte{
			{
				5,
				6,
			},
			{
				7,
				8,
			},
		},
		TransferInputSnapshots: []*TransferInputSnapshot{
			{
				Outpoint: wire.OutPoint{
					Hash: chainhash.Hash{
						9,
						10,
					},
					Index: 11,
				},
				AmountSat:       123,
				ClientKeyFamily: 1,
				ClientKeyIndex:  2,
				ClientPubKey: []byte{
					2,
					3,
					4,
				},
				OperatorPubKey: []byte{
					2,
					5,
					6,
				},
				ExitDelay: 42,
				OwnerLeafScript: []byte{
					0x51,
				},
			},
		},
		RetryAfter:     3 * time.Second,
		FailReason:     "retry later",
		IdempotencyKey: "funding-key-1",
	}

	raw, err := encodeOutgoingSnapshot(snapshot)
	require.NoError(t, err)

	decoded, err := decodeOutgoingSnapshot(raw)
	require.NoError(t, err)
	require.Equal(t, snapshot, decoded)
}

// TestOutgoingSnapshotFirstRejectRoundTrip asserts a version-5 submit-sent
// snapshot carrying a non-zero FirstRejectUnixNanos serializes and restores it
// so the bounded transient submit-reject retry window survives a restart.
func TestOutgoingSnapshotFirstRejectRoundTrip(t *testing.T) {
	t.Parallel()

	snapshot := &OutgoingSnapshot{
		Version:   5,
		SessionID: SessionID(chainhash.Hash{1, 2, 3}),
		Phase:     OutgoingPhaseSubmitSent,
		ArkPSBT: []byte{
			1,
			2,
			3,
			4,
		},
		FirstRejectUnixNanos: 1_700_000_000_123_456_789,
	}

	raw, err := encodeOutgoingSnapshot(snapshot)
	require.NoError(t, err)

	decoded, err := decodeOutgoingSnapshot(raw)
	require.NoError(t, err)
	require.Equal(
		t, int64(1_700_000_000_123_456_789),
		decoded.FirstRejectUnixNanos,
	)
	require.Equal(t, snapshot, decoded)
}

// TestOutgoingSnapshotV4RestoresZeroFirstReject asserts a legacy version-4
// snapshot (which lacks the FirstRejectUnixNanos record) decodes without error
// and restores FirstRejectUnixNanos to 0 (a fresh retry window), preserving
// backward compatibility.
func TestOutgoingSnapshotV4RestoresZeroFirstReject(t *testing.T) {
	t.Parallel()

	raw, err := encodeSnapshotRawForDecodeTest(4, 0)
	require.NoError(t, err)

	decoded, err := decodeOutgoingSnapshot(raw)
	require.NoError(t, err)
	require.Equal(t, uint8(4), decoded.Version)
	require.Zero(t, decoded.FirstRejectUnixNanos)
}

func TestRestoreSnapshotPayloadTLVRoundTrip(t *testing.T) {
	t.Parallel()

	snapshot := &OutgoingSnapshot{
		Version:   3,
		SessionID: SessionID(chainhash.Hash{21, 22, 23}),
		Phase:     OutgoingPhaseCompleted,
	}

	raw, err := encodeRestoreSnapshotPayload(snapshot)
	require.NoError(t, err)

	decoded, err := decodeRestoreSnapshotPayload(raw)
	require.NoError(t, err)
	require.Equal(t, snapshot, decoded)
}

func TestDecodeOutgoingSnapshotRejectsVersionOverflow(t *testing.T) {
	t.Parallel()

	raw, err := encodeSnapshotRawForDecodeTest(
		uint64(math.MaxUint8)+1, 0,
	)
	require.NoError(t, err)

	_, err = decodeOutgoingSnapshot(raw)
	require.ErrorContains(t, err, "snapshot version overflows uint8")
}

func TestDecodeOutgoingSnapshotRejectsRetryAfterOverflow(t *testing.T) {
	t.Parallel()

	raw, err := encodeSnapshotRawForDecodeTest(
		2, uint64(math.MaxInt64)+1,
	)
	require.NoError(t, err)

	_, err = decodeOutgoingSnapshot(raw)
	require.ErrorContains(
		t, err, "snapshot retry_after nanos overflows time.Duration",
	)
}

func encodeSnapshotRawForDecodeTest(version uint64,
	retryAfterNanos uint64) ([]byte, error) {

	sessionBytes := sessionIDBytes(SessionID(chainhash.Hash{1}))
	phaseBytes := []byte(OutgoingPhaseCompleted)
	arkPSBT := []byte(nil)

	checkpointPSBTsRaw, err := encodeLengthPrefixedBlobList(nil)
	if err != nil {
		return nil, err
	}

	inputSnapshotsRaw, err := encodeTransferInputSnapshots(nil)
	if err != nil {
		return nil, err
	}

	failReasonRaw := []byte(nil)

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
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, err
	}

	var raw bytes.Buffer
	if err := stream.Encode(&raw); err != nil {
		return nil, err
	}

	return raw.Bytes(), nil
}
