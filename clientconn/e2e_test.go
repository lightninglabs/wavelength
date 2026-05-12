package clientconn

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/lightninglabs/darepo-client/baselib/actor"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/darepo-client/mailbox/rpc"
	"github.com/lightninglabs/darepo/clientconn/roundtestpb"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"pgregory.net/rapid"
)

// ---------------------------------------------------------------------------
// Test message types
// ---------------------------------------------------------------------------

// roundStartedMsg is the server-side actor-local representation of a
// client-pushed RoundStartedEvent. It implements InboundActorMessage so it
// can be registered with NewEventRoute for automatic FromProto dispatch.
type roundStartedMsg struct {
	actor.BaseMessage

	// RoundID identifies the started round.
	RoundID string
}

// MessageType returns a human-readable type name for logging.
func (m *roundStartedMsg) MessageType() string {
	return "RoundStartedMsg"
}

// FromProto populates the message from a deserialized RoundStartedEvent.
func (m *roundStartedMsg) FromProto(p proto.Message) error {
	ev, ok := p.(*roundtestpb.RoundStartedEvent)
	if !ok {
		return fmt.Errorf("unexpected proto type: %T", p)
	}

	m.RoundID = ev.RoundId

	return nil
}

// clientJoinedMsg is the server-side actor-local representation of a
// client-pushed ClientJoinedEvent. It implements InboundActorMessage so the
// EventRouter can dispatch it to a server-side actor.
type clientJoinedMsg struct {
	actor.BaseMessage

	// ClientIDVal identifies the joining client.
	ClientIDVal string

	// RoundID identifies the round being joined.
	RoundID string
}

// MessageType returns a human-readable type name for logging.
func (m *clientJoinedMsg) MessageType() string {
	return "ClientJoinedMsg"
}

// FromProto populates the message from a deserialized ClientJoinedEvent.
func (m *clientJoinedMsg) FromProto(p proto.Message) error {
	ev, ok := p.(*roundtestpb.ClientJoinedEvent)
	if !ok {
		return fmt.Errorf("unexpected proto type: %T", p)
	}

	m.ClientIDVal = ev.ClientId
	m.RoundID = ev.RoundId

	return nil
}

// roundStartedServerMsg wraps a RoundStartedEvent for outbound delivery
// from the server to a specific client. It implements ClientMessage so it
// can be sent via SendServerEventRequest through the bridge.
type roundStartedServerMsg struct {
	actor.BaseMessage

	// clientID identifies the target client.
	targetClientID ClientID

	// RoundID identifies the started round.
	RoundID string
}

// ClientID returns the target client identifier.
func (m *roundStartedServerMsg) ClientID() ClientID {
	return m.targetClientID
}

// ToProto converts to a proto message for mailbox envelope transport.
func (m *roundStartedServerMsg) ToProto() proto.Message {
	return &roundtestpb.RoundStartedEvent{
		RoundId: m.RoundID,
	}
}

// ServiceMethod returns the routing key for client-side ingress
// dispatch.
func (m *roundStartedServerMsg) ServiceMethod() mailboxrpc.ServiceMethod {
	return mailboxrpc.ServiceMethod{
		Service: "hellotest.v1.HelloService",
		Method:  "RoundStarted",
	}
}

// Compile-time interface checks.
var (
	_ InboundActorMessage = (*roundStartedMsg)(nil)
	_ InboundActorMessage = (*clientJoinedMsg)(nil)
	_ ClientMessage       = (*roundStartedServerMsg)(nil)
)

// ---------------------------------------------------------------------------
// Test actor behaviors
// ---------------------------------------------------------------------------

// clientJoinedBehavior is a trivial actor behavior that appends received
// clientJoinedMsg messages to a transcript for test assertions.
type clientJoinedBehavior struct {
	received *transcript[*clientJoinedMsg]
}

// Receive appends the incoming clientJoinedMsg to the transcript.
func (b *clientJoinedBehavior) Receive(
	_ context.Context, msg *clientJoinedMsg,
) fn.Result[struct{}] {

	b.received.append(msg)

	return fn.Ok(struct{}{})
}

// ---------------------------------------------------------------------------
// Test client simulator
// ---------------------------------------------------------------------------

// testClient simulates the remote client side of a mailbox connection. It
// polls the client-side mailbox for inbound server envelopes and dispatches
// them: KIND_REQUEST envelopes go through a ServeMux, KIND_EVENT envelopes
// are appended to the received transcript.
type testClient struct {
	mb            *inMemoryMailbox
	mux           *mailboxrpc.ServeMux
	clientMailbox string
	serverMailbox string

	// received is a transcript of server-pushed events delivered to
	// the client.
	received *transcript[*mailboxpb.Envelope]
}

// newTestClient creates a client simulator backed by the given mailbox.
func newTestClient(
	mb *inMemoryMailbox, clientMailbox, serverMailbox string,
) *testClient {

	return &testClient{
		mb:            mb,
		mux:           mailboxrpc.NewServeMux(),
		clientMailbox: clientMailbox,
		serverMailbox: serverMailbox,
		received:      &transcript[*mailboxpb.Envelope]{},
	}
}

// run polls the client mailbox and dispatches incoming envelopes until ctx
// is cancelled.
func (c *testClient) run(ctx context.Context) {
	var cursor uint64

	for {
		select {
		case <-ctx.Done():
			return

		default:
		}

		envs, next, st := c.mb.pull(
			ctx, c.clientMailbox, cursor, 10, 50*time.Millisecond,
		)
		if !st.Ok || len(envs) == 0 {
			continue
		}

		cursor = next

		for _, env := range envs {
			if env.Rpc == nil {
				continue
			}

			switch env.Rpc.Kind {
			case mailboxpb.RpcMeta_KIND_REQUEST:
				c.handleRequest(ctx, env)

			case mailboxpb.RpcMeta_KIND_EVENT:
				c.received.append(env)
			}
		}
	}
}

// handleRequest dispatches a KIND_REQUEST envelope through the ServeMux
// and sends the response envelope back to the server's ReplyTo mailbox.
func (c *testClient) handleRequest(ctx context.Context,
	env *mailboxpb.Envelope) {

	if env.Body == nil {
		return
	}

	respMsg, err := c.mux.ServeRPC(
		ctx, env.Rpc.Service, env.Rpc.Method, env.Body.Value,
	)

	var (
		body    *anypb.Any
		headers map[string]string
	)

	if err != nil {
		// Transport the error via grpc_status headers so the
		// server can surface it as a gRPC status error.
		headers = mailboxrpc.EncodeErrorHeaders(err)
		body = &anypb.Any{}
	} else if body, err = anypb.New(respMsg); err != nil {
		// If wrapping the response fails, surface as Internal.
		headers = mailboxrpc.EncodeErrorHeaders(
			fmt.Errorf("wrap response in Any: %w", err),
		)
		body = &anypb.Any{}
	}

	responseEnv := &mailboxpb.Envelope{
		ProtocolVersion: 1,
		Sender:          c.clientMailbox,
		Recipient:       env.Rpc.ReplyTo,
		Headers:         headers,
		Body:            body,
		Rpc: &mailboxpb.RpcMeta{
			Kind:          mailboxpb.RpcMeta_KIND_RESPONSE,
			CorrelationId: env.Rpc.CorrelationId,
			Service:       env.Rpc.Service,
			Method:        env.Rpc.Method,
		},
	}

	c.mb.send(responseEnv)
}

