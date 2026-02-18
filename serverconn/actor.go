package serverconn

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/lightninglabs/darepo-client/baselib/actor"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
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

// TLV record type constants for message field serialization.
const (
	protoPayloadRecordType tlv.Type = 1
	envelopeRecordType     tlv.Type = 2
)

// ServerMessage is an interface that client FSM outbox messages must implement
// to be sent to the server. This allows conversion to proto messages without
// creating import cycles.
type ServerMessage interface {
	// ToProto converts the message to a protobuf message that can be sent
	// over gRPC.
	ToProto() proto.Message
}

// rawServerMessage wraps a protobuf Any for reconstructing a ServerMessage
// after TLV deserialization. The original concrete type is recovered using
// the global protobuf type registry via anypb.UnmarshalNew.
type rawServerMessage struct {
	anyMsg *anypb.Any
}

// ToProto reconstructs the original proto message from the stored Any
// wrapper. Returns nil if the type cannot be resolved from the global
// protobuf registry.
func (m *rawServerMessage) ToProto() proto.Message {
	msg, err := m.anyMsg.UnmarshalNew()
	if err != nil {
		log.ErrorS(context.Background(),
			"Failed to unmarshal Any type from registry",
			err,
			slog.String("type_url",
				m.anyMsg.GetTypeUrl()))

		return nil
	}

	return msg
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
// and marshaled to bytes for TLV storage.
func (m *SendClientEventRequest) Encode(w io.Writer) error {
	anyMsg, err := anypb.New(m.Message.ToProto())
	if err != nil {
		return fmt.Errorf("wrap proto in Any: %w", err)
	}

	anyBytes, err := proto.Marshal(anyMsg)
	if err != nil {
		return fmt.Errorf("marshal Any: %w", err)
	}

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(protoPayloadRecordType, &anyBytes),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// Decode deserializes the message from the provided reader. The proto payload
// is stored as a rawServerMessage that lazily unmarshals via the global
// protobuf type registry.
func (m *SendClientEventRequest) Decode(r io.Reader) error {
	var payload []byte

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(protoPayloadRecordType, &payload),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return err
	}

	if _, err := stream.DecodeWithParsedTypes(r); err != nil {
		return err
	}

	anyMsg := &anypb.Any{}
	if err := proto.Unmarshal(payload, anyMsg); err != nil {
		return fmt.Errorf("unmarshal Any: %w", err)
	}

	m.Message = &rawServerMessage{anyMsg: anyMsg}

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

// Encode serializes the message to the provided writer. The entire mailbox
// envelope is proto-marshaled for TLV storage.
func (m *SendRPCRequest) Encode(w io.Writer) error {
	envBytes, err := proto.Marshal(m.Envelope)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(envelopeRecordType, &envBytes),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// Decode deserializes the message from the provided reader.
func (m *SendRPCRequest) Decode(r io.Reader) error {
	var envBytes []byte

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(envelopeRecordType, &envBytes),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return err
	}

	if _, err := stream.DecodeWithParsedTypes(r); err != nil {
		return err
	}

	m.Envelope = &mailboxpb.Envelope{}
	if err := proto.Unmarshal(envBytes, m.Envelope); err != nil {
		return fmt.Errorf("unmarshal envelope: %w", err)
	}

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

	// responseRegistryMu protects concurrent access to the response
	// registry from the ingress loop and unary facade callers.
	responseRegistryMu sync.Mutex

	// responseRegistry maps correlation IDs to unary RPC waiters. The
	// ingress loop delivers KIND_RESPONSE envelopes to the appropriate
	// waiter channel. This is in-memory only — if the process crashes,
	// callers' contexts are cancelled and they retry.
	responseRegistry map[CorrelationID]*ResponseWaiter

	// cancelCh delivers the ingress loop cancel function from
	// StartIngress to StopIngress without a shared field, avoiding
	// any data-race between the two methods.
	cancelCh chan context.CancelFunc

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
		cfg:              cfg,
		responseRegistry: make(map[CorrelationID]*ResponseWaiter),
		cancelCh:         make(chan context.CancelFunc, 1),
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

	protoMsg := req.Message.ToProto()

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

	return fn.Ok[ServerConnResp](&SendClientEventResponse{
		Success: true,
	})
}

// RegisterWaiter adds a response waiter for the given correlation ID. The
// returned channel will receive the response envelope when the ingress loop
// pulls a KIND_RESPONSE with a matching correlation ID.
func (a *ServerConnectionActor) RegisterWaiter(
	id CorrelationID,
) <-chan *mailboxpb.Envelope {

	a.responseRegistryMu.Lock()
	defer a.responseRegistryMu.Unlock()

	waiter := &ResponseWaiter{
		Ch:      make(chan *mailboxpb.Envelope, 1),
		Created: time.Now(),
	}

	a.responseRegistry[id] = waiter

	return waiter.Ch
}

// removeWaiter removes a previously registered waiter, preventing leaks on
// cancellation or timeout.
func (a *ServerConnectionActor) removeWaiter(id CorrelationID) {
	a.responseRegistryMu.Lock()
	defer a.responseRegistryMu.Unlock()

	delete(a.responseRegistry, id)
}

// deliverResponse looks up a waiter by correlation ID and delivers the
// envelope. Returns true if a waiter was found and signaled.
func (a *ServerConnectionActor) deliverResponse(
	id CorrelationID, env *mailboxpb.Envelope,
) bool {

	a.responseRegistryMu.Lock()
	waiter, ok := a.responseRegistry[id]
	if ok {
		delete(a.responseRegistry, id)
	}
	a.responseRegistryMu.Unlock()

	if !ok {
		return false
	}

	// Non-blocking send on buffered channel. If the waiter's context
	// was already cancelled, the envelope is dropped (caller retries).
	select {
	case waiter.Ch <- env:
	default:
	}

	return true
}

// StartIngress launches the background ingress loop goroutine that
// continuously pulls envelopes from the remote mailbox and dispatches them
// to local actors.
func (a *ServerConnectionActor) StartIngress(ctx context.Context) {
	ingressCtx, cancel := context.WithCancel(ctx)

	a.wg.Add(1)
	a.cancelCh <- cancel
	go a.ingressLoop(ingressCtx)
}

// StopIngress cancels the ingress loop and waits for it to exit.
func (a *ServerConnectionActor) StopIngress() {
	if a.cancel != nil {
		a.cancel()
	}

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

	//nolint:ll
	_ actor.ActorBehavior[ServerConnMsg, ServerConnResp] = (*ServerConnectionActor)(nil)
)
