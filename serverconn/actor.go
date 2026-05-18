package serverconn

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	mailboxconn "github.com/lightninglabs/darepo-client/mailbox/conn"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/darepo-client/mailbox/rpc"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

// defaultSendEventTimeout is the timeout for outbound gRPC Edge.Send calls.
const defaultSendEventTimeout = 30 * time.Second

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

// ServerConnMsg is the sealed interface for messages that can be sent to the
// ServerConnectionActor. These are typically FSM outbox messages from the
// client that need to be relayed to the server.
type ServerConnMsg interface {
	actor.Message

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
	// Retries of the same SQL-owned request must reuse this key.
	IdempotencyKey string

	// Service is the fully-qualified protobuf service name for mailbox
	// routing (e.g. "round.v1.RoundService"). Populated from
	// ServerMessage.ServiceMethod() at send time when empty.
	Service string

	// Method is the RPC method name for mailbox routing (e.g.
	// "JoinRound"). Populated from ServerMessage.ServiceMethod() at
	// send time when empty.
	Method string
}

// MessageType returns a human-readable type name for logging.
func (m *SendClientEventRequest) MessageType() string {
	return "SendClientEventRequest"
}

// CorrelationKey forwards the inner ServerMessage's per-key FIFO key so
// the SQL mailbox's per-correlation-key claim invariant fires on the
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

	return ""
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

// serverConnMsgSealed implements the ServerConnMsg interface seal.
func (m *SendRPCRequest) serverConnMsgSealed() {}

// SendUnaryRequest wraps a typed unary RPC request for delivery via the
// mailbox edge. Unlike SendRPCRequest, the caller provides the request body
// and routing metadata rather than a pre-built envelope.
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
// Crash-safe egress is provided by SQL mailbox_egress rows owned by the
// transport runtime.
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
}

// NewServerConnectionActor creates a new server connection actor with the
// given configuration. The ingress loop must be started separately via
// StartIngress.
func NewServerConnectionActor(
	cfg ConnectorConfig,
) *ServerConnectionActor {

	return &ServerConnectionActor{
		cfg: cfg,
		log: cfg.Log.UnwrapOr(btclog.Disabled),
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

	case *SendUnaryRequest:
		return a.handleSendUnaryRequest(ctx, m)

	case DurableUnaryQuery:
		unary, buildErr := a.buildDurableUnary(ctx, m)
		if buildErr != nil {
			return fn.Err[ServerConnResp](buildErr)
		}

		return a.handleSendUnaryRequest(ctx, unary)

	case *SendRPCRequest:
		return a.handleSendRPCRequest(ctx, m)

	default:
		return fn.Err[ServerConnResp](
			fmt.Errorf("unknown message type: %T", msg),
		)
	}
}

// handleSendClientEvent converts a client FSM outbox message to a proto
// message and sends it to the server via the mailbox edge.
func (a *ServerConnectionActor) handleSendClientEvent(ctx context.Context,
	req *SendClientEventRequest) fn.Result[ServerConnResp] {

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
	// On SQL replay (both IDs already set), this marshal is skipped.
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
		ProtocolVersion: a.cfg.ProtocolVersion,
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
	if err != nil {
		return fn.Err[ServerConnResp](
			fmt.Errorf("send client event: %w", err),
		)
	}

	if resp.Status != nil && !resp.Status.Ok {
		return fn.Err[ServerConnResp](
			fmt.Errorf("send client event: %s (%s)",
				resp.Status.Message, resp.Status.Code),
		)
	}

	a.lastSendNano.Store(time.Now().UnixNano())

	return fn.Ok[ServerConnResp](&SendClientEventResponse{
		Success: true,
	})
}

// handleSendUnaryRequest sends a correlated unary request via the mailbox
// edge.
func (a *ServerConnectionActor) handleSendUnaryRequest(ctx context.Context,
	req *SendUnaryRequest) fn.Result[ServerConnResp] {

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
		ctx, req.Body, req.Service, req.Method, req.CorrelationID,
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
	body *anypb.Any, service string, method string, correlationID string,
	msgID string, idempotencyKey string) fn.Result[ServerConnResp] {

	envelope := &mailboxpb.Envelope{
		ProtocolVersion: a.cfg.ProtocolVersion,
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
	if err != nil {
		return fn.Err[ServerConnResp](
			fmt.Errorf("send unary request: %w", err),
		)
	}

	if resp.Status != nil && !resp.Status.Ok {
		return fn.Err[ServerConnResp](
			fmt.Errorf("send unary request: %s (%s)",
				resp.Status.Message, resp.Status.Code),
		)
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
	req *SendRPCRequest) fn.Result[ServerConnResp] {

	resp, err := a.cfg.Edge.Send(ctx, &mailboxpb.SendRequest{
		Envelope: req.Envelope,
	})
	if err != nil {
		return fn.Err[ServerConnResp](
			fmt.Errorf("send rpc request: %w", err),
		)
	}

	if resp.Status != nil && !resp.Status.Ok {
		return fn.Err[ServerConnResp](
			fmt.Errorf("send rpc request: %s (%s)",
				resp.Status.Message, resp.Status.Code),
		)
	}

	a.lastSendNano.Store(time.Now().UnixNano())

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

	state, err := a.loadCheckpoint(ctx)
	if err != nil {
		return fmt.Errorf("load ingress checkpoint: %w", err)
	}

	ingressCtx, cancel := context.WithCancel(ctx)

	a.wg.Add(2)
	a.cancelCh <- cancel
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
	_ actor.ActorBehavior[ServerConnMsg, ServerConnResp] = (*ServerConnectionActor)(nil)
)
