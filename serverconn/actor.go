package serverconn

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	mailboxconn "github.com/lightninglabs/wavelength/mailbox/conn"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/tlv"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

// TLV type constants for server connection messages. These are stable
// identifiers used for message serialization and dispatch within the durable
// actor mailbox.
const (
	// SendClientEventRequestMsgType is the TLV type for outbound FSM
	// events from the round actor to the server.
	SendClientEventRequestMsgType tlv.Type = 2000

	// SendRPCRequestMsgType is the TLV type for outbound unary RPC
	// envelopes from the unary facade.
	SendRPCRequestMsgType tlv.Type = 2001

	// SendUnaryRequestMsgType is the TLV type for durable correlated unary
	// requests where the connector constructs the mailbox envelope.
	SendUnaryRequestMsgType tlv.Type = 2002
)

// defaultSendEventTimeout is the timeout for outbound gRPC Edge.Send calls.
const defaultSendEventTimeout = 30 * time.Second

// TLV record type aliases for RecordT-style message field serialization.
type (
	protoPayloadRecordTLV   = tlv.TlvType1
	envelopeRecordTLV       = tlv.TlvType2
	msgIDRecordTLV          = tlv.TlvType3
	idempotencyRecordTLV    = tlv.TlvType4
	rpcServiceRecordTLV     = tlv.TlvType5
	rpcMethodRecordTLV      = tlv.TlvType6
	rpcCorrelationRecordTLV = tlv.TlvType7
	correlationKeyRecordTLV = tlv.TlvType8
)

// ServerMessage is an interface that client FSM outbox messages must implement
// to be sent to the server. This allows conversion to proto messages without
// creating import cycles.
//
// Every ServerMessage must declare its mailbox routing metadata via
// ServiceMethod so the operator's clientconn ingress loop can dispatch
// the envelope to the correct handler.
type ServerMessage interface {
	// ToProto converts the message to a protobuf message that can be
	// sent over gRPC. An error is returned if serialization of any
	// embedded field (e.g. signatures, transactions) fails.
	ToProto() fn.Result[proto.Message]

	// ServiceMethod returns the fully-qualified protobuf service and
	// method names used for mailbox envelope routing (e.g.
	// Service: "round.v1.RoundService", Method: "JoinRound").
	ServiceMethod() mailboxrpc.ServiceMethod
}

// InboundServerMessage is implemented by actor messages that arrive from the
// server via the mailbox ingress loop. FromProto mirrors the ToProto method
// on ServerMessage, completing the bidirectional proto<->actor message
// conversion pair.
//
// Callers that implement this interface on their actor message types can use
// the NewEventRoute helper to avoid writing explicit Adapt functions.
type InboundServerMessage interface {
	// FromProto populates the receiver from a server-pushed proto message.
	// It is called by the EventRouter dispatch closure after the envelope
	// body has been unmarshaled into the expected proto type. Return an
	// error to reject events whose proto fields cannot be converted.
	FromProto(proto.Message) error
}

// rawServerMessage wraps a protobuf Any for reconstructing a ServerMessage
// after TLV deserialization. The original concrete type is recovered using
// the global protobuf type registry via anypb.UnmarshalNew.
type rawServerMessage struct {
	anyMsg *anypb.Any
}

// ServiceMethod returns a zero-value ServiceMethod. After TLV decoding the
// routing metadata lives on the enclosing SendClientEventRequest rather than
// on the raw message wrapper.
func (m *rawServerMessage) ServiceMethod() mailboxrpc.ServiceMethod {
	return mailboxrpc.ServiceMethod{}
}

// ToProto reconstructs the original proto message from the stored Any
// wrapper.
func (m *rawServerMessage) ToProto() fn.Result[proto.Message] {
	msg, err := m.anyMsg.UnmarshalNew()
	if err != nil {
		return fn.Err[proto.Message](
			fmt.Errorf(
				"unmarshal Any type %q: %w",
				m.anyMsg.GetTypeUrl(), err,
			),
		)
	}

	return fn.Ok[proto.Message](msg)
}

// ServerConnMsg is the sealed interface for messages that can be sent to the
// ServerConnectionActor. These are typically FSM outbox messages from the
// client that need to be relayed to the server. The interface extends
// TLVMessage for durable actor mailbox persistence.
type ServerConnMsg interface {
	actor.TLVMessage

	serverConnMsgSealed()
}

// ServerConnResp is the sealed interface for responses from the
// ServerConnectionActor.
type ServerConnResp interface {
	actor.Message

	serverConnRespSealed()
}

// SendClientEventRequest wraps a client FSM outbox message and requests it be
// sent to the server. The actor will convert it to the appropriate proto
// message and send via the mailbox edge.
type SendClientEventRequest struct {
	actor.BaseMessage

	// Message is the client FSM outbox message to send to the server.
	// It must implement the ServerMessage interface which provides the
	// ToProto() method for conversion to protobuf.
	Message ServerMessage

	// MsgID uniquely identifies this send attempt. When this request is
	// durably persisted and later retried, the same MsgID is reused.
	MsgID string

	// IdempotencyKey identifies the semantic operation for remote dedupe.
	// Retries of the same persisted request must reuse this key.
	IdempotencyKey string

	// Service is the fully-qualified protobuf service name for mailbox
	// routing (e.g. "round.v1.RoundService"). Populated from
	// ServerMessage.ServiceMethod() at send time, persisted in TLV for
	// crash-safe replay.
	Service string

	// Method is the RPC method name for mailbox routing (e.g.
	// "JoinRound"). Populated from ServerMessage.ServiceMethod() at
	// send time, persisted in TLV for crash-safe replay.
	Method string

	// cachedCorrelationKey holds the per-key FIFO key that was stamped
	// on the concrete inner message at the time the wrapper was last
	// encoded. It exists because Decode replaces the typed inner
	// Message with a rawServerMessage that no longer implements
	// CorrelationKey, so a decoded wrapper has no way to recover the
	// key from the inner field alone. The outbox-CDC delivery path
	// hits this branch: the wrapper is encoded into the actor outbox,
	// later decoded by OutboxPublisher, then enqueued into the
	// serverconn mailbox for the first time, and that first enqueue
	// is where the durable mailbox reads CorrelationKey to stamp the
	// claim lane. Stored as []byte so Decode can hand the TLV record
	// straight through without a copy; the read path converts to
	// string at the CorrelationKey() boundary. Set by Decode; read
	// by CorrelationKey as the fallback when the structural assertion
	// on Message fails.
	cachedCorrelationKey []byte
}