// pushEvent injects a KIND_EVENT envelope from the client into the
// server's per-client mailbox.
func (c *testClient) pushEvent(t *testing.T, service, method string,
	event proto.Message) {

	t.Helper()

	body, err := anypb.New(event)
	require.NoError(t, err)

	env := &mailboxpb.Envelope{
		ProtocolVersion: 1,
		Sender:          c.clientMailbox,
		Recipient:       c.serverMailbox,
		Body:            body,
		Rpc: &mailboxpb.RpcMeta{
			Kind:    mailboxpb.RpcMeta_KIND_EVENT,
			Service: service,
			Method:  method,
			ReplyTo: c.clientMailbox,
		},
	}

	st := c.mb.send(env)
	require.True(t, st.Ok, "push event failed: %s", st.Message)
}

// ---------------------------------------------------------------------------
// Test RPC server implementations
// ---------------------------------------------------------------------------

// roundNotifyServer implements RoundNotifyServiceMailboxServer for the
// test client. It echoes back acknowledgments for incoming RPCs.
type roundNotifyServer struct{}

// NotifyRoundStarted acknowledges the round started notification.
func (s *roundNotifyServer) NotifyRoundStarted(_ context.Context,
	req *roundtestpb.RoundStartedNotification) (
	*roundtestpb.RoundStartedAck, error) {

	return &roundtestpb.RoundStartedAck{}, nil
}

// NotifyBatchReady acknowledges the batch ready notification.
func (s *roundNotifyServer) NotifyBatchReady(_ context.Context,
	req *roundtestpb.BatchReadyNotification) (*roundtestpb.BatchReadyAck,
	error) {

	return &roundtestpb.BatchReadyAck{}, nil
}

// errRoundNotifyServer implements RoundNotifyServiceMailboxServer with
// handlers that return gRPC status errors.
type errRoundNotifyServer struct{}

// NotifyRoundStarted returns a gRPC NotFound error for any request.
func (s *errRoundNotifyServer) NotifyRoundStarted(_ context.Context,
	_ *roundtestpb.RoundStartedNotification) (*roundtestpb.RoundStartedAck,
	error) {

	return nil, status.Errorf(codes.NotFound, "round not found")
}

// NotifyBatchReady returns a gRPC InvalidArgument error for any request.
func (s *errRoundNotifyServer) NotifyBatchReady(_ context.Context,
	_ *roundtestpb.BatchReadyNotification) (*roundtestpb.BatchReadyAck,
	error) {

	return nil, status.Errorf(codes.InvalidArgument, "batch_data is "+
		"required")
}

// ---------------------------------------------------------------------------
// E2E Tests
// ---------------------------------------------------------------------------

// TestE2EServerToClientEvent verifies the full egress path: server pushes
// a RoundStartedEvent to a client via the clientconn bridge. The test
// client receives the KIND_EVENT envelope on its mailbox. This exercises:
// bridge.Tell → per-client DurableActor → Edge.Send → client mailbox.
func TestE2EServerToClientEvent(t *testing.T) {
	t.Parallel()

	mb := newInMemoryMailbox()
	store := newMemCheckpointStore()
	cfg := newTestPerClientConfig(mb, store)

	bridge := NewClientsConnBridge()
	clientID := ClientID("client-1")

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	runtime, err := bridge.RegisterClient(ctx, clientID, cfg)
	require.NoError(t, err)
	defer bridge.Stop()

	// Start a test client simulator pulling from the client mailbox.
	tc := newTestClient(mb, "client-1", "server-for-client-1")
	go tc.run(ctx)

	// Server pushes a RoundStartedEvent via the bridge.
	err = bridge.Tell(ctx, &SendServerEventRequest{
		Message: &roundStartedServerMsg{
			targetClientID: clientID,
			RoundID:        "round-42",
		},
	})
	require.NoError(t, err)

	// Poll the transcript until the test client receives the event.
	require.Eventually(t, func() bool {
		return tc.received.entryCount() >= 1
	}, 5*time.Second, 50*time.Millisecond)

	env := tc.received.all()[0]
	require.NotNil(t, env.Rpc)
	require.Equal(t,
		mailboxpb.RpcMeta_KIND_EVENT, env.Rpc.Kind,
	)

	// Verify the body contains a RoundStartedEvent.
	var event roundtestpb.RoundStartedEvent
	err = proto.Unmarshal(env.Body.Value, &event)
	require.NoError(t, err)
	require.Equal(t, "round-42", event.RoundId)

	_ = runtime
}

// TestE2EClientToServerEvent verifies the ingress path: a client sends a
// ClientJoinedEvent to the server via the server's per-client mailbox. The
// server-side ingress loop pulls the envelope and dispatches it to a
// registered server actor via the EventRouter.
func TestE2EClientToServerEvent(t *testing.T) {
	t.Parallel()

	mb := newInMemoryMailbox()
	store := newMemCheckpointStore()
	system := actor.NewActorSystem()

	// Register a server-side actor to receive client events.
	joinKey := actor.NewServiceKey[*clientJoinedMsg, struct{}](
		"join-actor",
	)
	behavior := &clientJoinedBehavior{
		received: &transcript[*clientJoinedMsg]{},
	}
	actor.RegisterWithSystem(
		system, "join-1", joinKey, behavior,
	)

	// Wire EventRouter for ClientJoinedEvent dispatch.
	router := NewEventRouter(system)
	NewEventRoute(
		router,
		InboundEventRouteConfig[*clientJoinedMsg, struct{}]{
			Service: "roundtest.v1.RoundNotifyService",
			Method:  "ClientJoined",
			Key:     joinKey,
			NewEvent: func() proto.Message {
				return &roundtestpb.ClientJoinedEvent{}
			},
			NewMsg: func() *clientJoinedMsg {
				return &clientJoinedMsg{}
			},
		},
	)

	cfg := newTestPerClientConfig(mb, store)
	cfg.Dispatchers = router.AsDispatcherMap()

	bridge := NewClientsConnBridge()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	_, err := bridge.RegisterClient(
		ctx, ClientID("client-1"), cfg,
	)
	require.NoError(t, err)
	defer bridge.Stop()

	// Client pushes a ClientJoinedEvent to the server's per-client
	// mailbox.
	tc := newTestClient(mb, "client-1", "server-for-client-1")
	tc.pushEvent(
		t,
		"roundtest.v1.RoundNotifyService", "ClientJoined",
		&roundtestpb.ClientJoinedEvent{
			ClientId: "client-1",
			RoundId:  "round-7",
		},
	)

	// Poll the transcript until the server-side actor receives the
	// dispatched message.
	require.Eventually(t, func() bool {
		return behavior.received.entryCount() >= 1
	}, 5*time.Second, 50*time.Millisecond)

	msg := behavior.received.all()[0]
	require.Equal(t, "client-1", msg.ClientIDVal)
	require.Equal(t, "round-7", msg.RoundID)
}

