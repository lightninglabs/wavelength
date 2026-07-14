package serverconn

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/baselib/actor"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	"github.com/lightninglabs/wavelength/serverconn/hellotestpb"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

// helloStartedMsg is the actor-local representation of a server-pushed
// HelloStartedEvent. It implements InboundActorMessage so it can be
// registered with NewEventRoute for automatic FromProto dispatch.
type helloStartedMsg struct {
	actor.BaseMessage

	// SessionID identifies the greeting session.
	SessionID string
}

// MessageType returns a human-readable type name for logging.
func (m *helloStartedMsg) MessageType() string { return "HelloStartedMsg" }

// FromProto populates the message from a deserialized HelloStartedEvent.
func (m *helloStartedMsg) FromProto(p proto.Message) error {
	ev, ok := p.(*hellotestpb.HelloStartedEvent)
	if !ok {
		return fmt.Errorf("unexpected proto type: %T", p)
	}

	m.SessionID = ev.SessionId

	return nil
}

// joinGreetingServerMsg wraps a JoinGreetingRequest for outbound durable
// delivery. It implements ServerMessage so it can be sent via
// SendClientEventRequest through the DurableActor.
type joinGreetingServerMsg struct {
	actor.BaseMessage

	// SessionID identifies the greeting session to join.
	SessionID string
}

// ServiceMethod returns test routing metadata for the greeting service.
func (m *joinGreetingServerMsg) ServiceMethod() mailboxrpc.ServiceMethod {
	return mailboxrpc.ServiceMethod{
		Service: "hellotest.v1.HelloService",
		Method:  "JoinGreeting",
	}
}

// ToProto converts to a proto message for mailbox envelope transport.
func (m *joinGreetingServerMsg) ToProto() fn.Result[proto.Message] {
	return fn.Ok[proto.Message](&hellotestpb.JoinGreetingRequest{
		SessionId: m.SessionID,
	})
}

// Compile-time interface checks.
var (
	_ InboundServerMessage = (*helloStartedMsg)(nil)
	_ ServerMessage        = (*joinGreetingServerMsg)(nil)
)

// greetingBehavior is a trivial actor behavior that records received
// helloStartedMsg messages on a channel for test assertions.
type greetingBehavior struct {
	received chan *helloStartedMsg
}

// Receive processes a single helloStartedMsg by forwarding it to the
// test's observation channel.
func (b *greetingBehavior) Receive(
	ctx context.Context, msg *helloStartedMsg,
) fn.Result[struct{}] {

	select {
	case b.received <- msg:
	case <-ctx.Done():
		return fn.Err[struct{}](ctx.Err())
	}

	return fn.Ok(struct{}{})
}

// testServer simulates the remote server side of a mailbox connection. It
// polls the server-side mailbox for inbound client envelopes and dispatches
// them: KIND_REQUEST envelopes go through a ServeMux, KIND_EVENT envelopes
// are recorded on the received channel.
type testServer struct {
	mb              *inMemoryMailbox
	mux             *mailboxrpc.ServeMux
	serverMailboxID string

	// received tracks fire-and-forget events delivered to the server.
	received chan *mailboxpb.Envelope
}

// newTestServer creates a server simulator backed by the given mailbox.
func newTestServer(
	mb *inMemoryMailbox, serverMailboxID string,
) *testServer {

	return &testServer{
		mb:              mb,
		mux:             mailboxrpc.NewServeMux(),
		serverMailboxID: serverMailboxID,
		received:        make(chan *mailboxpb.Envelope, 20),
	}
}

// run polls the server mailbox and dispatches incoming envelopes until
// ctx is cancelled.
func (s *testServer) run(ctx context.Context) {
	var cursor uint64

	for {
		select {
		case <-ctx.Done():
			return

		default:
		}

		envs, next, status := s.mb.pull(
			ctx, s.serverMailboxID, cursor, 10, 50*time.Millisecond,
		)
		if !status.Ok || len(envs) == 0 {
			continue
		}

		cursor = next

		for _, env := range envs {
			if env.Rpc == nil {
				continue
			}

			switch env.Rpc.Kind {
			case mailboxpb.RpcMeta_KIND_REQUEST:
				s.handleRequest(ctx, env)

			case mailboxpb.RpcMeta_KIND_EVENT:
				select {
				case s.received <- env:
				default:
				}

			// The test server only cares about REQUEST/EVENT; the
			// other KIND_* values are ignored.
			case mailboxpb.RpcMeta_KIND_UNSPECIFIED,
				mailboxpb.RpcMeta_KIND_RESPONSE:
			}
		}
	}
}

