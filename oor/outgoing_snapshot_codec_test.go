package oor

import (
	"bytes"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
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
		DispatchRequestData: []byte{
			1, 2, 3,
		},
		RecipientOutputs: []oortx.RecipientOutput{{
			PkScript: []byte{
				0x51,
				0x20,
			},
			Value: 321,
			VTXOPolicyTemplate: []byte{
				0x01, 0x02,
			},
		}},
		FailedPrePONR: true,
	}

	raw, err := encodeOutgoingSnapshot(snapshot)
	require.NoError(t, err)

	decoded, err := decodeOutgoingSnapshot(raw)
	require.NoError(t, err)
	require.Equal(t, snapshot, decoded)
}

// TestOutgoingSnapshotFailureProofVersions preserves the two historical
// meanings of record 27 without confusing recipient bytes with abort proof.
func TestOutgoingSnapshotFailureProofVersions(t *testing.T) {
	t.Parallel()

	recipients, err := encodeRecipientPayloads([]recipientPayload{{
		PkScript: []byte{0x51}, ValueSat: 123,
	}})
	require.NoError(t, err)

	for _, version := range []uint64{6, 7, 8} {
		t.Run(fmt.Sprintf("version-%d", version), func(t *testing.T) {
			t.Parallel()

			session := sessionIDBytes(SessionID(chainhash.Hash{1}))
			phase := []byte(OutgoingPhaseFailed)
			marker := []byte{1}
			var legacyFailure uint8
			var currentRecipients []byte
			checkpoints, err := encodeLengthPrefixedBlobList(nil)
			require.NoError(t, err)
			inputs, err := encodeTransferInputSnapshots(nil)
			require.NoError(t, err)
			if version == 7 {
				marker = recipients
				legacyFailure = 1
			} else if version == 8 {
				currentRecipients = recipients
			}
			stream, err := tlv.NewStream(
				tlv.MakePrimitiveRecord(1, &version),
				tlv.MakePrimitiveRecord(3, &session),
				tlv.MakePrimitiveRecord(5, &phase),
				tlv.MakePrimitiveRecord(9, &checkpoints),
				tlv.MakePrimitiveRecord(11, &inputs),
				tlv.MakePrimitiveRecord(27, &marker),
				tlv.MakePrimitiveRecord(29, &legacyFailure),
				tlv.MakePrimitiveRecord(31, &currentRecipients),
			)
			require.NoError(t, err)
			var raw bytes.Buffer
			require.NoError(t, stream.Encode(&raw))
			snapshot, err := decodeOutgoingSnapshot(raw.Bytes())
			require.NoError(t, err)
			require.True(t, snapshot.FailedPrePONR)
			if version >= 7 {
				require.Len(t, snapshot.RecipientOutputs, 1)
				amount := snapshot.RecipientOutputs[0].Value
				require.EqualValues(t, 123, amount)
			}
			state, err := OutgoingStateFromSnapshot(snapshot)
			require.NoError(t, err)
			failed, ok := state.(*Failed)
			require.True(t, ok)
			require.True(t, failed.PrePONR)
		})
	}
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

	raw, err := encodeSnapshotRawForDecodeTest(4, 0, OutgoingPhaseCompleted)
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
		uint64(math.MaxUint8)+1, 0, OutgoingPhaseCompleted,
	)
	require.NoError(t, err)

	_, err = decodeOutgoingSnapshot(raw)
	require.ErrorContains(t, err, "snapshot version overflows uint8")
}

// TestDecodeOutgoingSnapshotRejectsFutureVersion verifies this binary fails
// closed instead of interpreting fields from an unknown snapshot schema.
func TestDecodeOutgoingSnapshotRejectsFutureVersion(t *testing.T) {
	t.Parallel()

	raw, err := encodeSnapshotRawForDecodeTest(
		uint64(outgoingSnapshotVersion)+1, 0, OutgoingPhaseCompleted,
	)
	require.NoError(t, err)

	_, err = decodeOutgoingSnapshot(raw)
	require.ErrorContains(t, err, "unknown outgoing snapshot version")
}

func TestDecodeOutgoingSnapshotRejectsRetryAfterOverflow(t *testing.T) {
	t.Parallel()

	raw, err := encodeSnapshotRawForDecodeTest(
		2, uint64(math.MaxInt64)+1, OutgoingPhaseCompleted,
	)
	require.NoError(t, err)

	_, err = decodeOutgoingSnapshot(raw)
	require.ErrorContains(
		t, err, "snapshot retry_after nanos overflows time.Duration",
	)
}

func encodeSnapshotRawForDecodeTest(version uint64, retryAfterNanos uint64,
	phase OutgoingPhase) ([]byte, error) {

	sessionBytes := sessionIDBytes(SessionID(chainhash.Hash{1}))
	phaseBytes := []byte(phase)
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
