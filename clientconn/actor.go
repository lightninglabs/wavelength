package clientconn

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	mailboxconn "github.com/lightninglabs/darepo-client/mailbox/conn"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/tlv"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

// TLV type constants for client connection messages. These are stable
// identifiers used for message serialization and dispatch within the
// per-client durable actor mailbox. They use the 3000 range to avoid
// collision with serverconn's 2000 range.
const (
	// sendEventMsgType is the TLV type for outbound server→client event
	// messages persisted in the per-client DurableActor mailbox.
	sendEventMsgType tlv.Type = 3000

	// sendRPCMsgType is the TLV type for outbound unary RPC envelopes
	// from the per-client UnaryFacade.
	sendRPCMsgType tlv.Type = 3001
)

// TLV record type aliases for RecordT-style message field serialization.
type (
	protoPayloadRecordTLV = tlv.TlvType1
	envelopeRecordTLV     = tlv.TlvType2
	msgIDRecordTLV        = tlv.TlvType3
	idempotencyRecordTLV  = tlv.TlvType4
	clientIDRecordTLV     = tlv.TlvType5
)

// ClientID is a unique identifier for a client.
type ClientID string

// ClientMessage is an interface that server rounds FSM outbox messages must
// implement to send messages to clients. This allows conversion to proto
// messages without creating import cycles.
type ClientMessage interface {
	// ClientID returns the identifier of the client to send the message
	// to.
	ClientID() ClientID

	// ToProto converts the message to a protobuf message that can be
	// sent over gRPC.
	ToProto() proto.Message
}

// ClientConnMsg is the sealed interface for messages that can be sent to the
// ClientsConnBridge. These are typically FSM outbox messages from the rounds
// actor that need to be relayed to clients.
type ClientConnMsg interface {
	actor.Message
	clientsConnMsgSealed()
}

// ClientConnResp is the sealed interface for responses from the
// ClientsConnBridge.
type ClientConnResp interface {
	actor.Message
	clientsConnRespSealed()
}

// SendServerEventRequest wraps a server rounds FSM outbox message and
// requests it be sent to the appropriate client. The bridge will route it
// to the correct per-client DurableActor based on the ClientID.
type SendServerEventRequest struct {
	actor.BaseMessage

	// Message is the server rounds FSM outbox message to send to
	// clients. It must implement the ClientMessage interface which
	// provides the ToProto() method for conversion to protobuf.
	Message ClientMessage
}

// MessageType returns a human-readable type name for logging.
func (m *SendServerEventRequest) MessageType() string {
	return "SendServerEventRequest"
}

// clientsConnMsgSealed implements the ClientConnMsg interface seal.
func (m *SendServerEventRequest) clientsConnMsgSealed() {}

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

// clientsConnRespSealed implements the ClientConnResp interface seal.
func (m *SendClientEventResponse) clientsConnRespSealed() {}

// connectorMsg is the sealed interface for messages internal to the
// per-client connector. These messages flow through the per-client
// DurableActor and must implement TLV serialization for crash-safe
// persistence.
type connectorMsg interface {
	actor.TLVMessage
	connectorMsgSealed()
}

// connectorResp is the sealed interface for responses from the per-client
// connector behavior.
type connectorResp interface {
	actor.Message
	connectorRespSealed()
}

// sendEventMsg wraps a ClientMessage for durable persistence in the
// per-client DurableActor mailbox. The ClientID field is stored for TLV
// replay fidelity (so rawClientMessage.ClientID() returns the correct
// value after deserialization), not for envelope routing — the per-client
// actor uses cfg.RemoteMailboxID for addressing, which is already bound
// to the correct client at construction time.
type sendEventMsg struct {
	actor.BaseMessage

	// Message is the outbound server event to send to the client.
	Message ClientMessage

	// MsgID uniquely identifies this send attempt. When this message is
	// durably persisted and later replayed, the same MsgID is reused to
	// maintain idempotency.
	MsgID string

	// IdempotencyKey identifies the semantic operation for remote dedupe.
	// Retries of the same persisted message must reuse this key.
	IdempotencyKey string

	// clientID is the target client, serialized in TLV for replay.
	clientID ClientID
}

