package serverconn

import (
	"context"
	"encoding/hex"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	mailboxconn "github.com/lightninglabs/darepo-client/mailbox/conn"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/darepo-client/mailbox/rpc"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

// PubKeyMailboxID returns the canonical mailbox identifier for a
// public key: the hex-encoded SEC compressed serialization. Both
// server and client derive their mailbox IDs from their respective
// identity keys using this function, ensuring the mailbox namespace
// is cryptographically bound to key material. Panics if key is nil.
func PubKeyMailboxID(key *btcec.PublicKey) string {
	if key == nil {
		panic("PubKeyMailboxID called with nil public key")
	}

	return hex.EncodeToString(key.SerializeCompressed())
}

// CompoundMailboxID builds a per-client mailbox identifier by
// joining the server (operator) and client pubkey-derived IDs
// with a colon separator. Both the client and server derive this
// independently so the wire-level Pull/Send addresses match, while
// the bridge's uniqueness constraint on LocalMailboxID is satisfied.
func CompoundMailboxID(serverID, clientID string) string {
	return serverID + ":" + clientID
}

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
		pkScript []byte, afterEventID uint64,
		limit uint32) (proto.Message, error)

	// BuildListVTXOsByScriptsRequest builds the ListVTXOsByScripts unary
	// request for the given taproot output scripts and cursor.
	BuildListVTXOsByScriptsRequest(ctx context.Context, pkScripts [][]byte,
		afterCursor []byte, limit uint32) (proto.Message, error)
}

// DurableUnaryQuery is implemented by transport-native durable query messages
// that persist raw query parameters and need a DurableUnaryRequestBuilder to
// construct the proof-gated proto body at send time. Implementations are
// handled generically in Receive by building a SendUnaryRequest on the fly.
type DurableUnaryQuery interface {
	ServerConnMsg

	// BuildBody constructs the proto request body and returns stable
	// identity bytes for deterministic ID derivation.
	BuildBody(ctx context.Context, builder DurableUnaryRequestBuilder) (
		body *anypb.Any, stableBytes []byte, err error)

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

	// Log is an optional logger for this connector instance.
	Log fn.Option[btclog.Logger]

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

	// AuthSignature is the Schnorr signature proving the client
	// holds the private key for its pubkey-derived mailbox ID.
	// When non-nil, it is serialized as hex and included as the
	// x-mailbox-auth-sig header on every outbound envelope. The
	// server verifies this signature during client registration.
	AuthSignature *schnorr.Signature

	// authSigHex caches the hex-encoded auth signature string,
	// computed once by InitAuthHeader to avoid per-envelope
	// serialization.
	authSigHex string

	// authHeaderCache holds the singleton auth-only header map
	// for the common case where callers provide no extra headers.
	authHeaderCache map[string]string
}

// InitAuthHeader pre-computes the cached auth header state from
// AuthSignature. Must be called after AuthSignature is set and
// before the first mergeAuthHeaders call.
func (c *ConnectorConfig) InitAuthHeader() {
	if c.AuthSignature == nil {
		return
	}

	c.authSigHex = hex.EncodeToString(c.AuthSignature.Serialize())
	c.authHeaderCache = map[string]string{
		AuthHeaderKey: c.authSigHex,
	}
}

// mergeAuthHeaders returns a new header map containing both src
// headers and the auth signature header. If AuthSignature is nil,
// src is returned unchanged. The auth signature header always takes
// precedence over any caller-provided header with the same key to
// prevent accidental or malicious signature replacement.
func (c *ConnectorConfig) mergeAuthHeaders(
	src map[string]string) map[string]string {

	if c.authSigHex == "" {
		return src
	}

	// Fast path: no caller headers, return the cached singleton.
	if len(src) == 0 {
		return c.authHeaderCache
	}

	merged := make(map[string]string, len(src)+1)

	// Copy caller-provided headers first.
	for k, v := range src {
		merged[k] = v
	}

	// Auth signature always wins over caller-provided headers.
	merged[AuthHeaderKey] = c.authSigHex

	return merged
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