// TestE2EServerUnaryRPCToClient verifies a full unary RPC round-trip from
// the server to a client. The server uses the per-client UnaryFacade to
// send a NotifyRoundStarted RPC, the test client dispatches it through a
// ServeMux and responds, and the server receives the typed response.
func TestE2EServerUnaryRPCToClient(t *testing.T) {
	t.Parallel()

	mb := newInMemoryMailbox()
	store := newMemCheckpointStore()
	cfg := newTestPerClientConfig(mb, store)

	bridge := NewClientsConnBridge()
	clientID := ClientID("client-1")

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	_, err := bridge.RegisterClient(ctx, clientID, cfg)
	require.NoError(t, err)
	defer bridge.Stop()

	// Start a test client with RoundNotifyService handlers.
	tc := newTestClient(mb, "client-1", "server-for-client-1")
	roundtestpb.RegisterRoundNotifyServiceMailboxServer(
		tc.mux, &roundNotifyServer{},
	)
	go tc.run(ctx)

	// Issue a unary NotifyRoundStarted RPC via the per-client
	// UnaryFacade.
	unary, ok := bridge.GetUnary(clientID)
	require.True(t, ok)

	client := roundtestpb.NewRoundNotifyServiceMailboxClient(unary)

	resp, err := client.NotifyRoundStarted(
		ctx, &roundtestpb.RoundStartedNotification{
			RoundId: "round-99",
		},
	)
	require.NoError(t, err)
	require.NotNil(t, resp)

	// Also exercise NotifyBatchReady.
	batchResp, err := client.NotifyBatchReady(
		ctx, &roundtestpb.BatchReadyNotification{
			RoundId:   "round-99",
			BatchData: []byte("test-batch-data"),
		},
	)
	require.NoError(t, err)
	require.NotNil(t, batchResp)
}

// TestE2EMultiClientEventDelivery registers 3 clients and verifies that
// each client receives only its own events when the server pushes events
// to each independently.
func TestE2EMultiClientEventDelivery(t *testing.T) {
	t.Parallel()

	mb := newInMemoryMailbox()
	bridge := NewClientsConnBridge()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	// Register 3 clients with separate mailbox pairs.
	type clientEntry struct {
		id    ClientID
		tc    *testClient
		store *memCheckpointStore
	}

	clients := make([]clientEntry, 3)
	for i := range clients {
		cid := ClientID(fmt.Sprintf("client-%d", i))
		localMB := fmt.Sprintf("server-for-client-%d", i)
		remoteMB := fmt.Sprintf("client-%d", i)
		store := newMemCheckpointStore()

		cfg := DefaultPerClientConfig()
		cfg.Edge = &fakeMailboxServiceClient{mb: mb}
		cfg.Store = store
		cfg.LocalMailboxID = localMB
		cfg.RemoteMailboxID = remoteMB
		cfg.ProtocolVersion = 1
		cfg.PullWaitTimeout = 50 * time.Millisecond
		cfg.Dispatchers = DispatcherMap{
			{
				Service: "test.v1.Noop",
				Method:  "Noop",
			}: func(
				_ context.Context,
				_ *mailboxpb.Envelope,
			) error {

				return nil
			},
		}

		_, err := bridge.RegisterClient(ctx, cid, cfg)
		require.NoError(t, err)

		tc := newTestClient(mb, remoteMB, localMB)
		go tc.run(ctx)

		clients[i] = clientEntry{
			id:    cid,
			tc:    tc,
			store: store,
		}
	}
	defer bridge.Stop()

	// Push a unique event to each client.
	for i, c := range clients {
		err := bridge.Tell(ctx, &SendServerEventRequest{
			Message: &roundStartedServerMsg{
				targetClientID: c.id,
				RoundID: fmt.Sprintf(
					"round-for-%d", i,
				),
			},
		})
		require.NoError(t, err)
	}

	// Verify each client received its own event.
	for i, c := range clients {
		expectedRound := fmt.Sprintf("round-for-%d", i)

		require.Eventually(t, func() bool {
			return c.tc.received.entryCount() >= 1
		}, 5*time.Second, 50*time.Millisecond,
			"client %d did not receive event", i,
		)

		env := c.tc.received.all()[0]
		var event roundtestpb.RoundStartedEvent
		err := proto.Unmarshal(
			env.Body.Value, &event,
		)
		require.NoError(t, err)
		require.Equal(t, expectedRound, event.RoundId)
	}
}

// TestE2EBidirectional verifies a combined scenario where the server
// pushes an event, the client sends an event, and the server issues a
// unary RPC — all in the same session using the same mailbox pair.
func TestE2EBidirectional(t *testing.T) {
	t.Parallel()

	mb := newInMemoryMailbox()
	store := newMemCheckpointStore()
	system := actor.NewActorSystem()

	// Register server-side actor for client-pushed events.
	joinKey := actor.NewServiceKey[*clientJoinedMsg, struct{}](
		"join-actor",
	)
	joinBehavior := &clientJoinedBehavior{
		received: &transcript[*clientJoinedMsg]{},
	}
	actor.RegisterWithSystem(
		system, "join-1", joinKey, joinBehavior,
	)

	// Wire EventRouter for ClientJoinedEvent dispatch.
	router := NewEventRouter(system)
	NewEventRoute(
		router,
		InboundEventRouteConfig[*clientJoinedMsg, struct{}]{
			Service: "roundtest.v1.RoundNotifyService",
			Method:  "ClientJoined",
			Key:     joinKey,
			NewEvent: func() proto.Message {
				return &roundtestpb.ClientJoinedEvent{}
			},
			NewMsg: func() *clientJoinedMsg {
				return &clientJoinedMsg{}
			},
		},
	)

	cfg := newTestPerClientConfig(mb, store)
	cfg.Dispatchers = router.AsDispatcherMap()

	bridge := NewClientsConnBridge()
	clientID := ClientID("client-1")

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	_, err := bridge.RegisterClient(ctx, clientID, cfg)
	require.NoError(t, err)
	defer bridge.Stop()

	// Start test client with RPC handlers.
	tc := newTestClient(mb, "client-1", "server-for-client-1")
	roundtestpb.RegisterRoundNotifyServiceMailboxServer(
		tc.mux, &roundNotifyServer{},
	)
	go tc.run(ctx)

	// Phase 1: Server pushes an event to the client.
	err = bridge.Tell(ctx, &SendServerEventRequest{
		Message: &roundStartedServerMsg{
			targetClientID: clientID,
			RoundID:        "round-bidir-1",
		},
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return tc.received.entryCount() >= 1
	}, 5*time.Second, 50*time.Millisecond)

	env := tc.received.all()[0]
	var biEvent roundtestpb.RoundStartedEvent
	unmarshalErr := proto.Unmarshal(
		env.Body.Value, &biEvent,
	)
	require.NoError(t, unmarshalErr)
	require.Equal(t, "round-bidir-1", biEvent.RoundId)

	// Phase 2: Client sends an event to the server.
	tc.pushEvent(
		t,
		"roundtest.v1.RoundNotifyService", "ClientJoined",
		&roundtestpb.ClientJoinedEvent{
			ClientId: "client-1",
			RoundId:  "round-bidir-1",
		},
	)

	require.Eventually(t, func() bool {
		return joinBehavior.received.entryCount() >= 1
	}, 5*time.Second, 50*time.Millisecond)

	joinMsg := joinBehavior.received.all()[0]
	require.Equal(t, "client-1", joinMsg.ClientIDVal)
	require.Equal(t, "round-bidir-1", joinMsg.RoundID)

	// Phase 3: Server sends a unary RPC to the client.
	unary, ok := bridge.GetUnary(clientID)
	require.True(t, ok)

	rpcClient := roundtestpb.NewRoundNotifyServiceMailboxClient(
		unary,
	)
	resp, err := rpcClient.NotifyRoundStarted(
		ctx, &roundtestpb.RoundStartedNotification{
			RoundId: "round-bidir-1",
		},
	)
	require.NoError(t, err)
	require.NotNil(t, resp)
}