// handleRequest dispatches a KIND_REQUEST envelope through the ServeMux
// and sends the response envelope back to the client's ReplyTo mailbox.
func (s *testServer) handleRequest(ctx context.Context,
	env *mailboxpb.Envelope) {

	if env.Body == nil {
		return
	}

	respMsg, err := s.mux.ServeRPC(
		ctx, env.Rpc.Service, env.Rpc.Method, env.Body.Value,
	)

	var (
		body    *anypb.Any
		headers map[string]string
	)

	if err != nil {
		// Transport the error via grpc_status headers so the client
		// can surface it as a gRPC status error.
		headers = mailboxrpc.EncodeErrorHeaders(err)
		body = &anypb.Any{}
	} else if body, err = anypb.New(respMsg); err != nil {
		// If wrapping the response fails (e.g., unregistered type
		// URL), surface it as a server-side Internal error so the
		// client sees a clear gRPC failure rather than garbled
		// bytes.
		headers = mailboxrpc.EncodeErrorHeaders(
			fmt.Errorf("wrap response in Any: %w", err),
		)
		body = &anypb.Any{}
	}

	responseEnv := &mailboxpb.Envelope{
		ProtocolVersion:    1,
		ArkProtocolVersion: 1,
		Sender:             s.serverMailboxID,
		Recipient:          env.Rpc.ReplyTo,
		Headers:            headers,
		Body:               body,
		Rpc: &mailboxpb.RpcMeta{
			Kind:          mailboxpb.RpcMeta_KIND_RESPONSE,
			CorrelationId: env.Rpc.CorrelationId,
			Service:       env.Rpc.Service,
			Method:        env.Rpc.Method,
		},
	}

	s.mb.send(responseEnv)
}

// pushEvent injects a KIND_EVENT envelope into the client's mailbox.
func (s *testServer) pushEvent(t *testing.T, recipientID, service,
	method string, event proto.Message) {

	t.Helper()

	body, err := anypb.New(event)
	require.NoError(t, err)

	env := &mailboxpb.Envelope{
		ProtocolVersion:    1,
		ArkProtocolVersion: 1,
		Sender:             s.serverMailboxID,
		Recipient:          recipientID,
		Body:               body,
		Rpc: &mailboxpb.RpcMeta{
			Kind:    mailboxpb.RpcMeta_KIND_EVENT,
			Service: service,
			Method:  method,
			ReplyTo: s.serverMailboxID,
		},
	}

	status := s.mb.send(env)
	require.True(t, status.Ok, "push event failed: %s", status.Message)
}

// helloServer implements the generated HelloServiceMailboxServer interface.
type helloServer struct{}

// SayHello echoes a greeting containing the caller's name.
func (s *helloServer) SayHello(_ context.Context,
	req *hellotestpb.HelloRequest) (*hellotestpb.HelloResponse, error) {

	return &hellotestpb.HelloResponse{
		Greeting: fmt.Sprintf("Hello, %s!", req.Name),
	}, nil
}

// SayGoodbye echoes a farewell containing the caller's name.
func (s *helloServer) SayGoodbye(_ context.Context,
	req *hellotestpb.GoodbyeRequest) (*hellotestpb.GoodbyeResponse, error) {

	return &hellotestpb.GoodbyeResponse{
		Farewell: fmt.Sprintf("Goodbye, %s!", req.Name),
	}, nil
}

// TestE2EUnaryRPC verifies a full unary request/response round-trip through
// the mailbox transport. The client sends SayHello via the generated
// HelloServiceMailboxClient, the server simulator dispatches it through
// ServeMux, and the client receives the typed response.
func TestE2EUnaryRPC(t *testing.T) {
	t.Parallel()

	mb := newInMemoryMailbox()
	store := newMemCheckpointStore()

	// Server side: register the HelloService handler.
	server := newTestServer(mb, "server-1")
	hellotestpb.RegisterHelloServiceMailboxServer(
		server.mux, &helloServer{},
	)

	cfg := newTestConnectorConfig(mb, store)

	runtime, err := NewRuntime(cfg)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	go server.run(ctx)
	require.NoError(t, runtime.Start(ctx))
	defer runtime.Stop()

	// Issue a unary SayHello RPC via the generated mailbox client.
	client := hellotestpb.NewHelloServiceMailboxClient(runtime.Unary())

	resp, err := client.SayHello(ctx, &hellotestpb.HelloRequest{
		Name: "Alice",
	})
	require.NoError(t, err)
	require.Equal(t, "Hello, Alice!", resp.Greeting)

	// Also exercise SayGoodbye to confirm independent method routing.
	goodbye, err := client.SayGoodbye(ctx, &hellotestpb.GoodbyeRequest{
		Name: "Bob",
	})
	require.NoError(t, err)
	require.Equal(t, "Goodbye, Bob!", goodbye.Farewell)
}

