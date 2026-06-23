package conn

import (
	"io"

	"github.com/lightningnetwork/lnd/tlv"
)

// CorrelationID links a mailbox request to its response.
type CorrelationID string

// IdempotencyKey deduplicates a semantic operation across retries.
type IdempotencyKey string

// CheckpointStateType is the checkpoint state type name used to persist ack
// watermark state.
const CheckpointStateType = "AckState"

// TLV record type constants for AckState checkpoint serialization.
const (
	pullCursorRecordType          tlv.Type = 1
	dispatchCommittedToRecordType tlv.Type = 2
	ackTargetRecordType           tlv.Type = 3
	ackCommittedToRecordType      tlv.Type = 4
)

// AckState tracks the four cursor variables that govern safe ack progression.
// All fields are monotonic and must not decrease during normal operation.
//
// The state machine enforces:
//
//	ack_committed_to <= dispatch_committed_to
//
// Cursor never advances past non-durable local work. Repeated acks are safe
// and idempotent.
type AckState struct {
	// PullCursor is the cursor for the next Pull call. After a successful
	// ack, PullCursor advances to at least the acked position.
	PullCursor uint64

	// DispatchCommittedTo is the max cursor whose envelopes were durably
	// committed to local processing.
	DispatchCommittedTo uint64

	// AckTarget is the max cursor that should be acked remotely.
	AckTarget uint64

	// AckCommittedTo is the last cursor successfully acked remotely.
	AckCommittedTo uint64
}

// AdvanceDispatch updates state after durable dispatch through nextCursor.
func (s *AckState) AdvanceDispatch(nextCursor uint64) {
	if nextCursor > s.DispatchCommittedTo {
		s.DispatchCommittedTo = nextCursor
	}

	if s.DispatchCommittedTo > s.AckTarget {
		s.AckTarget = s.DispatchCommittedTo
	}
}

// AdvanceAck updates state after a successful AckUpTo call.
func (s *AckState) AdvanceAck() {
	s.AckCommittedTo = s.AckTarget

	// Defensive: under normal operation PullCursor is always advanced
	// to at least AckTarget before AdvanceAck, so this branch is
	// unreachable. It exists as a crash-recovery safety net for the
	// case where a partial checkpoint write leaves AckCommittedTo >
	// PullCursor after restart.
	if s.AckCommittedTo > s.PullCursor {
		s.PullCursor = s.AckCommittedTo
	}
}

// NeedsAck returns true when AckTarget has advanced past AckCommittedTo.
func (s *AckState) NeedsAck() bool {
	return s.AckTarget > s.AckCommittedTo
}

// Encode serializes AckState to a TLV stream.
func (s *AckState) Encode(w io.Writer) error {
	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			pullCursorRecordType, &s.PullCursor,
		),
		tlv.MakePrimitiveRecord(
			dispatchCommittedToRecordType, &s.DispatchCommittedTo,
		),
		tlv.MakePrimitiveRecord(ackTargetRecordType, &s.AckTarget),
		tlv.MakePrimitiveRecord(
			ackCommittedToRecordType, &s.AckCommittedTo,
		),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// Decode deserializes AckState from a TLV stream.
func (s *AckState) Decode(r io.Reader) error {
	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			pullCursorRecordType, &s.PullCursor,
		),
		tlv.MakePrimitiveRecord(
			dispatchCommittedToRecordType, &s.DispatchCommittedTo,
		),
		tlv.MakePrimitiveRecord(ackTargetRecordType, &s.AckTarget),
		tlv.MakePrimitiveRecord(
			ackCommittedToRecordType, &s.AckCommittedTo,
		),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return err
	}

	// Bound the untrusted payload before decode: AckState is persisted
	// in the durable checkpoint store and replayed from disk, so a
	// crafted record length must not drive an unbounded make() in the
	// tlv library.
	safe, err := safeTLVReader(r)
	if err != nil {
		return err
	}

	_, err = stream.DecodeWithParsedTypes(safe)

	return err
}