// TestE2EClientRegistrationLifecycle verifies the register-send-deregister
// lifecycle: register a client, send events, deregister (stops ingress),
// then register again with fresh state.
func TestE2EClientRegistrationLifecycle(t *testing.T) {
	t.Parallel()

	mb := newInMemoryMailbox()
	bridge := NewClientsConnBridge()
	clientID := ClientID("client-1")

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	// Phase 1: Register client and send an event.
	store1 := newMemCheckpointStore()
	cfg1 := newTestPerClientConfig(mb, store1)

	_, err := bridge.RegisterClient(ctx, clientID, cfg1)
	require.NoError(t, err)

	tc := newTestClient(mb, "client-1", "server-for-client-1")
	tcCtx, tcCancel := context.WithCancel(ctx)
	go tc.run(tcCtx)

	err = bridge.Tell(ctx, &SendServerEventRequest{
		Message: &roundStartedServerMsg{
			targetClientID: clientID,
			RoundID:        "round-lifecycle-1",
		},
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return tc.received.entryCount() >= 1
	}, 5*time.Second, 50*time.Millisecond)

	lcEnv := tc.received.all()[0]
	var lcEvent roundtestpb.RoundStartedEvent
	require.NoError(t, proto.Unmarshal(
		lcEnv.Body.Value, &lcEvent,
	))
	require.Equal(t, "round-lifecycle-1", lcEvent.RoundId)

	// Phase 2: Deregister the client.
	tcCancel()
	err = bridge.DeregisterClient(clientID)
	require.NoError(t, err)

	// Sending to a deregistered client should fail.
	err = bridge.Tell(ctx, &SendServerEventRequest{
		Message: &roundStartedServerMsg{
			targetClientID: clientID,
			RoundID:        "should-fail",
		},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not registered")

	// Phase 3: Re-register the same client with a fresh store.
	store2 := newMemCheckpointStore()
	cfg2 := newTestPerClientConfig(mb, store2)

	_, err = bridge.RegisterClient(ctx, clientID, cfg2)
	require.NoError(t, err)
	defer bridge.Stop()

	tc2 := newTestClient(mb, "client-1", "server-for-client-1")
	go tc2.run(ctx)

	err = bridge.Tell(ctx, &SendServerEventRequest{
		Message: &roundStartedServerMsg{
			targetClientID: clientID,
			RoundID:        "round-lifecycle-2",
		},
	})
	require.NoError(t, err)

	// The shared in-memory mailbox may still contain the old
	// "round-lifecycle-1" event at a lower cursor. The new test
	// client starts from cursor 0, so it may deliver stale events
	// before the fresh one. Poll until we see the expected event.
	require.Eventually(t, func() bool {
		for _, env := range tc2.received.all() {
			var event roundtestpb.RoundStartedEvent
			if err := proto.Unmarshal(
				env.Body.Value, &event,
			); err != nil {

				continue
			}

			if event.RoundId == "round-lifecycle-2" {
				return true
			}
		}

		return false
	}, 5*time.Second, 50*time.Millisecond)
}

// TestE2EUnaryRPCError verifies that a client-side gRPC error is
// transported through the mailbox envelope headers and surfaced to the
// server as a proper gRPC status error via AwaitRPC.
func TestE2EUnaryRPCError(t *testing.T) {
	t.Parallel()

	mb := newInMemoryMailbox()
	store := newMemCheckpointStore()
	cfg := newTestPerClientConfig(mb, store)

	bridge := NewClientsConnBridge()
	clientID := ClientID("client-1")

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	_, err := bridge.RegisterClient(ctx, clientID, cfg)
	require.NoError(t, err)
	defer bridge.Stop()

	// Start a test client that always returns errors.
	tc := newTestClient(mb, "client-1", "server-for-client-1")
	roundtestpb.RegisterRoundNotifyServiceMailboxServer(
		tc.mux, &errRoundNotifyServer{},
	)
	go tc.run(ctx)

	unary, ok := bridge.GetUnary(clientID)
	require.True(t, ok)

	client := roundtestpb.NewRoundNotifyServiceMailboxClient(unary)

	// NotifyRoundStarted should surface a NotFound gRPC error.
	_, notifyErr := client.NotifyRoundStarted(
		ctx, &roundtestpb.RoundStartedNotification{
			RoundId: "round-err",
		},
	)
	require.Error(t, notifyErr)

	st, ok := status.FromError(notifyErr)
	require.True(
		t, ok,
		"expected gRPC status error, got: %v", notifyErr,
	)
	require.Equal(t, codes.NotFound, st.Code())
	require.Contains(t, st.Message(), "round not found")

	// NotifyBatchReady should surface an InvalidArgument gRPC error.
	_, batchErr := client.NotifyBatchReady(
		ctx, &roundtestpb.BatchReadyNotification{
			RoundId: "round-err",
		},
	)
	require.Error(t, batchErr)

	st, ok = status.FromError(batchErr)
	require.True(
		t, ok,
		"expected gRPC status error, got: %v", batchErr,
	)
	require.Equal(t, codes.InvalidArgument, st.Code())
	require.Contains(t, st.Message(), "batch_data is required")
}

// TestE2EIngressSurvivesCallerContextCancel verifies that cancelling the
// context used to register a client does not kill the ingress loop. The
// ingress goroutine runs under its own background-rooted context, so a
// request-scoped registration context can be cancelled without
// disrupting message delivery.
func TestE2EIngressSurvivesCallerContextCancel(t *testing.T) {
	t.Parallel()

	mb := newInMemoryMailbox()
	store := newMemCheckpointStore()
	system := actor.NewActorSystem()

	// Register a server-side actor to receive client events via the
	// EventRouter.
	joinKey := actor.NewServiceKey[*clientJoinedMsg, struct{}](
		"ctx-cancel-join",
	)
	behavior := &clientJoinedBehavior{
		received: &transcript[*clientJoinedMsg]{},
	}
	actor.RegisterWithSystem(
		system, "ctx-cancel-join-1", joinKey, behavior,
	)

	router := NewEventRouter(system)
	NewEventRoute(
		router,
		InboundEventRouteConfig[*clientJoinedMsg, struct{}]{
			Service: "roundtest.v1.RoundNotifyService",
			Method:  "ClientJoined",
			Key:     joinKey,
			NewEvent: func() proto.Message {
				return &roundtestpb.ClientJoinedEvent{}
			},
			NewMsg: func() *clientJoinedMsg {
				return &clientJoinedMsg{}
			},
		},
	)

	cfg := newTestPerClientConfig(mb, store)
	cfg.Dispatchers = router.AsDispatcherMap()

	// Register with a short-lived context that we cancel
	// immediately after startup completes. We intentionally use
	// context.Background() here (not t.Context()) because we need
	// an independently cancellable context to test that the ingress
	// loop survives its cancellation.
	regCtx, regCancel := context.WithCancel(
		context.Background(),
	)

	bridge := NewClientsConnBridge()
	_, err := bridge.RegisterClient(
		regCtx, ClientID("client-ctx-test"), cfg,
	)
	require.NoError(t, err)
	t.Cleanup(bridge.Stop)

	// Cancel the registration context. If the ingress loop were
	// derived from this context, it would die here.
	regCancel()

	// Give the cancellation a moment to propagate.
	time.Sleep(50 * time.Millisecond)

	// The ingress loop should still be alive. Push a client event
	// into the server's per-client mailbox and verify the router
	// dispatches it to the registered actor.
	tc := newTestClient(
		mb, cfg.RemoteMailboxID, cfg.LocalMailboxID,
	)
	tc.pushEvent(
		t,
		"roundtest.v1.RoundNotifyService", "ClientJoined",
		&roundtestpb.ClientJoinedEvent{
			ClientId: "client-ctx-test",
			RoundId:  "ctx-cancel-round",
		},
	)

	// Wait for the server-side actor to receive the dispatched
	// message. This proves the ingress loop survived the caller
	// context cancellation.
	require.Eventually(t, func() bool {
		return behavior.received.entryCount() > 0
	}, 5*time.Second, 25*time.Millisecond)

	entry := behavior.received.all()[0]
	require.Equal(t, "ctx-cancel-round", entry.RoundID)
	require.Equal(t, "client-ctx-test", entry.ClientIDVal)
}

// TestDispatchBatchCursorAdvancesPastSkipped verifies that dispatchBatch
// advances the committed cursor past skipped envelopes (missing RPC
// metadata, unknown kind, no registered dispatcher). If a later envelope
// in the batch fails dispatch, the returned cursor should reflect the
// position of the failing envelope, not stall behind the skipped ones.
// Without this fix, skipped envelopes would be re-pulled and re-warned
// on every retry, creating avoidable replay churn.
func TestDispatchBatchCursorAdvancesPastSkipped(t *testing.T) {
	t.Parallel()

	mb := newInMemoryMailbox()
	store := newMemCheckpointStore()
	cfg := newTestPerClientConfig(mb, store)

	// Register a dispatcher that always fails, so we can observe
	// the cursor returned on the error path.
	failErr := fmt.Errorf("deliberate dispatch failure")
	cfg.Dispatchers = DispatcherMap{
		{
			Service: "test.v1.Svc",
			Method:  "Fail",
		}: func(
			_ context.Context,
			_ *mailboxpb.Envelope,
		) error {

			return failErr
		},
	}

	connector := NewClientConnectionActor(cfg)

	// Build a batch: [skip(seq=1), skip(seq=2), fail(seq=3)].
	// First two have no RPC metadata and will be skipped. Third
	// has a dispatcher that returns an error.
	envelopes := []*mailboxpb.Envelope{
		{
			MsgId:    "skip-1",
			EventSeq: 1,
			// No RPC metadata — will be skipped.
		},
		{
			MsgId:    "skip-2",
			EventSeq: 2,
			// No RPC metadata — will be skipped.
		},
		{
			MsgId:    "fail-3",
			EventSeq: 3,
			Rpc: &mailboxpb.RpcMeta{
				Kind:    mailboxpb.RpcMeta_KIND_EVENT,
				Service: "test.v1.Svc",
				Method:  "Fail",
			},
			Body: &anypb.Any{
				Value: []byte{},
			},
		},
	}

	cursor, _, err := connector.dispatchBatch(
		t.Context(), envelopes, 4,
	)

	// Dispatch should fail on the third envelope.
	require.ErrorIs(t, err, failErr)

	// The cursor should be at 2 (the last safely-skipped
	// envelope before the failure), NOT 0 (stalled behind skipped
	// envelopes) and NOT 3 (the failing envelope's seq, which
	// would skip it and cause message loss). The caller adds 1 to
	// get exclusive cursor 3, meaning envelope 3 will be retried.
	require.Equal(t, uint64(2), cursor)
}

// TestBridgeTellNilMessage verifies that Tell returns an error (not a
// panic) when given a SendServerEventRequest with a nil Message field.
func TestBridgeTellNilMessage(t *testing.T) {
	t.Parallel()

	bridge := NewClientsConnBridge()
	defer bridge.Stop()

	err := bridge.Tell(t.Context(), &SendServerEventRequest{
		Message: nil,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "nil Message")
}

// TestRegisterClientDuplicateMailboxID verifies that RegisterClient
// rejects a client whose LocalMailboxID or RemoteMailboxID collides
// with an already-registered client, even if the ClientID is different.
// Sharing mailbox IDs would alias checkpoint and durable actor
// identity, corrupting delivery progress.
func TestRegisterClientDuplicateMailboxID(t *testing.T) {
	t.Parallel()

	mb := newInMemoryMailbox()
	store := newMemCheckpointStore()

	bridge := NewClientsConnBridge()
	defer bridge.Stop()

	// Register the first client.
	cfg1 := newTestPerClientConfig(mb, store)
	_, err := bridge.RegisterClient(
		t.Context(), "client-1", cfg1,
	)
	require.NoError(t, err)

	// Attempt to register a second client with the same
	// LocalMailboxID.
	cfg2 := newTestPerClientConfig(mb, store)
	cfg2.RemoteMailboxID = "different-remote"

	_, err = bridge.RegisterClient(
		t.Context(), "client-2", cfg2,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "LocalMailboxID")
	require.Contains(t, err.Error(), "already in use")

	// Attempt to register with the same RemoteMailboxID.
	cfg3 := newTestPerClientConfig(mb, store)
	cfg3.LocalMailboxID = "different-local"

	_, err = bridge.RegisterClient(
		t.Context(), "client-3", cfg3,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "RemoteMailboxID")
	require.Contains(t, err.Error(), "already in use")
}

// TestWithStatusTrackerNil verifies that passing nil to
// WithStatusTracker preserves the default noop tracker instead of
// causing a nil pointer panic.
func TestWithStatusTrackerNil(t *testing.T) {
	t.Parallel()

	// Should not panic — nil tracker is silently ignored.
	bridge := NewClientsConnBridge(WithStatusTracker(nil))

	// ClientStatus should return the default StatusUnknown from the
	// noop tracker.
	status := bridge.ClientStatus("nonexistent")
	require.Equal(t, StatusUnknown, status)

	bridge.Stop()
}

// TestBridgeTellTypedNilPointer verifies that Tell returns an error (not a
// panic) when called with a typed-nil *SendServerEventRequest. This exercises
// the type-switch branch where m is a non-nil interface holding a nil concrete
// pointer — the Go type switch assigns m != nil (the interface holds a type),
// but dereferencing m.Message would panic without an explicit nil guard.
func TestBridgeTellTypedNilPointer(t *testing.T) {
	t.Parallel()

	bridge := NewClientsConnBridge()
	defer bridge.Stop()

	var req *SendServerEventRequest // typed nil
	err := bridge.Tell(t.Context(), req)
	require.Error(t, err)
	require.Contains(t, err.Error(), "typed-nil")
}

// ---------------------------------------------------------------------------
// StatusTracker integration tests
// ---------------------------------------------------------------------------

// TestTrackerWiringInboundActivity verifies the full bridge + ingress →
// tracker integration: a real PullActivityTracker is injected into the
// bridge, a client sends an envelope, and the tracker transitions the
// client from unknown to online.
func TestTrackerWiringInboundActivity(t *testing.T) {
	t.Parallel()

	mb := newInMemoryMailbox()
	store := newMemCheckpointStore()

	tracker := NewPullActivityTracker()
	defer tracker.Stop()

	bridge := NewClientsConnBridge(WithStatusTracker(tracker))

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	clientID := ClientID("client-1")
	cfg := newTestPerClientConfig(mb, store)

	// Add the heartbeat dispatcher so the heartbeat service/method
	// is recognised by the ingress loop.
	cfg.Dispatchers[HeartbeatServiceMethod()] =
		HeartbeatDispatcher()

	_, err := bridge.RegisterClient(ctx, clientID, cfg)
	require.NoError(t, err)
	defer bridge.Stop()

	// Verify the tracker registered the client but status is
	// still unknown (no activity yet).
	require.Equal(t, StatusUnknown, bridge.ClientStatus(clientID))

	// Simulate the client sending a heartbeat envelope to the
	// server's per-client mailbox.
	tc := newTestClient(mb, "client-1", "server-for-client-1")
	tc.pushEvent(
		t, HeartbeatService, HeartbeatMethod,
		&roundtestpb.ClientJoinedEvent{},
	)

	// The ingress loop should pull, dispatch, and call MarkActive.
	require.Eventually(t, func() bool {
		return bridge.ClientStatus(clientID) == StatusOnline
	}, 5*time.Second, 50*time.Millisecond)

	// ListClients should also reflect the online status.
	clients := bridge.ListClients()
	require.Len(t, clients, 1)
	require.Equal(t, StatusOnline, clients[0].Status)
}

// TestTrackerWiringDeregister verifies that deregistering a client
// cleans up the tracker state and fires an offline callback.
func TestTrackerWiringDeregister(t *testing.T) {
	t.Parallel()

	mb := newInMemoryMailbox()
	store := newMemCheckpointStore()

	tracker := NewPullActivityTracker()
	defer tracker.Stop()

	var (
		mu          sync.Mutex
		transitions []ClientStatus
	)
	tracker.OnStatusChange(func(_ ClientID, s ClientStatus) {
		mu.Lock()
		defer mu.Unlock()

		transitions = append(transitions, s)
	})

	bridge := NewClientsConnBridge(WithStatusTracker(tracker))

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	clientID := ClientID("client-1")
	cfg := newTestPerClientConfig(mb, store)
	cfg.Dispatchers[HeartbeatServiceMethod()] =
		HeartbeatDispatcher()

	_, err := bridge.RegisterClient(ctx, clientID, cfg)
	require.NoError(t, err)

	// Make the client online first.
	tracker.MarkActive(clientID)
	require.Equal(t, StatusOnline, bridge.ClientStatus(clientID))

	// Deregister should clean up and fire offline callback.
	err = bridge.DeregisterClient(clientID)
	require.NoError(t, err)

	require.Equal(t, StatusUnknown, bridge.ClientStatus(clientID))

	mu.Lock()
	require.Equal(
		t, []ClientStatus{StatusOnline, StatusOffline}, transitions,
	)
	mu.Unlock()
}

// TestTrackerWiringStaleOffline verifies that a client that goes quiet
// transitions from online to offline after the staleness threshold. Uses
// a short threshold and sweep interval so the test completes quickly.
func TestTrackerWiringStaleOffline(t *testing.T) {
	t.Parallel()

	mb := newInMemoryMailbox()
	store := newMemCheckpointStore()

	tracker := NewPullActivityTracker(
		WithStaleThreshold(500*time.Millisecond),
		WithSweepInterval(100*time.Millisecond),
	)
	defer tracker.Stop()

	var (
		mu          sync.Mutex
		transitions []ClientStatus
	)
	tracker.OnStatusChange(func(_ ClientID, s ClientStatus) {
		mu.Lock()
		defer mu.Unlock()

		transitions = append(transitions, s)
	})

	bridge := NewClientsConnBridge(WithStatusTracker(tracker))

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	clientID := ClientID("client-1")
	cfg := newTestPerClientConfig(mb, store)
	cfg.Dispatchers[HeartbeatServiceMethod()] =
		HeartbeatDispatcher()

	_, err := bridge.RegisterClient(ctx, clientID, cfg)
	require.NoError(t, err)
	defer bridge.Stop()

	// Send a heartbeat to go online.
	tc := newTestClient(mb, "client-1", "server-for-client-1")
	tc.pushEvent(
		t, HeartbeatService, HeartbeatMethod,
		&roundtestpb.ClientJoinedEvent{},
	)

	require.Eventually(t, func() bool {
		return bridge.ClientStatus(clientID) == StatusOnline
	}, 5*time.Second, 50*time.Millisecond)

	// Now go quiet. After the stale threshold + sweep, the
	// tracker should transition to offline.
	require.Eventually(t, func() bool {
		return bridge.ClientStatus(clientID) == StatusOffline
	}, 5*time.Second, 50*time.Millisecond)

	mu.Lock()
	require.Equal(
		t, []ClientStatus{StatusOnline, StatusOffline}, transitions,
	)
	mu.Unlock()
}

// TestTrackerWiringMultipleClients verifies that ListClients returns
// correct per-client status when multiple clients have different
// activity patterns.
func TestTrackerWiringMultipleClients(t *testing.T) {
	t.Parallel()

	mb := newInMemoryMailbox()

	tracker := NewPullActivityTracker()
	defer tracker.Stop()

	bridge := NewClientsConnBridge(WithStatusTracker(tracker))

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	// Register two clients with separate stores and mailbox IDs.
	store1 := newMemCheckpointStore()
	cfg1 := newTestPerClientConfig(mb, store1)
	cfg1.Dispatchers[HeartbeatServiceMethod()] =
		HeartbeatDispatcher()

	store2 := newMemCheckpointStore()
	cfg2 := DefaultPerClientConfig()
	cfg2.Edge = &fakeMailboxServiceClient{mb: mb}
	cfg2.Store = store2
	cfg2.LocalMailboxID = "server-for-client-2"
	cfg2.RemoteMailboxID = "client-2"
	cfg2.ProtocolVersion = 1
	cfg2.PullWaitTimeout = 50 * time.Millisecond
	cfg2.Dispatchers = DispatcherMap{
		HeartbeatServiceMethod(): HeartbeatDispatcher(),
	}

	_, err := bridge.RegisterClient(
		ctx, ClientID("client-1"), cfg1,
	)
	require.NoError(t, err)

	_, err = bridge.RegisterClient(
		ctx, ClientID("client-2"), cfg2,
	)
	require.NoError(t, err)
	defer bridge.Stop()

	// Only client-1 sends traffic.
	tc := newTestClient(mb, "client-1", "server-for-client-1")
	tc.pushEvent(
		t, HeartbeatService, HeartbeatMethod,
		&roundtestpb.ClientJoinedEvent{},
	)

	require.Eventually(t, func() bool {
		return bridge.ClientStatus("client-1") == StatusOnline
	}, 5*time.Second, 50*time.Millisecond)

	// Client-2 has not sent anything — should still be unknown.
	require.Equal(
		t, StatusUnknown,
		bridge.ClientStatus("client-2"),
	)

	// ListClients should show both with correct statuses.
	clients := bridge.ListClients()
	require.Len(t, clients, 2)

	statusMap := make(map[ClientID]ClientStatus)
	for _, c := range clients {
		statusMap[c.ID] = c.Status
	}
	require.Equal(t, StatusOnline, statusMap["client-1"])
	require.Equal(t, StatusUnknown, statusMap["client-2"])
}

// ---------------------------------------------------------------------------
// Property-based tests for dispatchBatch cursor invariants
// ---------------------------------------------------------------------------

// TestDispatchBatchCursorInvariants_Property uses rapid to verify that
// dispatchBatch preserves two invariants across a wide range of randomly
// generated envelope batches:
//
//  1. On dispatch error, the returned cursor never includes the failing
//     envelope — it is the last safely-processed (dispatched or skipped)
//     envelope's event_seq, or 0 if nothing was safely processed.
//  2. On success, the returned cursor equals batchNextCursor (the
//     exclusive position after all envelopes in the batch).
//
// The test generates random batches of envelopes where each envelope is
// randomly assigned one of: skip (nil RPC metadata), skip (no dispatcher),
// dispatch-succeed, or dispatch-fail. It then asserts the invariants.
func TestDispatchBatchCursorInvariants_Property(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		batchSize := rapid.IntRange(1, 20).Draw(
			rt, "batch_size",
		)

		failErr := fmt.Errorf("deliberate dispatch failure")
		kindEvent := mailboxpb.RpcMeta_KIND_EVENT

		// Decide behavior for each envelope: 0=skip-nil-rpc,
		// 1=skip-no-dispatcher, 2=succeed, 3=fail.
		behaviors := make([]int, batchSize)
		envelopes := make([]*mailboxpb.Envelope, batchSize)

		// At most one envelope fails dispatch per batch (the
		// first one drawn as "fail" triggers the error return).
		// Track the expected failure point.
		failIdx := -1

		for i := 0; i < batchSize; i++ {
			seq := uint64(i + 1)

			behavior := rapid.IntRange(0, 3).Draw(
				rt,
				fmt.Sprintf("env_%d_behavior", i),
			)

			// Only the first fail counts — subsequent
			// envelopes won't be reached after the error
			// return.
			if behavior == 3 && failIdx >= 0 {
				behavior = 2
			}
			if behavior == 3 {
				failIdx = i
			}

			behaviors[i] = behavior

			switch behavior {
			case 0:
				// Skip: nil RPC metadata.
				envelopes[i] = &mailboxpb.Envelope{
					MsgId:    fmt.Sprintf("msg-%d", i),
					EventSeq: seq,
				}

			case 1:
				// Skip: has RPC metadata but no
				// registered dispatcher.
				envelopes[i] = &mailboxpb.Envelope{
					MsgId:    fmt.Sprintf("msg-%d", i),
					EventSeq: seq,
					Rpc: &mailboxpb.RpcMeta{
						Kind:    kindEvent,
						Service: "unregistered.v1.Svc",
						Method:  "NoHandler",
					},
					Body: &anypb.Any{
						Value: []byte{},
					},
				}

			case 2:
				// Succeed: has dispatcher that returns
				// nil.
				envelopes[i] = &mailboxpb.Envelope{
					MsgId:    fmt.Sprintf("msg-%d", i),
					EventSeq: seq,
					Rpc: &mailboxpb.RpcMeta{
						Kind:    kindEvent,
						Service: "test.v1.Svc",
						Method:  "OK",
					},
					Body: &anypb.Any{
						Value: []byte{},
					},
				}

			case 3:
				// Fail: has dispatcher that returns an
				// error.
				envelopes[i] = &mailboxpb.Envelope{
					MsgId:    fmt.Sprintf("msg-%d", i),
					EventSeq: seq,
					Rpc: &mailboxpb.RpcMeta{
						Kind:    kindEvent,
						Service: "test.v1.Svc",
						Method:  "Fail",
					},
					Body: &anypb.Any{
						Value: []byte{},
					},
				}
			}
		}

		// Build dispatchers with OK and Fail routes.
		dispatchers := DispatcherMap{
			{
				Service: "test.v1.Svc",
				Method:  "OK",
			}: func(
				_ context.Context,
				_ *mailboxpb.Envelope,
			) error {

				return nil
			},
			{
				Service: "test.v1.Svc",
				Method:  "Fail",
			}: func(
				_ context.Context,
				_ *mailboxpb.Envelope,
			) error {

				return failErr
			},
		}

		mb := newInMemoryMailbox()
		store := newMemCheckpointStore()
		cfg := newTestPerClientConfig(mb, store)
		cfg.Dispatchers = dispatchers

		connector := NewClientConnectionActor(cfg)

		batchNextCursor := uint64(batchSize + 1)
		cursor, _, err := connector.dispatchBatch(
			t.Context(), envelopes, batchNextCursor,
		)

		if failIdx >= 0 {
			// Error path: cursor must NOT include the
			// failing envelope.
			if err == nil {
				rt.Fatalf("expected error, got nil")
			}

			failSeq := uint64(failIdx + 1)
			if cursor >= failSeq {
				rt.Fatalf("cursor %d >= failing envelope seq "+
					"%d — would skip it", cursor, failSeq)
			}

			// Cursor must be the largest event_seq of all
			// safely-processed envelopes before the failure.
			expectedSafe := uint64(0)
			for j := 0; j < failIdx; j++ {
				s := envelopes[j].EventSeq
				if s > expectedSafe {
					expectedSafe = s
				}
			}

			if cursor != expectedSafe {
				rt.Fatalf("cursor %d != expected safe %d",
					cursor, expectedSafe)
			}
		} else {
			// Success path: cursor equals
			// batchNextCursor.
			if err != nil {
				rt.Fatalf("unexpected error: %v", err)
			}

			if cursor != batchNextCursor {
				rt.Fatalf("cursor %d != batchNextCursor %d",
					cursor, batchNextCursor)
			}
		}
	})
}

// TestIngressAckNeverExceedsCommitted_Property validates that randomized ack
// and partial-dispatch progressions preserve the invariant:
// AckCommittedTo <= DispatchCommittedTo. This mirrors the serverconn property
// test of the same name, adapted for clientconn's cursor semantics where
// lastSafe excludes the failing envelope.
func TestIngressAckNeverExceedsCommitted_Property(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		var state AckState
		steps := rapid.IntRange(1, 400).Draw(rt, "steps")

		for i := 0; i < steps; i++ {
			if state.NeedsAck() {
				ackSucceeds := rapid.Bool().Draw(
					rt, "ack_succeeds",
				)
				if ackSucceeds {
					state.AdvanceAck()
				}
			}

			dispatchFails := rapid.Bool().Draw(
				rt, "dispatch_fails",
			)

			batchSize := rapid.Uint32Range(1, 8).Draw(
				rt, "batch_size",
			)
			batchNextCursor := state.PullCursor +
				uint64(batchSize)

			if dispatchFails {
				// Model lastSafe: some envelopes before
				// the failure point were safely skipped
				// or dispatched.
				safeCount := rapid.Uint32Range(
					0, batchSize-1,
				).Draw(rt, "safe_count")

				if safeCount == 0 {
					// Nothing safely processed, no
					// cursor advance.
					continue
				}

				// lastSafe is the inclusive event_seq
				// of the last safely-processed envelope.
				// Envelope seqs start at PullCursor.
				lastSafe := state.PullCursor +
					uint64(safeCount) - 1

				// ingressLoop converts inclusive to
				// exclusive.
				advanceCursor := lastSafe + 1
				if lastSafe > 0 &&
					advanceCursor > state.PullCursor {

					state.AdvanceDispatch(advanceCursor)
					state.PullCursor = advanceCursor
				}
			} else {
				state.AdvanceDispatch(batchNextCursor)
				state.PullCursor = batchNextCursor
			}

			// Invariant 1: Ack never exceeds dispatch.
			if state.AckCommittedTo > state.DispatchCommittedTo {
				rt.Fatalf("ack cursor > dispatch: %d > %d",
					state.AckCommittedTo,
					state.DispatchCommittedTo)
			}

			// Invariant 2: Ack never exceeds pull.
			if state.AckCommittedTo > state.PullCursor {
				rt.Fatalf("ack cursor > pull: %d > %d",
					state.AckCommittedTo, state.PullCursor)
			}

			// Invariant 3: Pull is always at or past dispatch.
			if state.DispatchCommittedTo > 0 &&
				state.PullCursor < state.DispatchCommittedTo {

				rt.Fatalf("pull cursor behind dispatch: "+
					"%d < %d", state.PullCursor,
					state.DispatchCommittedTo)
			}
		}
	})
}

