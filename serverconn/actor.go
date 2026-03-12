package serverconn

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/lightninglabs/darepo-client/baselib/actor"
	mailboxconn "github.com/lightninglabs/darepo-client/mailbox/conn"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/darepo-client/mailbox/rpc"
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
)

// TLV record type aliases for RecordT-style message field serialization.
type (
	protoPayloadRecordTLV = tlv.TlvType1
	envelopeRecordTLV     = tlv.TlvType2
	msgIDRecordTLV        = tlv.TlvType3
	idempotencyRecordTLV  = tlv.TlvType4
	rpcServiceRecordTLV   = tlv.TlvType5
	rpcMethodRecordTLV    = tlv.TlvType6
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
		return fn.Err[proto.Message](fmt.Errorf(
			"unmarshal Any type %q: %w",
			m.anyMsg.GetTypeUrl(), err,
		))
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
			StableEventIdempotencyKey(anyBytes)
	}
	idempotencyBytes := []byte(idempotencyKey)

	payload := tlv.NewRecordT[protoPayloadRecordTLV](
		mailboxconn.WrappedProto[*anypb.Any]{Val: anyMsg},
	)
	msgIDRec := tlv.NewPrimitiveRecord[msgIDRecordTLV](
		msgIDBytes,
	)
	idemRec := tlv.NewPrimitiveRecord[idempotencyRecordTLV](
		idempotencyBytes,
	)
	svcRec := tlv.NewPrimitiveRecord[rpcServiceRecordTLV](
		[]byte(m.Service),
	)
	methodRec := tlv.NewPrimitiveRecord[rpcMethodRecordTLV](
		[]byte(m.Method),
	)

	stream, err := tlv.NewStream(
		payload.Record(), msgIDRec.Record(),
		idemRec.Record(), svcRec.Record(),
		methodRec.Record(),
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

	stream, err := tlv.NewStream(
		payload.Record(), msgIDRec.Record(),
		idemRec.Record(), svcRec.Record(),
		methodRec.Record(),
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

	return nil
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

// ServerConnectionActor is the unified connector boundary for all mailbox
// traffic between the client and the remote server. It serves as both:
//
//  1. An egress actor: receives outbound messages from the round actor (FSM
//     events) and unary facade (RPC requests), then sends them via the
//     mailbox edge.
//
//  2. An ingress loop: continuously pulls envelopes from the remote mailbox,
//     dispatches them to local actors via ServiceKey-based routing, and
//     manages the ack watermark state machine to ensure at-least-once
//     delivery with crash safety.
//
// The actor is backed by a DurableActor for crash-safe egress. Outbound
// messages from the round actor persist in the durable mailbox before
// processing, ensuring no message loss on crashes.
type ServerConnectionActor struct {
	// cfg holds all dependencies and tuning knobs for the connector.
	cfg ConnectorConfig

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
}

// NewServerConnectionActor creates a new server connection actor with the
// given configuration. The actor must be started via its DurableActor wrapper
// and the ingress loop must be started separately via StartIngress.
func NewServerConnectionActor(
	cfg ConnectorConfig,
) *ServerConnectionActor {

	return &ServerConnectionActor{
		cfg: cfg,
		responseRegistry: mailboxconn.NewResponseRegistry(
			cfg.ResponseWaiterTTL,
		),
		cancelCh: make(chan context.CancelFunc, 1),
	}
}

// Receive processes incoming egress messages. This is called by the durable
// actor runtime when messages arrive in the actor's mailbox.
func (a *ServerConnectionActor) Receive(ctx context.Context,
	msg ServerConnMsg) fn.Result[ServerConnResp] {

	switch m := msg.(type) {
	case *SendClientEventRequest:
		return a.handleSendClientEvent(ctx, m)

	case *SendRPCRequest:
		return a.handleSendRPCRequest(ctx, m)

	default:
		return fn.Err[ServerConnResp](fmt.Errorf(
			"unknown message type: %T", msg,
		))
	}
}

// handleSendClientEvent converts a client FSM outbox message to a proto
// message and sends it to the server via the mailbox edge.
func (a *ServerConnectionActor) handleSendClientEvent(ctx context.Context,
	req *SendClientEventRequest) fn.Result[ServerConnResp] {

	protoMsg, err := req.Message.ToProto().Unpack()
	if err != nil {
		return fn.Err[ServerConnResp](fmt.Errorf(
			"convert to proto: %w", err,
		))
	}

	body, err := anypb.New(protoMsg)
	if err != nil {
		return fn.Err[ServerConnResp](fmt.Errorf(
			"wrap proto in Any: %w", err,
		))
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
			return fn.Err[ServerConnResp](fmt.Errorf(
				"marshal event body: %w", marshalErr,
			))
		}

		if msgID == "" {
			msgID = mailboxconn.StableEventMsgID(bodyBytes)
		}

		if idempotencyKey == "" {
			idempotencyKey = mailboxconn.
				StableEventIdempotencyKey(bodyBytes)
		}
	}

	envelope := &mailboxpb.Envelope{
		ProtocolVersion: a.cfg.ProtocolVersion,
		MsgId:           msgID,
		IdempotencyKey:  idempotencyKey,
		Sender:          a.cfg.LocalMailboxID,
		Recipient:       a.cfg.RemoteMailboxID,
		CreatedAtUnixMs: time.Now().UnixMilli(),
		Body:            body,
		Rpc: &mailboxpb.RpcMeta{
			Kind:    mailboxpb.RpcMeta_KIND_EVENT,
			Service: req.Service,
			Method:  req.Method,
			ReplyTo: a.cfg.LocalMailboxID,
		},
	}

	resp, err := a.cfg.Edge.Send(ctx, &mailboxpb.SendRequest{
		Envelope: envelope,
	})
	if err != nil {
		return fn.Err[ServerConnResp](fmt.Errorf(
			"send client event: %w", err,
		))
	}

	if resp.Status != nil && !resp.Status.Ok {
		return fn.Err[ServerConnResp](fmt.Errorf(
			"send client event: %s (%s)",
			resp.Status.Message, resp.Status.Code,
		))
	}

	return fn.Ok[ServerConnResp](&SendClientEventResponse{
		Success: true,
	})
}

// handleSendRPCRequest sends a pre-built unary RPC envelope via the mailbox
// edge.
func (a *ServerConnectionActor) handleSendRPCRequest(ctx context.Context,
	req *SendRPCRequest) fn.Result[ServerConnResp] {

	resp, err := a.cfg.Edge.Send(ctx, &mailboxpb.SendRequest{
		Envelope: req.Envelope,
	})
	if err != nil {
		return fn.Err[ServerConnResp](fmt.Errorf(
			"send rpc request: %w", err,
		))
	}

	if resp.Status != nil && !resp.Status.Ok {
		return fn.Err[ServerConnResp](fmt.Errorf(
			"send rpc request: %s (%s)",
			resp.Status.Message, resp.Status.Code,
		))
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

// deliverResponse looks up a waiter by correlation ID and delivers the
// envelope. If no waiter exists yet, the response is buffered so a later
// AwaitRPC call can still observe it.
func (a *ServerConnectionActor) deliverResponse(
	id CorrelationID, env *mailboxpb.Envelope,
) bool {

	return a.responseRegistry.DeliverResponse(id, env)
}

// StartIngress loads the ack checkpoint from the store and launches the
// background ingress loop goroutine. If the checkpoint cannot be loaded,
// an error is returned and the loop is not started — the caller should
// treat this as a fatal startup failure.
func (a *ServerConnectionActor) StartIngress(
	ctx context.Context,
) error {

	state, err := a.loadCheckpoint(ctx)
	if err != nil {
		return fmt.Errorf("load ingress checkpoint: %w", err)
	}

	ingressCtx, cancel := context.WithCancel(ctx)

	a.wg.Add(1)
	a.cancelCh <- cancel
	go a.ingressLoop(ingressCtx, state)

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

	return codec
}

// Compile-time interface checks.
var (
	_ ServerConnMsg  = (*SendClientEventRequest)(nil)
	_ ServerConnMsg  = (*SendRPCRequest)(nil)
	_ ServerConnResp = (*SendClientEventResponse)(nil)
	_ ServerConnResp = (*SendRPCResponse)(nil)

	//nolint:ll
	_ actor.ActorBehavior[ServerConnMsg, ServerConnResp] = (*ServerConnectionActor)(nil)
)