// TestE2EServerPushEvent verifies that a server-pushed KIND_EVENT envelope
// is routed through the EventRouter to a registered greeting actor. The
// test creates a full actor system, registers a greeting actor under a
// ServiceKey, and verifies that the actor receives the deserialized
// helloStartedMsg via the FromProto interface.
func TestE2EServerPushEvent(t *testing.T) {
	t.Parallel()

	mb := newInMemoryMailbox()
	store := newMemCheckpointStore()
	system := actor.NewActorSystem()

	// Register a greeting actor to receive server push events.
	greetingKey := actor.NewServiceKey[*helloStartedMsg, struct{}](
		"greeting-actor",
	)
	behavior := &greetingBehavior{
		received: make(chan *helloStartedMsg, 10),
	}
	actor.RegisterWithSystem(
		system, "greeting-1", greetingKey, behavior,
	)

	// Wire up the EventRouter with the InboundServerMessage-based
	// helper. NewEventRoute auto-generates the Adapt function from
	// helloStartedMsg.FromProto.
	router := NewEventRouter(system)
	NewEventRoute(
		router, InboundEventRouteConfig[*helloStartedMsg, struct{}]{
			Service: "hellotest.v1.HelloService",
			Method:  "HelloStarted",
			Key:     greetingKey,
			NewEvent: func() proto.Message {
				return &hellotestpb.HelloStartedEvent{}
			},
			NewMsg: func() *helloStartedMsg {
				return &helloStartedMsg{}
			},
		})

	server := newTestServer(mb, "server-1")

	cfg := newTestConnectorConfig(mb, store)

	cfg.Dispatchers = router.AsDispatcherMap()

	runtime, err := NewRuntime(cfg)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	go server.run(ctx)
	require.NoError(t, runtime.Start(ctx))
	defer runtime.Stop()

	// Server pushes a HelloStartedEvent to the client.
	server.pushEvent(
		t, "client-1", "hellotest.v1.HelloService", "HelloStarted",
		&hellotestpb.HelloStartedEvent{
			SessionId: "session-42",
		},
	)

	// Wait for the greeting actor to receive the dispatched message.
	select {
	case msg := <-behavior.received:
		require.Equal(t, "session-42", msg.SessionID)

	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for greeting actor to receive event")
	}
}

// TestE2EClientFireAndForget verifies that the client can send a
// fire-and-forget event to the server via the DurableActor egress path.
// The test sends a JoinGreetingRequest through SendClientEventRequest,
// which is durably persisted in the actor's mailbox, serialized to proto
// via ToProto, and sent as a KIND_EVENT envelope to the server.
func TestE2EClientFireAndForget(t *testing.T) {
	t.Parallel()

	mb := newInMemoryMailbox()
	store := newMemCheckpointStore()
	server := newTestServer(mb, "server-1")

	cfg := newTestConnectorConfig(mb, store)

	runtime, err := NewRuntime(cfg)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	go server.run(ctx)
	require.NoError(t, runtime.Start(ctx))
	defer runtime.Stop()

	// Client sends a fire-and-forget event via the DurableActor. The
	// message is persisted in the actor mailbox before the DurableActor
	// processes it and sends it to the server via Edge.Send.
	err = runtime.TellRef().Tell(ctx, &SendClientEventRequest{
		Message: &joinGreetingServerMsg{SessionID: "greeting-99"},
	})
	require.NoError(t, err)

	// Wait for the server to receive the event envelope.
	select {
	case env := <-server.received:
		require.NotNil(t, env.Rpc)
		require.Equal(t,
			mailboxpb.RpcMeta_KIND_EVENT, env.Rpc.Kind,
		)

		// Unmarshal the body to verify the JoinGreetingRequest
		// arrived intact.
		var joinReq hellotestpb.JoinGreetingRequest
		err := proto.Unmarshal(env.Body.Value, &joinReq)
		require.NoError(t, err)
		require.Equal(t, "greeting-99", joinReq.SessionId)

	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for server to receive event")
	}
}