// TestDispatchBatchSkipOnly_Property verifies that when no dispatchers match
// any envelopes in a batch (all envelopes are skipped), the cursor advances
// to batchNextCursor and no error is returned. This confirms that the "safe
// to skip" paths all correctly advance lastSafe.
func TestDispatchBatchSkipOnly_Property(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		batchSize := rapid.IntRange(1, 30).Draw(
			rt, "batch_size",
		)
		kindEvent := mailboxpb.RpcMeta_KIND_EVENT

		envelopes := make([]*mailboxpb.Envelope, batchSize)
		for i := 0; i < batchSize; i++ {
			skipKind := rapid.IntRange(0, 2).Draw(
				rt,
				fmt.Sprintf("skip_kind_%d", i),
			)

			switch skipKind {
			case 0:
				// Nil RPC metadata.
				envelopes[i] = &mailboxpb.Envelope{
					MsgId:    fmt.Sprintf("s-%d", i),
					EventSeq: uint64(i + 1),
				}

			case 1:
				// No registered dispatcher.
				envelopes[i] = &mailboxpb.Envelope{
					MsgId:    fmt.Sprintf("s-%d", i),
					EventSeq: uint64(i + 1),
					Rpc: &mailboxpb.RpcMeta{
						Kind:    kindEvent,
						Service: "unknown.v1.Svc",
						Method:  "NoRoute",
					},
					Body: &anypb.Any{
						Value: []byte{},
					},
				}

			case 2:
				// Unknown RPC kind (e.g., 99).
				envelopes[i] = &mailboxpb.Envelope{
					MsgId:    fmt.Sprintf("s-%d", i),
					EventSeq: uint64(i + 1),
					Rpc: &mailboxpb.RpcMeta{
						Kind:    99,
						Service: "test.v1.Svc",
						Method:  "Whatever",
					},
					Body: &anypb.Any{
						Value: []byte{},
					},
				}
			}
		}

		mb := newInMemoryMailbox()
		store := newMemCheckpointStore()
		cfg := newTestPerClientConfig(mb, store)
		cfg.Dispatchers = DispatcherMap{}

		connector := NewClientConnectionActor(cfg)

		batchNextCursor := uint64(batchSize + 1)
		cursor, _, err := connector.dispatchBatch(
			t.Context(), envelopes, batchNextCursor,
		)

		if err != nil {
			rt.Fatalf("unexpected error: %v", err)
		}

		// All skipped — cursor should be batchNextCursor.
		if cursor != batchNextCursor {
			rt.Fatalf("cursor %d != batchNextCursor %d", cursor,
				batchNextCursor)
		}
	})
}

