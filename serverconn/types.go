package serverconn

import (
	"context"
	"time"

	"github.com/lightninglabs/darepo-client/baselib/actor"
	mailboxconn "github.com/lightninglabs/darepo-client/mailbox/conn"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/darepo-client/mailbox/rpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

// CorrelationID links a mailbox request to its response.
type CorrelationID = mailboxconn.CorrelationID

// IdempotencyKey deduplicates a semantic operation across retries.
type IdempotencyKey = mailboxconn.IdempotencyKey

// AckState tracks connector ack watermark state for checkpoint persistence.
type AckState = mailboxconn.AckState

// ackStateType is the checkpoint state type used for ack watermark storage.
const ackStateType = mailboxconn.CheckpointStateType

// EnvelopeDispatcher routes an inbound envelope to the correct local actor.
// A nil error means the envelope was durably committed to the target actor's
// mailbox (i.e., DurableActor.Tell returned nil, confirming persistence).
// The dispatcher is a closure configured at wiring time that captures a
// ServiceKey reference for the target actor.
type EnvelopeDispatcher func(
	ctx context.Context, env *mailboxpb.Envelope,
) error

// DurableUnaryRequestBuilder constructs proof-gated unary request payloads
// for durable transport messages that only persist the query spec. The
// returned proto is wrapped into a mailbox KIND_REQUEST envelope after the
// durable serverconn mailbox commit completes.
type DurableUnaryRequestBuilder interface {
	// BuildListOORRecipientEventsByScriptRequest builds the
	// ListOORRecipientEventsByScript unary request for the given taproot
	// output script and monotonic cursor.
	BuildListOORRecipientEventsByScriptRequest(ctx context.Context,
		pkScript []byte, afterEventID uint64, limit uint32) (
		proto.Message, error,
	)

	// BuildListVTXOsByScriptsRequest builds the ListVTXOsByScripts unary
	// request for the given taproot output scripts and cursor.
	BuildListVTXOsByScriptsRequest(ctx context.Context,
		pkScripts [][]byte, afterCursor uint64, limit uint32) (
		proto.Message, error,
	)
}

// DurableUnaryQuery is implemented by transport-native durable query messages
// that persist raw query parameters and need a DurableUnaryRequestBuilder to
// construct the proof-gated proto body at send time. Implementations are
// handled generically in Receive by building a SendUnaryRequest on the fly.
type DurableUnaryQuery interface {
	ServerConnMsg

	// BuildBody constructs the proto request body and returns stable
	// identity bytes for deterministic ID derivation.
	BuildBody(ctx context.Context,
		builder DurableUnaryRequestBuilder) (
		body *anypb.Any, stableBytes []byte, err error,
	)

	// QueryCorrelationID returns the correlation ID for response routing.
	QueryCorrelationID() string

	// QueryMsgID returns the caller-provided msg ID (empty = auto-derive).
	QueryMsgID() string

	// QueryIdempotencyKey returns the caller-provided idempotency key
	// (empty = auto-derive).
	QueryIdempotencyKey() string

	// ServiceMethod returns the mailbox route for this query.
	ServiceMethod() mailboxrpc.ServiceMethod
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

	// DurableUnaryBuilder constructs proof-gated unary request bodies for
	// transport-native durable unary messages such as indexer script-scope
	// queries. When nil, those message types are rejected.
	DurableUnaryBuilder DurableUnaryRequestBuilder

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

	// ResponseWaiterTTL bounds how long a response waiter (or buffered
	// early response) is retained before stale cleanup.
	ResponseWaiterTTL time.Duration

	// HeartbeatInterval is the interval between heartbeat sends to
	// the server. A zero or negative value uses
	// DefaultHeartbeatInterval (30 s). The server's staleness
	// threshold should be at least 2× this interval.
	HeartbeatInterval time.Duration
}

// DefaultConnectorConfig returns a ConnectorConfig with sensible defaults for
// polling and retry behavior. The caller must still set Edge, mailbox IDs,
// and Store. Codec is optional — NewRuntime fills a default.
func DefaultConnectorConfig() ConnectorConfig {
	return ConnectorConfig{
		PullMaxEnvelopes:  50,
		PullWaitTimeout:   5 * time.Second,
		RetryBaseDelay:    200 * time.Millisecond,
		RetryMaxDelay:     30 * time.Second,
		ResponseWaiterTTL: mailboxconn.DefaultResponseWaiterTTL,
	}
}