// MessageType returns a human-readable type name for logging.
func (m *SendClientEventRequest) MessageType() string {
	return "SendClientEventRequest"
}

// TLVType returns the unique TLV type identifier for this message.
func (m *SendClientEventRequest) TLVType() tlv.Type {
	return SendClientEventRequestMsgType
}

// Encode serializes the message to the provided writer. The ServerMessage is
// converted to proto, wrapped in anypb.Any (preserving type information),
// and stored via a WrappedProto TLV record.
//
// We use TLV here (rather than storing raw proto bytes) because the
// DurableActor runtime requires all messages to satisfy the TLVMessage
// interface (TLVType, Encode, Decode). The MessageCodec uses these methods
// to serialize messages into the durable mailbox. WrappedProto handles the
// proto↔bytes conversion inside the TLV record, keeping the codec contract
// simple and uniform across message types.
func (m *SendClientEventRequest) Encode(w io.Writer) error {
	protoMsg, err := m.Message.ToProto().Unpack()
	if err != nil {
		return fmt.Errorf("convert to proto: %w", err)
	}

	service, method := eventRoutingMetadata(m)

	anyMsg, err := anypb.New(protoMsg)
	if err != nil {
		return fmt.Errorf("wrap proto in Any: %w", err)
	}

	// We still need the raw bytes for stable ID derivation, so marshal
	// deterministically before constructing the TLV records.
	anyBytes, err := (proto.MarshalOptions{
		Deterministic: true,
	}).Marshal(anyMsg)
	if err != nil {
		return fmt.Errorf("marshal Any: %w", err)
	}

	msgID := m.MsgID
	if msgID == "" {
		msgID = mailboxconn.StableEventMsgID(anyBytes)
	}
	msgIDBytes := []byte(msgID)

	idempotencyKey := m.IdempotencyKey
	if idempotencyKey == "" {
		idempotencyKey = mailboxconn.
			StableEventIdempotencyKey(
				anyBytes,
			)
	}
	idempotencyBytes := []byte(idempotencyKey)

	payload := tlv.NewRecordT[protoPayloadRecordTLV](
		mailboxconn.WrappedProto[*anypb.Any]{
			Val: anyMsg,
		},
	)
	msgIDRec := tlv.NewPrimitiveRecord[msgIDRecordTLV](
		msgIDBytes,
	)
	idemRec := tlv.NewPrimitiveRecord[idempotencyRecordTLV](
		idempotencyBytes,
	)
	svcRec := tlv.NewPrimitiveRecord[rpcServiceRecordTLV](
		[]byte(service),
	)
	methodRec := tlv.NewPrimitiveRecord[rpcMethodRecordTLV](
		[]byte(method),
	)

	// Persist the per-key FIFO key alongside the routing metadata so
	// a wrapper round-tripped through the outbox CDC table still
	// surfaces the right key on first enqueue into the serverconn
	// mailbox. Computing via CorrelationKey() consults the cached
	// value first (covering re-Encode of a previously decoded
	// wrapper), then falls back to the structural assertion on the
	// concrete inner Message (the normal pre-Encode path).
	corrKeyBytes := []byte(m.CorrelationKey())
	corrKeyRec := tlv.NewPrimitiveRecord[correlationKeyRecordTLV](
		corrKeyBytes,
	)

	stream, err := tlv.NewStream(
		payload.Record(), msgIDRec.Record(), idemRec.Record(),
		svcRec.Record(), methodRec.Record(), corrKeyRec.Record(),
	)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// Decode deserializes the message from the provided reader. The proto payload
// is stored as a rawServerMessage that lazily unmarshals via the global
// protobuf type registry.
func (m *SendClientEventRequest) Decode(r io.Reader) error {
	payload := tlv.ZeroRecordT[
		protoPayloadRecordTLV,
		mailboxconn.WrappedProto[*anypb.Any],
	]()
	payload.Val.Val = &anypb.Any{}

	msgIDRec := tlv.ZeroRecordT[msgIDRecordTLV, []byte]()
	idemRec := tlv.ZeroRecordT[idempotencyRecordTLV, []byte]()
	svcRec := tlv.ZeroRecordT[rpcServiceRecordTLV, []byte]()
	methodRec := tlv.ZeroRecordT[rpcMethodRecordTLV, []byte]()
	corrKeyRec := tlv.ZeroRecordT[correlationKeyRecordTLV, []byte]()

	stream, err := tlv.NewStream(
		payload.Record(), msgIDRec.Record(), idemRec.Record(),
		svcRec.Record(), methodRec.Record(), corrKeyRec.Record(),
	)
	if err != nil {
		return err
	}

	if _, err := stream.DecodeWithParsedTypes(r); err != nil {
		return err
	}

	m.Message = &rawServerMessage{anyMsg: payload.Val.Val}
	m.MsgID = string(msgIDRec.Val)
	m.IdempotencyKey = string(idemRec.Val)
	m.Service = string(svcRec.Val)
	m.Method = string(methodRec.Val)
	m.cachedCorrelationKey = corrKeyRec.Val

	return nil
}

// CorrelationKey forwards the inner ServerMessage's per-key FIFO key so
// the durable mailbox's per-correlation-key claim invariant fires on the
// right lane (e.g. "oor/<session>", "round/<id>"). Without this hop the
// wrapper's embedded BaseMessage default ("") would erase the inner key
// at enqueue and every outbox row would land in the unkeyed lane, leaving
// the original Nack-with-backoff reorder window open.
//
// The inner field is typed as ServerMessage (ToProto + ServiceMethod);
// the per-key contract lives on actor.Message. We use a structural
// assertion so the forward works for any concrete outbox message that
// opts in to per-key FIFO without coupling ServerMessage to the actor
// interface. That assertion covers the in-process pre-Encode path where
// the inner Message is the concrete OOR/round outbox type.
//
// After TLV decode the inner becomes a *rawServerMessage that no longer
// satisfies the assertion. That is the path the outbox-CDC delivery uses:
// sendTransportEvent encodes the wrapper into the actor outbox, the
// OutboxPublisher decodes it later, and then Tells the decoded wrapper
// into the serverconn mailbox for the first time. The durable mailbox
// reads CorrelationKey on that first enqueue, so we have to surface
// the key from somewhere other than the now-erased inner type. Encode
// persists the inner key in its own TLV record and Decode caches it on
// cachedCorrelationKey; this method falls back to that cache when the
// structural assertion misses.
func (m *SendClientEventRequest) CorrelationKey() string {
	if m == nil {
		return ""
	}

	if m.Message != nil {
		keyed, ok := m.Message.(interface{ CorrelationKey() string })
		if ok {
			if key := keyed.CorrelationKey(); key != "" {
				return key
			}
		}
	}

	return string(m.cachedCorrelationKey)
}

// serverConnMsgSealed implements the ServerConnMsg interface seal.
func (m *SendClientEventRequest) serverConnMsgSealed() {}

// SendClientEventResponse acknowledges that the message was sent.
type SendClientEventResponse struct {
	actor.BaseMessage

	// Success indicates whether the send operation succeeded.
	Success bool

	// Error contains the error message if the send failed.
	Error string
}

// MessageType returns a human-readable type name for logging.
func (m *SendClientEventResponse) MessageType() string {
	return "SendClientEventResponse"
}

// serverConnRespSealed implements the ServerConnResp interface seal.
func (m *SendClientEventResponse) serverConnRespSealed() {}

// SendRPCResponse acknowledges that an RPC envelope was sent.
type SendRPCResponse struct {
	actor.BaseMessage

	// Success indicates whether the send operation succeeded.
	Success bool

	// Error contains the error message if the send failed.
	Error string
}

// MessageType returns a human-readable type name for logging.
func (m *SendRPCResponse) MessageType() string {
	return "SendRPCResponse"
}

// serverConnRespSealed implements the ServerConnResp interface seal.
func (m *SendRPCResponse) serverConnRespSealed() {}

// SendRPCRequest wraps a pre-built outbound unary RPC envelope. The unary
// facade constructs the envelope with all metadata (correlation ID,
// idempotency key, service/method) and hands it to the connector for
// transport via Edge.Send.
//
// This type intentionally does not override CorrelationKey, so it is unkeyed
// (the BaseMessage default of ""). Under an EgressWorkers > 1 pool, unkeyed
// messages are not held in any per-key FIFO lane and may be sent in any order
// relative to one another. That is safe here precisely because every
// SendRPCRequest is an independent request/response RPC matched back to its
// caller by an explicit correlation ID through the response registry, so there
// is no ordered stream among them to violate. Do NOT add an order-sensitive
// payload to this type without also giving it a CorrelationKey: an unkeyed
// order-sensitive message would silently reorder across the worker pool with
// no error. Only SendClientEventRequest (the FSM event stream) is keyed,
// because it is the one egress path whose per-session order is load-bearing.
type SendRPCRequest struct {
	actor.BaseMessage

	// Envelope is the pre-built mailbox envelope ready for sending.
	Envelope *mailboxpb.Envelope
}

// MessageType returns a human-readable type name for logging.
func (m *SendRPCRequest) MessageType() string {
	return "SendRPCRequest"
}

// TLVType returns the unique TLV type identifier for this message.
func (m *SendRPCRequest) TLVType() tlv.Type {
	return SendRPCRequestMsgType
}

// Encode serializes the message to the provided writer. The mailbox
// envelope is stored via a WrappedProto TLV record.
func (m *SendRPCRequest) Encode(w io.Writer) error {
	envRec := tlv.NewRecordT[envelopeRecordTLV](
		mailboxconn.WrappedProto[*mailboxpb.Envelope]{
			Val: m.Envelope,
		},
	)

	stream, err := tlv.NewStream(envRec.Record())
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// Decode deserializes the message from the provided reader.
func (m *SendRPCRequest) Decode(r io.Reader) error {
	envRec := tlv.ZeroRecordT[
		envelopeRecordTLV,
		mailboxconn.WrappedProto[*mailboxpb.Envelope],
	]()
	envRec.Val.Val = &mailboxpb.Envelope{}

	stream, err := tlv.NewStream(envRec.Record())
	if err != nil {
		return err
	}

	if _, err := stream.DecodeWithParsedTypes(r); err != nil {
		return err
	}

	m.Envelope = envRec.Val.Val

	return nil
}

// serverConnMsgSealed implements the ServerConnMsg interface seal.
func (m *SendRPCRequest) serverConnMsgSealed() {}

// SendUnaryRequest wraps a typed unary RPC request for durable delivery via
// the mailbox edge. Unlike SendRPCRequest, the caller provides the request
// body and routing metadata rather than a pre-built envelope.
//
// Like SendRPCRequest, this type is intentionally unkeyed (no CorrelationKey
// override), so under an EgressWorkers > 1 pool distinct unary requests may be
// sent out of order relative to one another. That is safe because each is an
// independent request/response RPC correlated back to its waiter by an explicit
// correlation ID, not a position in an ordered stream. Do NOT add an
// order-sensitive payload here without also defining a CorrelationKey, or it
// will silently reorder across workers. See SendRPCRequest for the full
// rationale.
type SendUnaryRequest struct {
	actor.BaseMessage

	// Body is the request payload wrapped as Any.
	Body *anypb.Any

	// MsgID uniquely identifies this send attempt. Retries of the same
	// persisted request must reuse this ID.
	MsgID string

	// IdempotencyKey identifies the semantic operation for remote dedupe.
	IdempotencyKey string

	// Service is the fully-qualified protobuf service name.
	Service string

	// Method is the RPC method name.
	Method string

	// CorrelationID links this request to the eventual KIND_RESPONSE.
	CorrelationID string
}

// NewSendUnaryRequest constructs a durable unary request from the given proto
// payload and routing metadata.
func NewSendUnaryRequest(method mailboxrpc.ServiceMethod, req proto.Message,
	correlationID string) (*SendUnaryRequest, error) {

	if req == nil {
		return nil, fmt.Errorf("unary request body must be provided")
	}

	body, err := anypb.New(req)
	if err != nil {
		return nil, fmt.Errorf("wrap unary request in Any: %w", err)
	}

	return &SendUnaryRequest{
		Body:          body,
		Service:       method.Service,
		Method:        method.Method,
		CorrelationID: correlationID,
	}, nil
}

// MessageType returns a human-readable type name for logging.
func (m *SendUnaryRequest) MessageType() string {
	return "SendUnaryRequest"
}

// TLVType returns the unique TLV type identifier for this message.
func (m *SendUnaryRequest) TLVType() tlv.Type {
	return SendUnaryRequestMsgType
}

// Encode serializes the message to the provided writer.
func (m *SendUnaryRequest) Encode(w io.Writer) error {
	body := m.Body
	if body == nil {
		return fmt.Errorf("unary request body must be provided")
	}

	bodyBytes, err := (proto.MarshalOptions{
		Deterministic: true,
	}).Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal unary request body: %w", err)
	}

	msgID := m.MsgID
	if msgID == "" {
		msgID = mailboxconn.StableEventMsgID(bodyBytes)
	}

	idempotencyKey := m.IdempotencyKey
	if idempotencyKey == "" {
		idempotencyKey = mailboxconn.
			StableEventIdempotencyKey(
				bodyBytes,
			)
	}

	bodyRec := tlv.NewRecordT[protoPayloadRecordTLV](
		mailboxconn.WrappedProto[*anypb.Any]{
			Val: body,
		},
	)
	msgIDRec := tlv.NewPrimitiveRecord[msgIDRecordTLV](
		[]byte(msgID),
	)
	idemRec := tlv.NewPrimitiveRecord[idempotencyRecordTLV](
		[]byte(idempotencyKey),
	)
	svcRec := tlv.NewPrimitiveRecord[rpcServiceRecordTLV](
		[]byte(m.Service),
	)
	methodRec := tlv.NewPrimitiveRecord[rpcMethodRecordTLV](
		[]byte(m.Method),
	)
	corrRec := tlv.NewPrimitiveRecord[rpcCorrelationRecordTLV](
		[]byte(m.CorrelationID),
	)

	stream, err := tlv.NewStream(
		bodyRec.Record(), msgIDRec.Record(), idemRec.Record(),
		svcRec.Record(), methodRec.Record(), corrRec.Record(),
	)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// Decode deserializes the message from the provided reader.
func (m *SendUnaryRequest) Decode(r io.Reader) error {
	bodyRec := tlv.ZeroRecordT[
		protoPayloadRecordTLV,
		mailboxconn.WrappedProto[*anypb.Any],
	]()
	bodyRec.Val.Val = &anypb.Any{}

	msgIDRec := tlv.ZeroRecordT[msgIDRecordTLV, []byte]()
	idemRec := tlv.ZeroRecordT[idempotencyRecordTLV, []byte]()
	svcRec := tlv.ZeroRecordT[rpcServiceRecordTLV, []byte]()
	methodRec := tlv.ZeroRecordT[rpcMethodRecordTLV, []byte]()
	corrRec := tlv.ZeroRecordT[rpcCorrelationRecordTLV, []byte]()

	stream, err := tlv.NewStream(
		bodyRec.Record(), msgIDRec.Record(), idemRec.Record(),
		svcRec.Record(), methodRec.Record(), corrRec.Record(),
	)
	if err != nil {
		return err
	}

	if _, err := stream.DecodeWithParsedTypes(r); err != nil {
		return err
	}

	m.Body = bodyRec.Val.Val
	m.MsgID = string(msgIDRec.Val)
	m.IdempotencyKey = string(idemRec.Val)
	m.Service = string(svcRec.Val)
	m.Method = string(methodRec.Val)
	m.CorrelationID = string(corrRec.Val)

	return nil
}

// serverConnMsgSealed implements the ServerConnMsg interface seal.
func (m *SendUnaryRequest) serverConnMsgSealed() {}

// ServerConnectionActor is the unified connector boundary for all mailbox
// traffic between the client and the remote server. It serves as both:
//
//  1. An egress actor: receives outbound messages from durable protocol actors
//     (for example round and OOR) plus unary facade requests, then sends them
//     via the mailbox edge.
//
//  2. An ingress loop: continuously pulls envelopes from the remote mailbox,
//     dispatches them to local actors via ServiceKey-based routing, and
//     manages the ack watermark state machine to ensure at-least-once
//     delivery with crash safety.
//
// The actor is backed by a DurableActor for crash-safe egress. Outbound
// messages from protocol actors persist in the durable mailbox before
// processing, ensuring no message loss on crashes.
type ServerConnectionActor struct {
	// cfg holds all dependencies and tuning knobs for the connector.
	cfg ConnectorConfig

	log btclog.Logger

	// responseRegistry maps correlation IDs to unary RPC waiters and
	// buffers early responses that arrive before a waiter is registered.
	// This is in-memory only.
	responseRegistry *mailboxconn.ResponseRegistry

	// cancelCh delivers the ingress loop cancel function from
	// StartIngress to StopIngress without a shared field, avoiding
	// any data-race between the two methods.
	cancelCh chan context.CancelFunc

	// stopOnce ensures StopIngress cancels the ingress loop exactly
	// once.
	stopOnce sync.Once

	// wg tracks the ingress loop goroutine for clean shutdown.
	wg sync.WaitGroup

	// lastSendNano stores the UnixNano timestamp of the last
	// successful outbound Edge.Send. The heartbeat goroutine
	// checks this to skip sending when real traffic already
	// proves liveness.
	lastSendNano atomic.Int64

	// compatErr caches the first permanent version error that moved the
	// connector to the terminal INCOMPATIBLE state. A nil pointer means the
	// connector is still COMPATIBLE. Once set, new sends return this error
	// without contacting the edge.
	compatErr atomic.Pointer[mailboxconn.StatusError]

	// compatOnce guarantees the incompatibility transition (cache, cancel,
	// fail waiters, callback) runs exactly once even under concurrent
	// permanent failures.
	compatOnce sync.Once

	// ingressCancel holds the ingress/heartbeat context cancel function so
	// the incompatibility transition can stop them asynchronously without
	// joining its own goroutine. CancelFunc is idempotent, so StopIngress
	// may also invoke it.
	ingressCancel atomic.Pointer[context.CancelFunc]
}

// NewServerConnectionActor creates a new server connection actor with the
// given configuration. The actor must be started via its DurableActor wrapper
// and the ingress loop must be started separately via StartIngress.
func NewServerConnectionActor(
	cfg ConnectorConfig,
) *ServerConnectionActor {

	// Stamp the runtime-bound version pair onto every outbound envelope in
	// one place by wrapping the edge, so no individual send path has to
	// remember to stamp. This mirrors the auth decorator layered over the
	// same edge.
	cfg.Edge = newVersionStampingMailboxClient(
		cfg.Edge, cfg.MailboxProtocolVersion, cfg.ArkProtocolVersion,
	)

	return &ServerConnectionActor{
		cfg: cfg,
		log: cfg.Log.UnwrapOr(btclog.Disabled),
		responseRegistry: mailboxconn.NewResponseRegistry(
			cfg.ResponseWaiterTTL,
		),
		cancelCh: make(chan context.CancelFunc, 1),
	}
}

// egressTx is the transaction-scoped store for the serverconn egress behavior.
// The egress path persists no domain state -- each handler builds an envelope
// and sends it over the wire -- so the store is empty. It exists only to
// satisfy the TxBehavior store type parameter; the sole work inside an egress
// Commit transaction is the framework's lease-fenced ack and dedup mark.
type egressTx struct{}

// bindStores is the StoreFactory for the egress durable actor. Egress joins no
// domain stores to the Commit transaction, so the factory ignores its arguments
// and returns the empty egressTx.
func (a *ServerConnectionActor) bindStores(context.Context,
	actor.DeliveryStore) egressTx {

	return egressTx{}
}

// commitSend consumes the in-flight egress message exactly once after a
// successful Edge.Send. Because egress writes no domain state, the Commit
// closure is empty: the framework folds the lease-fenced ack and the dedup mark
// into one short writer transaction, advancing the mailbox now that the wire
// send -- the actual side effect -- has completed. Crucially the writer lock is
// held only for this sub-millisecond bookkeeping, never across the gRPC send,
// which is the whole point of the Read/Commit migration.
//
// A lost lease (actor.ErrLeaseLost) means a concurrent worker reclaimed and
// re-sent the message while this Send was in flight; the duplicate is absorbed
// by the server's MsgId/IdempotencyKey dedup, and we surface the error so the
// framework's retry path takes over.
func (a *ServerConnectionActor) commitSend(ctx context.Context,
	ax actor.Exec[egressTx]) error {

	return ax.Commit(ctx, func(context.Context, egressTx) error {
		return nil
	})
}

// Receive processes incoming egress messages. This is called by the durable
// actor runtime when messages arrive in the actor's mailbox. The connector runs
// on the Read/Commit execution path: each handler builds and sends its envelope
// with no writer lock held, then folds the lease-fenced ack into one short
// Commit via commitSend.
func (a *ServerConnectionActor) Receive(ctx context.Context, msg ServerConnMsg,
	ax actor.Exec[egressTx]) fn.Result[ServerConnResp] {

	switch m := msg.(type) {
	case *SendClientEventRequest:
		return a.handleSendClientEvent(ctx, m, ax)

	case *SendUnaryRequest:
		return a.handleSendUnaryRequest(ctx, m, ax)

	case DurableUnaryQuery:
		unary, buildErr := a.buildDurableUnary(ctx, m)
		if buildErr != nil {
			return fn.Err[ServerConnResp](buildErr)
		}

		return a.handleSendUnaryRequest(ctx, unary, ax)

	case *SendRPCRequest:
		return a.handleSendRPCRequest(ctx, m, ax)

	default:
		return fn.Err[ServerConnResp](
			fmt.Errorf("unknown message type: %T", msg),
		)
	}
}

// handleSendClientEvent converts a client FSM outbox message to a proto
// message and sends it to the server via the mailbox edge.
func (a *ServerConnectionActor) handleSendClientEvent(ctx context.Context,
	req *SendClientEventRequest,
	ax actor.Exec[egressTx]) fn.Result[ServerConnResp] {

	if ce := a.compatibilityError(); ce != nil {
		return fn.Err[ServerConnResp](ce)
	}

	protoMsg, err := req.Message.ToProto().Unpack()
	if err != nil {
		a.log.WarnS(ctx, "Failed to convert to proto", err)

		return fn.Err[ServerConnResp](
			fmt.Errorf("convert to proto: %w", err),
		)
	}

	body, err := anypb.New(protoMsg)
	if err != nil {
		return fn.Err[ServerConnResp](
			fmt.Errorf("wrap proto in Any: %w", err),
		)
	}

	msgID := req.MsgID
	idempotencyKey := req.IdempotencyKey

	// Only marshal the body bytes when we need to derive stable IDs.
	// On replay (both IDs already set from the persisted TLV), this
	// marshal is skipped.
	if msgID == "" || idempotencyKey == "" {
		bodyBytes, marshalErr := (proto.MarshalOptions{
			Deterministic: true,
		}).Marshal(body)
		if marshalErr != nil {
			return fn.Err[ServerConnResp](
				fmt.Errorf("marshal event body: %w",
					marshalErr),
			)
		}

		if msgID == "" {
			msgID = mailboxconn.StableEventMsgID(bodyBytes)
		}

		if idempotencyKey == "" {
			idempotencyKey = mailboxconn.
				StableEventIdempotencyKey(
					bodyBytes,
				)
		}
	}

	service, method := eventRoutingMetadata(req)

	envelope := &mailboxpb.Envelope{
		MsgId:           msgID,
		IdempotencyKey:  idempotencyKey,
		Sender:          a.cfg.LocalMailboxID,
		Recipient:       a.cfg.RemoteMailboxID,
		CreatedAtUnixMs: time.Now().UnixMilli(),
		Headers:         a.cfg.mergeAuthHeaders(nil),
		Body:            body,
		Rpc: &mailboxpb.RpcMeta{
			Kind:    mailboxpb.RpcMeta_KIND_EVENT,
			Service: service,
			Method:  method,
			ReplyTo: a.cfg.LocalMailboxID,
		},
	}

	// Use a detached context for the gRPC call so that parent
	// cancellation does not abort the outbound send, while
	// preserving trace/logging context values.
	sendCtx, sendCancel := context.WithTimeout(
		context.WithoutCancel(ctx), defaultSendEventTimeout,
	)
	defer sendCancel()

	resp, err := a.cfg.Edge.Send(sendCtx, &mailboxpb.SendRequest{
		Envelope: envelope,
	})
	if sErr := edgeResponseError(
		"send client event", resp, err,
	); sErr != nil {

		a.checkPermanentStatus(ctx, sErr)

		return fn.Err[ServerConnResp](sErr)
	}

	a.lastSendNano.Store(time.Now().UnixNano())

	if err := a.commitSend(ctx, ax); err != nil {
		return fn.Err[ServerConnResp](err)
	}

	return fn.Ok[ServerConnResp](&SendClientEventResponse{
		Success: true,
	})
}

// handleSendUnaryRequest sends a durable correlated unary request via the
// mailbox edge.
func (a *ServerConnectionActor) handleSendUnaryRequest(ctx context.Context,
	req *SendUnaryRequest,
	ax actor.Exec[egressTx]) fn.Result[ServerConnResp] {

	if req == nil {
		return fn.Err[ServerConnResp](
			fmt.Errorf("unary request must be provided"),
		)
	}

	if req.Body == nil {
		return fn.Err[ServerConnResp](
			fmt.Errorf("unary request body must be provided"),
		)
	}

	if req.Service == "" || req.Method == "" {
		return fn.Err[ServerConnResp](
			fmt.Errorf("unary request service and method must " +
				"be provided"),
		)
	}

	if req.CorrelationID == "" {
		return fn.Err[ServerConnResp](
			fmt.Errorf("unary request correlation id must be " +
				"provided"),
		)
	}

	msgID := req.MsgID
	idempotencyKey := req.IdempotencyKey

	if msgID == "" || idempotencyKey == "" {
		bodyBytes, err := (proto.MarshalOptions{
			Deterministic: true,
		}).Marshal(req.Body)
		if err != nil {
			return fn.Err[ServerConnResp](
				fmt.Errorf("marshal unary body: %w", err),
			)
		}

		if msgID == "" {
			msgID = mailboxconn.StableEventMsgID(bodyBytes)
		}

		if idempotencyKey == "" {
			idempotencyKey = mailboxconn.
				StableEventIdempotencyKey(
					bodyBytes,
				)
		}
	}

	return a.sendUnaryEnvelope(
		ctx, ax, req.Body, req.Service, req.Method, req.CorrelationID,
		msgID, idempotencyKey,
	)
}

// buildDurableUnary converts a DurableUnaryQuery into a SendUnaryRequest by
// calling the query's BuildBody with the configured DurableUnaryRequestBuilder.
// The returned SendUnaryRequest can be passed directly to
// handleSendUnaryRequest.
func (a *ServerConnectionActor) buildDurableUnary(ctx context.Context,
	q DurableUnaryQuery) (*SendUnaryRequest, error) {

	if a.cfg.DurableUnaryBuilder == nil {
		return nil, fmt.Errorf("durable unary builder must be provided")
	}

	if q.QueryCorrelationID() == "" {
		return nil, fmt.Errorf("durable unary query requires a " +
			"correlation ID")
	}

	body, stableBytes, err := q.BuildBody(
		ctx, a.cfg.DurableUnaryBuilder,
	)
	if err != nil {
		return nil, err
	}

	msgID := q.QueryMsgID()
	if msgID == "" {
		msgID = mailboxconn.StableEventMsgID(stableBytes)
	}

	idempotencyKey := q.QueryIdempotencyKey()
	if idempotencyKey == "" {
		idempotencyKey = mailboxconn.
			StableEventIdempotencyKey(
				stableBytes,
			)
	}

	method := q.ServiceMethod()

	return &SendUnaryRequest{
		Body:           body,
		Service:        method.Service,
		Method:         method.Method,
		CorrelationID:  q.QueryCorrelationID(),
		MsgID:          msgID,
		IdempotencyKey: idempotencyKey,
	}, nil
}

// sendUnaryEnvelope sends one durable unary request envelope via the mailbox
// edge using the given routing metadata and stable identifiers.
func (a *ServerConnectionActor) sendUnaryEnvelope(ctx context.Context,
	ax actor.Exec[egressTx], body *anypb.Any, service string, method string,
	correlationID string, msgID string,
	idempotencyKey string) fn.Result[ServerConnResp] {

	if ce := a.compatibilityError(); ce != nil {
		return fn.Err[ServerConnResp](ce)
	}

	envelope := &mailboxpb.Envelope{
		MsgId:           msgID,
		IdempotencyKey:  idempotencyKey,
		Sender:          a.cfg.LocalMailboxID,
		Recipient:       a.cfg.RemoteMailboxID,
		CreatedAtUnixMs: time.Now().UnixMilli(),
		Headers:         a.cfg.mergeAuthHeaders(nil),
		Body:            body,
		Rpc: &mailboxpb.RpcMeta{
			Kind:          mailboxpb.RpcMeta_KIND_REQUEST,
			Service:       service,
			Method:        method,
			CorrelationId: correlationID,
			ReplyTo:       a.cfg.LocalMailboxID,
		},
	}

	sendCtx, sendCancel := context.WithTimeout(
		context.WithoutCancel(ctx), defaultSendEventTimeout,
	)
	defer sendCancel()

	resp, err := a.cfg.Edge.Send(sendCtx, &mailboxpb.SendRequest{
		Envelope: envelope,
	})
	if sErr := edgeResponseError(
		"send unary request", resp, err,
	); sErr != nil {

		a.checkPermanentStatus(ctx, sErr)

		return fn.Err[ServerConnResp](sErr)
	}

	if err := a.commitSend(ctx, ax); err != nil {
		return fn.Err[ServerConnResp](err)
	}

	return fn.Ok[ServerConnResp](&SendRPCResponse{
		Success: true,
	})
}

// eventRoutingMetadata returns the outbound routing metadata persisted on a
// SendClientEventRequest. When callers leave Service/Method empty, we derive
// them directly from the wrapped ServerMessage so direct sends remain aligned
// with the production round-actor outbox path.
func eventRoutingMetadata(req *SendClientEventRequest) (string, string) {
	if req == nil {
		return "", ""
	}

	if req.Service != "" && req.Method != "" {
		return req.Service, req.Method
	}

	if req.Message == nil {
		return req.Service, req.Method
	}

	sm := req.Message.ServiceMethod()
	service := req.Service
	method := req.Method
	if service == "" {
		service = sm.Service
	}
	if method == "" {
		method = sm.Method
	}

	return service, method
}

// handleSendRPCRequest sends a pre-built unary RPC envelope via the mailbox
// edge.
func (a *ServerConnectionActor) handleSendRPCRequest(ctx context.Context,
	req *SendRPCRequest,
	ax actor.Exec[egressTx]) fn.Result[ServerConnResp] {

	if ce := a.compatibilityError(); ce != nil {
		return fn.Err[ServerConnResp](ce)
	}

	resp, err := a.cfg.Edge.Send(ctx, &mailboxpb.SendRequest{
		Envelope: req.Envelope,
	})
	if sErr := edgeResponseError(
		"send rpc request", resp, err,
	); sErr != nil {

		a.checkPermanentStatus(ctx, sErr)

		return fn.Err[ServerConnResp](sErr)
	}

	a.lastSendNano.Store(time.Now().UnixNano())

	if err := a.commitSend(ctx, ax); err != nil {
		return fn.Err[ServerConnResp](err)
	}

	return fn.Ok[ServerConnResp](&SendRPCResponse{
		Success: true,
	})
}

// RegisterWaiter adds a response waiter for the given correlation ID. The
// returned Future completes when the ingress loop delivers a KIND_RESPONSE
// with a matching correlation ID, or errors if the waiter expires or is
// cancelled.
func (a *ServerConnectionActor) RegisterWaiter(
	id CorrelationID,
) actor.Future[*mailboxpb.Envelope] {

	return a.responseRegistry.RegisterWaiter(id)
}

// removeWaiter removes a previously registered waiter, preventing leaks on
// cancellation or timeout.
func (a *ServerConnectionActor) removeWaiter(id CorrelationID) {
	a.responseRegistry.RemoveWaiter(id)
}

// removePendingResponse drops any buffered early response for the correlation
// ID. Durable response dispatchers use this after converting a response into a
// local actor message so the in-memory unary path cannot consume it later.
func (a *ServerConnectionActor) removePendingResponse(id CorrelationID) {
	a.responseRegistry.RemovePending(id)
}

// hasResponseWaiter reports whether an active in-memory waiter is registered
// for the correlation ID. The ingress loop uses this to classify a
// KIND_RESPONSE at split time: only responses with a live waiter take the fast
// pre-transaction delivery path; everything else folds into the durable
// dispatch transaction.
func (a *ServerConnectionActor) hasResponseWaiter(id CorrelationID) bool {
	return a.responseRegistry.HasWaiter(id)
}

// deliverResponse looks up a waiter by correlation ID and delivers the
// envelope. If no waiter exists yet, the response is buffered so a later
// AwaitRPC call can still observe it.
func (a *ServerConnectionActor) deliverResponse(
	id CorrelationID, env *mailboxpb.Envelope,
) mailboxconn.DeliveryResult {

	return a.responseRegistry.DeliverResponse(id, env)
}

// StartIngress loads the ack checkpoint from the store and launches the
// background ingress loop and heartbeat goroutines. If the checkpoint
// cannot be loaded, an error is returned and neither goroutine is
// started — the caller should treat this as a fatal startup failure.
func (a *ServerConnectionActor) StartIngress(
	ctx context.Context,
) error {

	// Fast path: if the connector already transitioned to the terminal
	// incompatible state (e.g. a durable egress replay hit a permanent
	// version error before ingress started), refuse to start polling or
	// load checkpoints. Returning the cached error keeps the caller from
	// marking the connection healthy.
	if ce := a.compatibilityError(); ce != nil {
		return ce
	}

	state, err := a.loadCheckpoint(ctx)
	if err != nil {
		return fmt.Errorf("load ingress checkpoint: %w", err)
	}

	ingressCtx, cancel := context.WithCancel(ctx)

	// Publish the cancel func so the incompatibility transition can stop
	// ingress and heartbeat asynchronously. CancelFunc is idempotent, so
	// StopIngress invoking it again via cancelCh is harmless.
	a.ingressCancel.Store(&cancel)

	// Recheck after publishing the cancel func to close the race with a
	// concurrent markIncompatible. The two transitions touch the compat
	// error and the cancel pointer in opposite orders: markIncompatible
	// stores the compat error then loads the cancel; we store the cancel
	// then load the compat error. So at least one of us observes the
	// other — either we see the incompatible state here and abort, or
	// markIncompatible sees our published cancel and stops the goroutines.
	if ce := a.compatibilityError(); ce != nil {
		cancel()

		return ce
	}

	a.wg.Add(2)
	a.cancelCh <- cancel

	if a.cfg.AuthSignature != nil || a.cfg.TLSBindSignature != nil {
		// Prime server-side mailbox registration before the first Pull.
		// The heartbeat envelope carries the same Schnorr and
		// TLS-binding headers as normal outbound traffic, so the server
		// can record the binding without waiting for the first ticker
		// or user request.
		heartbeatCtx, heartbeatCancel := context.WithTimeout(
			ingressCtx, defaultSendEventTimeout,
		)
		a.sendHeartbeat(heartbeatCtx)
		heartbeatCancel()
	}

	go a.ingressLoop(ingressCtx, state)
	go func() {
		defer a.wg.Done()
		a.startHeartbeat(ingressCtx)
	}()

	return nil
}

// StopIngress cancels the ingress loop and waits for it to exit. Safe to
// call multiple times — the cancel is executed at most once.
func (a *ServerConnectionActor) StopIngress() {
	a.stopOnce.Do(func() {
		select {
		case fn := <-a.cancelCh:
			fn()

		default:
		}
	})

	a.wg.Wait()
}

// NewServerConnCodec creates a MessageCodec with all server connection
// message types registered.
func NewServerConnCodec() *actor.MessageCodec {
	codec := actor.NewMessageCodec()

	codec.MustRegister(
		SendClientEventRequestMsgType,
		func() actor.TLVMessage {
			return &SendClientEventRequest{}
		},
	)

	codec.MustRegister(
		SendRPCRequestMsgType,
		func() actor.TLVMessage {
			return &SendRPCRequest{}
		},
	)
	codec.MustRegister(
		SendUnaryRequestMsgType,
		func() actor.TLVMessage {
			return &SendUnaryRequest{}
		},
	)
	codec.MustRegister(
		SendListOORRecipientEventsByScriptRequestMsgType,
		func() actor.TLVMessage {
			return &SendListOORRecipientEventsByScriptRequest{}
		},
	)
	codec.MustRegister(
		SendListVTXOsByScriptsRequestMsgType,
		func() actor.TLVMessage {
			return &SendListVTXOsByScriptsRequest{}
		},
	)

	return codec
}

// Compile-time interface checks.
var (
	_ ServerConnMsg  = (*SendClientEventRequest)(nil)
	_ ServerConnMsg  = (*SendUnaryRequest)(nil)
	_ ServerConnMsg  = (*SendRPCRequest)(nil)
	_ ServerConnMsg  = (*SendListOORRecipientEventsByScriptRequest)(nil)
	_ ServerConnMsg  = (*SendListVTXOsByScriptsRequest)(nil)
	_ ServerConnResp = (*SendClientEventResponse)(nil)
	_ ServerConnResp = (*SendRPCResponse)(nil)

	//nolint:ll
	_ actor.TxBehavior[ServerConnMsg, ServerConnResp, egressTx] = (*ServerConnectionActor)(nil)
)