// ---------------------------------------------------------------------------
// TLV round-trip tests
// ---------------------------------------------------------------------------

// TestSendEventMsgTLVRoundTrip verifies that a sendEventMsg survives
// Encode → Decode without data loss. The decoded message must carry the
// same clientID, MsgID, IdempotencyKey, and proto payload.
func TestSendEventMsgTLVRoundTrip(t *testing.T) {
	t.Parallel()

	original := &sendEventMsg{
		Message: &roundStartedServerMsg{
			targetClientID: "client-1",
			RoundID:        "round-42",
		},
		MsgID:          "msg-aaa",
		IdempotencyKey: "idem-bbb",
		clientID:       "client-1",
	}

	var buf bytes.Buffer
	require.NoError(t, original.Encode(&buf))

	decoded := &sendEventMsg{}
	require.NoError(t, decoded.Decode(&buf))

	require.Equal(t, original.MsgID, decoded.MsgID)
	require.Equal(t, original.IdempotencyKey,
		decoded.IdempotencyKey)
	require.Equal(t, original.clientID, decoded.clientID)

	// The decoded message wraps a rawClientMessage; verify the
	// proto round-trips correctly.
	decodedProto := decoded.Message.ToProto()
	require.NotNil(t, decodedProto)

	got, ok := decodedProto.(*roundtestpb.RoundStartedEvent)
	require.True(t, ok)
	require.Equal(t, "round-42", got.RoundId)

	// Verify the routing metadata survives the TLV round-trip.
	// These fields (TLV types 6/7) are critical for client-side
	// ingress dispatch after crash-recovery replay.
	decodedSM := decoded.Message.ServiceMethod()
	require.Equal(
		t, "hellotest.v1.HelloService", decodedSM.Service,
		"rpcService must survive TLV round-trip",
	)
	require.Equal(
		t, "RoundStarted", decodedSM.Method,
		"rpcMethod must survive TLV round-trip",
	)
}

