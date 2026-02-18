package serverconn

import (
	"context"
	"io"
	"time"

	"github.com/lightninglabs/darepo-client/baselib/actor"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/darepo-client/mailbox/rpc"
	"github.com/lightningnetwork/lnd/tlv"
)

// CorrelationID is an opaque identifier linking a mailbox request to its
// response. Using a named type prevents accidental string swaps with other
// identifiers.
type CorrelationID string

// IdempotencyKey is a stable key for deduplicating semantic operations across
// retries. Two sends with the same idempotency key are treated as the same
// logical operation by the remote mailbox edge.
type IdempotencyKey string

// ackStateType is the checkpoint state type used when persisting the ack
// watermark to the delivery store.
const ackStateType = "AckState"

// TLV record type constants for AckState checkpoint serialization.
const (
	pullCursorRecordType          tlv.Type = 1
	dispatchCommittedToRecordType tlv.Type = 2
	ackTargetRecordType           tlv.Type = 3
	ackCommittedToRecordType      tlv.Type = 4
)

// AckState tracks the four cursor variables that govern safe ack progression.
// All fields are monotonic — they never decrease during normal operation.
//
// The state machine enforces the invariant:
//
//	ack_committed_to <= dispatch_committed_to
//
// Cursor never advances past non-durable local work. Repeated acks are safe
// and idempotent.
type AckState struct {
	// PullCursor is the cursor for the next Pull call. After a successful
	// ack, this advances to at least the acked position.
	PullCursor uint64

	// DispatchCommittedTo is the max cursor whose envelopes have been
	// durably committed to local actor mailboxes via Tell.
	DispatchCommittedTo uint64

	// AckTarget is the max cursor that should be acked remotely. This is
	// always >= DispatchCommittedTo.
	AckTarget uint64

	// AckCommittedTo is the last cursor successfully acked to the remote
	// mailbox edge.
	AckCommittedTo uint64
}

// AdvanceDispatch updates the state after a successful durable dispatch
// through nextCursor. The ack target is advanced to match the dispatch
// frontier.
func (s *AckState) AdvanceDispatch(nextCursor uint64) {
	if nextCursor > s.DispatchCommittedTo {
		s.DispatchCommittedTo = nextCursor
	}

	if s.DispatchCommittedTo > s.AckTarget {
		s.AckTarget = s.DispatchCommittedTo
	}
}

// AdvanceAck updates the state after a successful AckUpTo call. The pull
// cursor advances to at least the acked position so that subsequent pulls
// do not re-fetch already-acked envelopes.
func (s *AckState) AdvanceAck() {
	s.AckCommittedTo = s.AckTarget

	if s.AckCommittedTo > s.PullCursor {
		s.PullCursor = s.AckCommittedTo
	}
}

// NeedsAck returns true when there is an un-acked committed dispatch. This
// means AckTarget has advanced past AckCommittedTo and a remote AckUpTo call
// is needed.
func (s *AckState) NeedsAck() bool {
	return s.AckTarget > s.AckCommittedTo
}

// Encode serializes the AckState to the provided writer as a TLV stream.
func (s *AckState) Encode(w io.Writer) error {
	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			pullCursorRecordType, &s.PullCursor,
		),
		tlv.MakePrimitiveRecord(
			dispatchCommittedToRecordType,
			&s.DispatchCommittedTo,
		),
		tlv.MakePrimitiveRecord(
			ackTargetRecordType, &s.AckTarget,
		),
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

// Decode deserializes the AckState from the provided reader.
func (s *AckState) Decode(r io.Reader) error {
	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			pullCursorRecordType, &s.PullCursor,
		),
		tlv.MakePrimitiveRecord(
			dispatchCommittedToRecordType,
			&s.DispatchCommittedTo,
		),
		tlv.MakePrimitiveRecord(
			ackTargetRecordType, &s.AckTarget,
		),
		tlv.MakePrimitiveRecord(
			ackCommittedToRecordType, &s.AckCommittedTo,
		),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return err
	}

	_, err = stream.DecodeWithParsedTypes(r)

	return err
}

// EnvelopeDispatcher routes an inbound envelope to the correct local actor.
// A nil error means the envelope was durably committed to the target actor's
// mailbox (i.e., DurableActor.Tell returned nil, confirming persistence).
// The dispatcher is a closure configured at wiring time that captures a
// ServiceKey reference for the target actor.
type EnvelopeDispatcher func(
	ctx context.Context, env *mailboxpb.Envelope,
) error

// ResponseWaiter is registered by unary facade callers so the ingress loop
// can deliver KIND_RESPONSE envelopes without actor dispatch. The channel
// has buffer size 1 to prevent the ingress loop from blocking.
type ResponseWaiter struct {
	// Ch receives the response envelope from the ingress loop.
	Ch chan *mailboxpb.Envelope

	// Created records when the waiter was registered, for diagnostics
	// and stale waiter cleanup.
	Created time.Time
}

// ConnectorConfig holds all dependencies and tuning knobs for the server
// connection actor. The connector is the single boundary for all mailbox
// traffic between the client and the remote server.
type ConnectorConfig struct {
	// Edge is the gRPC client for the remote mailbox edge service,
	// providing Send, Pull, and AckUpTo operations.
	Edge mailboxpb.MailboxServiceClient

	// LocalMailboxID is this client's mailbox identifier. Inbound
	// envelopes are pulled from this mailbox, and it is set as the
	// sender on outbound envelopes.
	LocalMailboxID string

	// RemoteMailboxID is the remote server's mailbox identifier. Outbound
	// envelopes are addressed to this mailbox.
	RemoteMailboxID string

	// ProtocolVersion is the protocol version stamped on outbound
	// envelopes.
	ProtocolVersion uint32

	// Dispatchers maps (service, method) pairs to envelope dispatchers.
	// The ingress loop uses this table to route KIND_REQUEST and
	// KIND_EVENT envelopes to the correct local actor via ServiceKey.
	Dispatchers map[mailboxrpc.ServiceMethod]EnvelopeDispatcher

	// Store is the delivery store used by both the durable actor runtime
	// (for inbox persistence) and checkpoint persistence (for ack
	// watermark state). This is the single durability source of truth.
	Store actor.DeliveryStore

	// Codec handles TLV serialization of ServerConnMsg types for the
	// durable actor mailbox.
	Codec *actor.MessageCodec

	// PullMaxEnvelopes bounds the number of envelopes returned per Pull
	// call.
	PullMaxEnvelopes uint32

	// PullWaitTimeout is the long-poll timeout for Pull calls. The remote
	// edge will hold the connection open for this duration before
	// returning an empty response.
	PullWaitTimeout time.Duration

	// RetryBaseDelay is the base delay for exponential backoff on
	// transient failures (pull, ack, dispatch).
	RetryBaseDelay time.Duration

	// RetryMaxDelay caps the exponential backoff delay.
	RetryMaxDelay time.Duration
}

// DefaultConnectorConfig returns a ConnectorConfig with sensible defaults for
// polling and retry behavior. The caller must still set Edge, mailbox IDs,
// and Store. Codec is optional — NewRuntime fills a default.
func DefaultConnectorConfig() ConnectorConfig {
	return ConnectorConfig{
		PullMaxEnvelopes: 50,
		PullWaitTimeout:  5 * time.Second,
		RetryBaseDelay:   200 * time.Millisecond,
		RetryMaxDelay:    30 * time.Second,
	}
}