// TestE2EUnaryAndPush verifies a combined scenario where the client
// issues a unary RPC and also receives server-push events in the same
// session. This exercises concurrent ingress (response delivery +
// event dispatch) with the EventRouter and UnaryFacade both active.
func TestE2EUnaryAndPush(t *testing.T) {
	t.Parallel()

	mb := newInMemoryMailbox()
	store := newMemCheckpointStore()
	system := actor.NewActorSystem()

	// Register greeting actor.
	greetingKey := actor.NewServiceKey[*helloStartedMsg, struct{}](
		"greeting-actor",
	)
	behavior := &greetingBehavior{
		received: make(chan *helloStartedMsg, 10),
	}
	actor.RegisterWithSystem(
		system, "greeting-1", greetingKey, behavior,
	)

	// Wire EventRouter.
	router := NewEventRouter(system)
	NewEventRoute(
		router, InboundEventRouteConfig[*helloStartedMsg, struct{}]{
			Service: "hellotest.v1.HelloService",
			Method:  "HelloStarted",
			Key:     greetingKey,
			NewEvent: func() proto.Message {
				return &hellotestpb.HelloStartedEvent{}
			},
			NewMsg: func() *helloStartedMsg {
				return &helloStartedMsg{}
			},
		})

	// Server with HelloService handler.
	server := newTestServer(mb, "server-1")
	hellotestpb.RegisterHelloServiceMailboxServer(
		server.mux, &helloServer{},
	)

	cfg := newTestConnectorConfig(mb, store)

	cfg.Dispatchers = router.AsDispatcherMap()

	runtime, err := NewRuntime(cfg)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	go server.run(ctx)
	require.NoError(t, runtime.Start(ctx))
	defer runtime.Stop()

	// Phase 1: Server pushes an event before the client sends an RPC.
	server.pushEvent(
		t, "client-1", "hellotest.v1.HelloService", "HelloStarted",
		&hellotestpb.HelloStartedEvent{
			SessionId: "pre-rpc",
		},
	)

	select {
	case msg := <-behavior.received:
		require.Equal(t, "pre-rpc", msg.SessionID)

	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for pre-RPC push event")
	}

	// Phase 2: Client issues a unary SayHello RPC.
	client := hellotestpb.NewHelloServiceMailboxClient(runtime.Unary())

	resp, err := client.SayHello(ctx, &hellotestpb.HelloRequest{
		Name: "Charlie",
	})
	require.NoError(t, err)
	require.Equal(t, "Hello, Charlie!", resp.Greeting)

	// Phase 3: Server pushes another event after the RPC.
	server.pushEvent(
		t, "client-1", "hellotest.v1.HelloService", "HelloStarted",
		&hellotestpb.HelloStartedEvent{
			SessionId: "post-rpc",
		},
	)

	select {
	case msg := <-behavior.received:
		require.Equal(t, "post-rpc", msg.SessionID)

	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for post-RPC push event")
	}
}

// errHelloServer implements HelloServiceMailboxServer with handlers that
// return gRPC status errors so the error-header transport path can be
// exercised end-to-end.
type errHelloServer struct{}

// SayHello returns a gRPC NotFound error for any request.
func (s *errHelloServer) SayHello(_ context.Context,
	_ *hellotestpb.HelloRequest) (*hellotestpb.HelloResponse, error) {

	return nil, status.Errorf(codes.NotFound, "user not found")
}

// SayGoodbye returns a gRPC InvalidArgument error for any request.
func (s *errHelloServer) SayGoodbye(_ context.Context,
	_ *hellotestpb.GoodbyeRequest) (*hellotestpb.GoodbyeResponse, error) {

	return nil, status.Errorf(codes.InvalidArgument, "name is required")
}

// TestE2EUnaryRPCError verifies that a server-side gRPC error is
// transported through the mailbox envelope headers and surfaced to the
// client as a proper gRPC status error via AwaitRPC. This exercises the
// EncodeErrorHeaders → HeaderGRPCStatusB64 → DecodeErrorHeaders path
// end-to-end.
func TestE2EUnaryRPCError(t *testing.T) {
	t.Parallel()

	mb := newInMemoryMailbox()
	store := newMemCheckpointStore()

	// Server side: register a handler that always returns errors.
	server := newTestServer(mb, "server-1")
	hellotestpb.RegisterHelloServiceMailboxServer(
		server.mux, &errHelloServer{},
	)

	cfg := newTestConnectorConfig(mb, store)

	runtime, err := NewRuntime(cfg)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	go server.run(ctx)
	require.NoError(t, runtime.Start(ctx))
	defer runtime.Stop()

	client := hellotestpb.NewHelloServiceMailboxClient(runtime.Unary())

	// SayHello should surface a NotFound gRPC error.
	_, helloErr := client.SayHello(ctx, &hellotestpb.HelloRequest{
		Name: "Alice",
	})
	require.Error(t, helloErr)

	st, ok := status.FromError(helloErr)
	require.True(t, ok, "expected gRPC status error, got: %v", helloErr)
	require.Equal(t, codes.NotFound, st.Code())
	require.Contains(t, st.Message(), "user not found")

	// SayGoodbye should surface an InvalidArgument gRPC error.
	_, goodbyeErr := client.SayGoodbye(
		ctx, &hellotestpb.GoodbyeRequest{
			Name: "Bob",
		},
	)
	require.Error(t, goodbyeErr)

	st, ok = status.FromError(goodbyeErr)
	require.True(t, ok, "expected gRPC status error, got: %v", goodbyeErr)
	require.Equal(t, codes.InvalidArgument, st.Code())
	require.Contains(t, st.Message(), "name is required")
}