// TestSendRPCMsgTLVRoundTrip verifies that a sendRPCMsg survives
// Encode → Decode without data loss. The decoded message must carry the
// same envelope fields.
func TestSendRPCMsgTLVRoundTrip(t *testing.T) {
	t.Parallel()

	body, err := anypb.New(&roundtestpb.RoundStartedNotification{
		RoundId: "round-99",
	})
	require.NoError(t, err)

	original := &sendRPCMsg{
		Envelope: &mailboxpb.Envelope{
			ProtocolVersion: 1,
			MsgId:           "rpc-msg-1",
			IdempotencyKey:  "rpc-idem-1",
			Sender:          "server-for-client-1",
			Recipient:       "client-1",
			CreatedAtUnixMs: 1700000000000,
			Body:            body,
			Rpc: &mailboxpb.RpcMeta{
				Kind:          mailboxpb.RpcMeta_KIND_REQUEST,
				Service:       "test.v1.RoundService",
				Method:        "NotifyRoundStarted",
				CorrelationId: "corr-123",
				ReplyTo:       "server-for-client-1",
			},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, original.Encode(&buf))

	decoded := &sendRPCMsg{}
	require.NoError(t, decoded.Decode(&buf))

	// Verify all envelope fields survived the round-trip.
	require.True(
		t, proto.Equal(original.Envelope, decoded.Envelope),
		"envelope mismatch after TLV round-trip",
	)
}