// MessageType returns a human-readable type name for logging.
func (m *sendEventMsg) MessageType() string {
	return "sendEventMsg"
}

// TLVType returns the unique TLV type identifier for this message.
func (m *sendEventMsg) TLVType() tlv.Type {
	return sendEventMsgType
}

// Encode serializes the message to the provided writer. The ClientMessage
// is converted to proto, wrapped in anypb.Any (preserving type
// information), and stored via a WrappedProto TLV record along with the
// clientID, msgID, and idempotencyKey.
func (m *sendEventMsg) Encode(w io.Writer) error {
	anyMsg, err := anypb.New(m.Message.ToProto())
	if err != nil {
		return fmt.Errorf("wrap proto in Any: %w", err)
	}

	// Marshal deterministically for stable ID derivation.
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

	clientIDBytes := []byte(string(m.clientID))

	payload := tlv.NewRecordT[protoPayloadRecordTLV](
		mailboxconn.WrappedProto[*anypb.Any]{Val: anyMsg},
	)
	msgIDRec := tlv.NewPrimitiveRecord[msgIDRecordTLV](
		msgIDBytes,
	)
	idemRec := tlv.NewPrimitiveRecord[idempotencyRecordTLV](
		idempotencyBytes,
	)
	clientIDRec := tlv.NewPrimitiveRecord[clientIDRecordTLV](
		clientIDBytes,
	)

	stream, err := tlv.NewStream(
		payload.Record(), msgIDRec.Record(),
		idemRec.Record(), clientIDRec.Record(),
	)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// Decode deserializes the message from the provided reader. The proto
// payload is stored as a rawClientMessage that lazily unmarshals via the
// global protobuf type registry.
func (m *sendEventMsg) Decode(r io.Reader) error {
	payload := tlv.ZeroRecordT[
		protoPayloadRecordTLV,
		mailboxconn.WrappedProto[*anypb.Any],
	]()
	payload.Val.Val = &anypb.Any{}

	msgIDRec := tlv.ZeroRecordT[msgIDRecordTLV, []byte]()
	idemRec := tlv.ZeroRecordT[idempotencyRecordTLV, []byte]()
	clientIDRec := tlv.ZeroRecordT[clientIDRecordTLV, []byte]()

	stream, err := tlv.NewStream(
		payload.Record(), msgIDRec.Record(),
		idemRec.Record(), clientIDRec.Record(),
	)
	if err != nil {
		return err
	}

	if _, err := stream.DecodeWithParsedTypes(r); err != nil {
		return err
	}

	cid := ClientID(clientIDRec.Val)
	m.Message = &rawClientMessage{
		anyMsg:   payload.Val.Val,
		clientID: cid,
	}
	m.MsgID = string(msgIDRec.Val)
	m.IdempotencyKey = string(idemRec.Val)
	m.clientID = cid

	return nil
}

// connectorMsgSealed implements the connectorMsg interface seal.
func (m *sendEventMsg) connectorMsgSealed() {}

// sendRPCMsg wraps a pre-built outbound unary RPC envelope for durable
// persistence. The UnaryFacade constructs the envelope with all metadata
// (correlation ID, idempotency key, service/method) and hands it to the
// per-client connector for transport via Edge.Send.
//
// NOTE: UnaryFacade currently sends RPCs directly via Edge.Send()
// (synchronous, not crash-safe) by design — callers retry on failure,
// so durable egress is unnecessary. This matches the client-side
// serverconn pattern. The TLV encode/decode and handleSendRPC
// infrastructure is retained for future use if a crash-safe RPC path
// is needed.
type sendRPCMsg struct {
	actor.BaseMessage

	// Envelope is the pre-built mailbox envelope ready for sending.
	Envelope *mailboxpb.Envelope
}

// MessageType returns a human-readable type name for logging.
func (m *sendRPCMsg) MessageType() string {
	return "sendRPCMsg"
}

// TLVType returns the unique TLV type identifier for this message.
func (m *sendRPCMsg) TLVType() tlv.Type {
	return sendRPCMsgType
}

// Encode serializes the message to the provided writer. The mailbox
// envelope is stored via a WrappedProto TLV record.
func (m *sendRPCMsg) Encode(w io.Writer) error {
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
func (m *sendRPCMsg) Decode(r io.Reader) error {
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

// connectorMsgSealed implements the connectorMsg interface seal.
func (m *sendRPCMsg) connectorMsgSealed() {}

// sendEventResp acknowledges that a server→client event was sent via the
// mailbox edge.
type sendEventResp struct {
	actor.BaseMessage

	// Success indicates whether the send operation succeeded.
	Success bool

	// Error contains the error message if the send failed.
	Error string
}

// MessageType returns a human-readable type name for logging.
func (m *sendEventResp) MessageType() string {
	return "sendEventResp"
}

// connectorRespSealed implements the connectorResp interface seal.
func (m *sendEventResp) connectorRespSealed() {}

// sendRPCResp acknowledges that an RPC envelope was sent via the mailbox
// edge.
type sendRPCResp struct {
	actor.BaseMessage

	// Success indicates whether the send operation succeeded.
	Success bool

	// Error contains the error message if the send failed.
	Error string
}

// MessageType returns a human-readable type name for logging.
func (m *sendRPCResp) MessageType() string {
	return "sendRPCResp"
}

// connectorRespSealed implements the connectorResp interface seal.
func (m *sendRPCResp) connectorRespSealed() {}

// rawClientMessage wraps a protobuf Any plus the client identifier for
// reconstructing a ClientMessage after TLV deserialization. The original
// concrete type is recovered using the global protobuf type registry via
// anypb.UnmarshalNew.
type rawClientMessage struct {
	anyMsg   *anypb.Any
	clientID ClientID
}

// ClientID returns the target client identifier stored during TLV decode.
func (m *rawClientMessage) ClientID() ClientID {
	return m.clientID
}

// ToProto reconstructs the original proto message from the stored Any
// wrapper. Returns nil if the type cannot be resolved from the global
// protobuf registry.
func (m *rawClientMessage) ToProto() proto.Message {
	msg, err := m.anyMsg.UnmarshalNew()
	if err != nil {
		// Log at package level using btclog.Disabled as fallback
		// since rawClientMessage has no actor context. This path
		// only triggers on corrupted TLV data, so silent failure
		// is acceptable.
		_ = err

		return nil
	}

	return msg
}

// ClientConnectionActor is the per-client connector boundary for all
// mailbox traffic between the server and a single remote client. It
// serves as both:
//
//  1. An egress actor: receives outbound messages from the bridge (FSM
//     events) and UnaryFacade (RPC requests), then sends them via the
//     mailbox edge to the client's mailbox.
//
//  2. An ingress loop: continuously pulls envelopes from the server's
//     per-client mailbox (messages from the client), dispatches them to
//     local server-side actors via ServiceKey-based routing, and manages
//     the ack watermark state machine to ensure at-least-once delivery
//     with crash safety.
//
// Each registered client gets its own ClientConnectionActor, backed by a
// DurableActor for crash-safe egress. Outbound messages from the rounds
// actor persist in the per-client durable mailbox before processing,
// ensuring no message loss on crashes.
type ClientConnectionActor struct {
	// cfg holds all dependencies and tuning knobs for this client's
	// connector.
	cfg PerClientConfig

	// log is the resolved logger for this actor.
	log btclog.Logger

	// responseRegistry maps correlation IDs to unary RPC waiters and
	// buffers early responses that arrive before a waiter is registered.
	// This is in-memory only.
	responseRegistry *mailboxconn.ResponseRegistry

	// ingressMu guards the ingress lifecycle state (cancelFn and
	// started) to prevent races between StartIngress and
	// StopIngress.
	ingressMu sync.Mutex

	// cancelFn cancels the ingress loop's background context. Nil
	// when the ingress loop is not running.
	cancelFn context.CancelFunc

	// started tracks whether StartIngress has been called. Once
	// true, subsequent calls to StartIngress return an error.
	started bool

	// stopped tracks whether StopIngress has been called. Once
	// true, subsequent calls are no-ops (idempotent shutdown).
	stopped bool

	// wg tracks the ingress loop goroutine for clean shutdown.
	wg sync.WaitGroup
}

// NewClientConnectionActor creates a new per-client connection actor with
// the given configuration. The actor must be started via its DurableActor
// wrapper and the ingress loop must be started separately via
// StartIngress.
func NewClientConnectionActor(
	cfg PerClientConfig,
) *ClientConnectionActor {

	return &ClientConnectionActor{
		cfg: cfg,
		log: cfg.Log.UnwrapOr(btclog.Disabled),
		responseRegistry: mailboxconn.NewResponseRegistry(
			cfg.ResponseWaiterTTL,
		),
	}
}

// Receive processes incoming egress messages. This is called by the
// per-client DurableActor runtime when messages arrive in the actor's
// mailbox.
func (a *ClientConnectionActor) Receive(ctx context.Context,
	msg connectorMsg) fn.Result[connectorResp] {

	switch m := msg.(type) {
	case *sendEventMsg:
		return a.handleSendEvent(ctx, m)

	case *sendRPCMsg:
		return a.handleSendRPC(ctx, m)

	default:
		return fn.Err[connectorResp](fmt.Errorf(
			"unknown message type: %T", msg,
		))
	}
}

// handleSendEvent converts a server FSM outbox message to a proto message
// and sends it to the client via the mailbox edge.
func (a *ClientConnectionActor) handleSendEvent(ctx context.Context,
	req *sendEventMsg) fn.Result[connectorResp] {

	protoMsg := req.Message.ToProto()
	if protoMsg == nil {
		return fn.Err[connectorResp](fmt.Errorf(
			"message ToProto() returned nil for client %q",
			a.cfg.RemoteMailboxID,
		))
	}

	body, err := anypb.New(protoMsg)
	if err != nil {
		return fn.Err[connectorResp](fmt.Errorf(
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
			return fn.Err[connectorResp](fmt.Errorf(
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
		return fn.Err[connectorResp](fmt.Errorf(
			"send server event: %w", err,
		))
	}

	if resp.Status != nil && !resp.Status.Ok {
		return fn.Err[connectorResp](fmt.Errorf(
			"send server event: %s (%s)",
			resp.Status.Message, resp.Status.Code,
		))
	}

	return fn.Ok[connectorResp](&sendEventResp{
		Success: true,
	})
}

// handleSendRPC sends a pre-built unary RPC envelope via the mailbox edge.
func (a *ClientConnectionActor) handleSendRPC(ctx context.Context,
	req *sendRPCMsg) fn.Result[connectorResp] {

	resp, err := a.cfg.Edge.Send(ctx, &mailboxpb.SendRequest{
		Envelope: req.Envelope,
	})
	if err != nil {
		return fn.Err[connectorResp](fmt.Errorf(
			"send rpc request: %w", err,
		))
	}

	if resp.Status != nil && !resp.Status.Ok {
		return fn.Err[connectorResp](fmt.Errorf(
			"send rpc request: %s (%s)",
			resp.Status.Message, resp.Status.Code,
		))
	}

	return fn.Ok[connectorResp](&sendRPCResp{
		Success: true,
	})
}

// RegisterWaiter adds a response waiter for the given correlation ID. The
// returned Future completes when the ingress loop delivers a KIND_RESPONSE
// with a matching correlation ID, or errors if the waiter expires or is
// cancelled.
func (a *ClientConnectionActor) RegisterWaiter(
	id CorrelationID,
) actor.Future[*mailboxpb.Envelope] {

	return a.responseRegistry.RegisterWaiter(id)
}

// removeWaiter removes a previously registered waiter, preventing leaks
// on cancellation or timeout.
func (a *ClientConnectionActor) removeWaiter(id CorrelationID) {
	a.responseRegistry.RemoveWaiter(id)
}

// deliverResponse looks up a waiter by correlation ID and delivers the
// envelope. If no waiter exists yet, the response is buffered so a later
// AwaitRPC call can still observe it.
func (a *ClientConnectionActor) deliverResponse(
	id CorrelationID, env *mailboxpb.Envelope,
) bool {

	return a.responseRegistry.DeliverResponse(id, env)
}

// StartIngress loads the ack checkpoint from the store and launches the
// background ingress loop goroutine. The caller's context is used only
// for the startup checkpoint load; the ingress goroutine itself runs
// under its own cancellable context derived from context.Background()
// so that a request-scoped caller context does not inadvertently kill
// the long-running loop.
//
// StartIngress must be called exactly once. Calling it a second time
// returns an error. If the checkpoint cannot be loaded, an error is
// returned and the loop is not started — the caller should treat this
// as a fatal startup failure.
func (a *ClientConnectionActor) StartIngress(
	ctx context.Context,
) error {

	a.ingressMu.Lock()
	if a.started {
		a.ingressMu.Unlock()

		return fmt.Errorf("ingress already started")
	}
	if a.stopped {
		a.ingressMu.Unlock()

		return fmt.Errorf("ingress already stopped")
	}
	a.started = true
	a.ingressMu.Unlock()

	state, err := a.loadCheckpoint(ctx)
	if err != nil {
		return fmt.Errorf("load ingress checkpoint: %w", err)
	}

	// Use a background-rooted context for the ingress goroutine so
	// its lifetime is tied only to StopIngress, not to the caller's
	// (potentially request-scoped) context.
	ingressCtx, cancel := context.WithCancel(
		context.Background(),
	)

	a.ingressMu.Lock()
	a.cancelFn = cancel
	a.ingressMu.Unlock()

	a.wg.Add(1)
	go a.ingressLoop(ingressCtx, state)

	return nil
}

// StopIngress cancels the ingress loop and waits for it to exit. Safe
// to call multiple times — subsequent calls are no-ops. Also safe to
// call before StartIngress (marks the actor as stopped so a later
// StartIngress returns an error).
func (a *ClientConnectionActor) StopIngress() {
	a.ingressMu.Lock()
	if a.stopped {
		a.ingressMu.Unlock()
		a.wg.Wait()

		return
	}
	a.stopped = true

	if a.cancelFn != nil {
		a.cancelFn()
	}
	a.ingressMu.Unlock()

	a.wg.Wait()
}

// newClientConnCodec creates a MessageCodec with all per-client connector
// message types registered. This codec is used by the per-client
// DurableActor for TLV serialization of messages in the durable mailbox.
func newClientConnCodec() *actor.MessageCodec {
	codec := actor.NewMessageCodec()

	codec.MustRegister(
		sendEventMsgType,
		func() actor.TLVMessage {
			return &sendEventMsg{}
		},
	)

	codec.MustRegister(
		sendRPCMsgType,
		func() actor.TLVMessage {
			return &sendRPCMsg{}
		},
	)

	return codec
}

// Compile-time interface checks.
var (
	_ connectorMsg  = (*sendEventMsg)(nil)
	_ connectorMsg  = (*sendRPCMsg)(nil)
	_ connectorResp = (*sendEventResp)(nil)
	_ connectorResp = (*sendRPCResp)(nil)

	_ ClientConnMsg  = (*SendServerEventRequest)(nil)
	_ ClientConnResp = (*SendClientEventResponse)(nil)

	//nolint:ll
	_ actor.ActorBehavior[connectorMsg, connectorResp] = (*ClientConnectionActor)(nil)
)
