package clientconn

import (
	"context"
	"time"

	"github.com/lightninglabs/darepo-client/baselib/actor"
	mailboxconn "github.com/lightninglabs/darepo-client/mailbox/conn"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/darepo-client/mailbox/rpc"
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

// PerClientConfig holds all dependencies and tuning knobs for a single
// client's connector. Each registered client gets its own config with
// dedicated mailbox IDs, edge connection, and dispatch table. This mirrors
// serverconn.ConnectorConfig but is instantiated per-client rather than
// once globally.
type PerClientConfig struct {
	// Edge is the gRPC client for the remote mailbox edge service,
	// providing Send, Pull, and AckUpTo operations.
	Edge mailboxpb.MailboxServiceClient

	// LocalMailboxID is the server's per-client mailbox identifier.
	// Inbound envelopes from this client are pulled from this mailbox,
	// and it is set as the sender on outbound envelopes.
	LocalMailboxID string

	// RemoteMailboxID is the client's mailbox identifier. Outbound
	// envelopes (server→client events and RPC requests) are addressed
	// to this mailbox.
	RemoteMailboxID string

	// ProtocolVersion is the protocol version stamped on outbound
	// envelopes.
	ProtocolVersion uint32

	// Dispatchers maps (service, method) pairs to envelope dispatchers.
	// The ingress loop uses this table to route KIND_REQUEST and
	// KIND_EVENT envelopes from the client to the correct server-side
	// actor via ServiceKey.
	Dispatchers map[mailboxrpc.ServiceMethod]EnvelopeDispatcher

	// Store is the delivery store used by both the durable actor runtime
	// (for inbox persistence) and checkpoint persistence (for ack
	// watermark state). This is the single durability source of truth.
	Store actor.DeliveryStore

	// Codec handles TLV serialization of connector message types for the
	// durable actor mailbox.
	Codec *actor.MessageCodec

	// PullMaxEnvelopes bounds the number of envelopes returned per Pull
	// call.
	PullMaxEnvelopes uint32

	// PullWaitTimeout is the long-poll timeout for Pull calls. The
	// remote edge will hold the connection open for this duration before
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
}

// DefaultPerClientConfig returns a PerClientConfig with sensible defaults for
// polling and retry behavior. The caller must still set Edge, mailbox IDs,
// and Store. Codec is optional — NewClientRuntime fills a default.
func DefaultPerClientConfig() PerClientConfig {
	return PerClientConfig{
		PullMaxEnvelopes:  50,
		PullWaitTimeout:   5 * time.Second,
		RetryBaseDelay:    200 * time.Millisecond,
		RetryMaxDelay:     30 * time.Second,
		ResponseWaiterTTL: mailboxconn.DefaultResponseWaiterTTL,
	}
}
