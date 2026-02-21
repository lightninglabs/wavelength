package clientconn

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/lightninglabs/darepo-client/baselib/actor"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	"github.com/lightninglabs/darepo/clientconn/roundtestpb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// ---------------------------------------------------------------------------
// Integration tests with real SQLite DeliveryStore
//
// These tests mirror the e2e tests but swap the in-memory checkpoint store
// for a real SQLite-backed actor delivery store. This exercises the full
// persistence path: DurableActor TLV encoding → SQLite enqueue → lease →
// behavior Receive → ack.
// ---------------------------------------------------------------------------

// TestIntegrationSQLiteEgress verifies the full server-to-client egress path
// using a real SQLite delivery store. The DurableActor persists the
// sendEventMsg in SQLite, leases it, processes it through the behavior, and
// acks it — all with real SQL transactions.
func TestIntegrationSQLiteEgress(t *testing.T) {
	t.Parallel()

	mb := newInMemoryMailbox()
	store := newRealDeliveryStore(t)
	cfg := newTestPerClientConfig(mb, store)

	bridge := NewClientsConnBridge()
	clientID := ClientID("client-1")

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	_, err := bridge.RegisterClient(ctx, clientID, cfg)
	require.NoError(t, err)
	defer bridge.Stop()

	// Start a test client simulator pulling from the client mailbox.
	tc := newTestClient(mb, "client-1", "server-for-client-1")
	go tc.run(ctx)

	// Server pushes a RoundStartedEvent via the bridge.
	err = bridge.Tell(ctx, &SendServerEventRequest{
		Message: &roundStartedServerMsg{
			targetClientID: clientID,
			RoundID:        "round-sqlite-1",
		},
	})
	require.NoError(t, err)

	// Poll the transcript until the test client receives the event.
	require.Eventually(t, func() bool {
		return tc.received.entryCount() >= 1
	}, 10*time.Second, 50*time.Millisecond)

	env := tc.received.all()[0]
	require.NotNil(t, env.Rpc)
	require.Equal(t,
		mailboxpb.RpcMeta_KIND_EVENT, env.Rpc.Kind,
	)

	var event roundtestpb.RoundStartedEvent
	err = proto.Unmarshal(env.Body.Value, &event)
	require.NoError(t, err)
	require.Equal(t, "round-sqlite-1", event.RoundId)
}

// TestIntegrationSQLiteMultiClient registers 3 clients each with its own
// SQLite-backed delivery store and verifies that events are delivered to
// the correct client. Each client's DurableActor uses independent SQLite
// persistence, ensuring no cross-contamination between client states.
func TestIntegrationSQLiteMultiClient(t *testing.T) {
	t.Parallel()

	mb := newInMemoryMailbox()
	bridge := NewClientsConnBridge()

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	type clientEntry struct {
		id ClientID
		tc *testClient
	}

	clients := make([]clientEntry, 3)
	for i := range clients {
		cid := ClientID(fmt.Sprintf("client-%d", i))
		localMB := fmt.Sprintf("server-for-client-%d", i)
		remoteMB := fmt.Sprintf("client-%d", i)

		// Each client gets its own SQLite database for the
		// delivery store, mirroring production where each
		// client's DurableActor state is independent.
		store := newRealDeliveryStore(t)

		cfg := DefaultPerClientConfig()
		cfg.Edge = &fakeMailboxServiceClient{mb: mb}
		cfg.Store = store
		cfg.LocalMailboxID = localMB
		cfg.RemoteMailboxID = remoteMB
		cfg.ProtocolVersion = 1
		cfg.PullWaitTimeout = 50 * time.Millisecond
		cfg.Dispatchers = DispatcherMap{
			{Service: "test.v1.Noop", Method: "Noop"}: func(
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
			id: cid,
			tc: tc,
		}
	}
	defer bridge.Stop()

	// Push a unique event to each client.
	for i, c := range clients {
		err := bridge.Tell(ctx, &SendServerEventRequest{
			Message: &roundStartedServerMsg{
				targetClientID: c.id,
				RoundID: fmt.Sprintf(
					"round-multi-%d", i,
				),
			},
		})
		require.NoError(t, err)
	}

	// Verify each client received its own event.
	for i, c := range clients {
		expectedRound := fmt.Sprintf("round-multi-%d", i)

		require.Eventually(t, func() bool {
			return c.tc.received.entryCount() >= 1
		}, 10*time.Second, 50*time.Millisecond,
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

// TestIntegrationSQLiteUnaryRPC verifies a full unary RPC round-trip using
// a real SQLite delivery store. The server sends a NotifyRoundStarted RPC
// to the client via the per-client UnaryFacade, the test client processes
// it and responds, and the server receives the typed response. This
// exercises the full persistence and ingress/egress paths with real SQL
// transactions.
func TestIntegrationSQLiteUnaryRPC(t *testing.T) {
	t.Parallel()

	mb := newInMemoryMailbox()
	store := newRealDeliveryStore(t)
	cfg := newTestPerClientConfig(mb, store)

	bridge := NewClientsConnBridge()
	clientID := ClientID("client-1")

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
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

	unary, ok := bridge.GetUnary(clientID)
	require.True(t, ok)

	client := roundtestpb.NewRoundNotifyServiceMailboxClient(unary)

	// Issue a unary NotifyRoundStarted RPC.
	resp, err := client.NotifyRoundStarted(
		ctx, &roundtestpb.RoundStartedNotification{
			RoundId: "round-sqlite-rpc",
		},
	)
	require.NoError(t, err)
	require.NotNil(t, resp)

	// Also exercise NotifyBatchReady.
	batchResp, err := client.NotifyBatchReady(
		ctx, &roundtestpb.BatchReadyNotification{
			RoundId:   "round-sqlite-rpc",
			BatchData: []byte("sqlite-batch-data"),
		},
	)
	require.NoError(t, err)
	require.NotNil(t, batchResp)
}

// TestIntegrationSQLiteUnaryRPCError verifies that gRPC errors are properly
// transported through the mailbox envelope headers when using a real SQLite
// delivery store.
func TestIntegrationSQLiteUnaryRPCError(t *testing.T) {
	t.Parallel()

	mb := newInMemoryMailbox()
	store := newRealDeliveryStore(t)
	cfg := newTestPerClientConfig(mb, store)

	bridge := NewClientsConnBridge()
	clientID := ClientID("client-1")

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
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
			RoundId: "round-sqlite-err",
		},
	)
	require.Error(t, notifyErr)

	st, ok := status.FromError(notifyErr)
	require.True(t, ok, "expected gRPC status error, got: %v",
		notifyErr,
	)
	require.Equal(t, codes.NotFound, st.Code())
	require.Contains(t, st.Message(), "round not found")
}

// TestIntegrationSQLiteBidirectional verifies a combined scenario using real
// SQLite: server pushes an event, client sends an event back via the
// EventRouter, and server sends a unary RPC — all with the same
// SQLite-backed DurableActor.
func TestIntegrationSQLiteBidirectional(t *testing.T) {
	t.Parallel()

	mb := newInMemoryMailbox()
	store := newRealDeliveryStore(t)
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

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
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
			RoundID:        "round-sqlite-bidir",
		},
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return tc.received.entryCount() >= 1
	}, 10*time.Second, 50*time.Millisecond)

	env := tc.received.all()[0]
	var biEvent roundtestpb.RoundStartedEvent
	require.NoError(t, proto.Unmarshal(
		env.Body.Value, &biEvent,
	))
	require.Equal(t, "round-sqlite-bidir", biEvent.RoundId)

	// Phase 2: Client sends an event to the server.
	tc.pushEvent(
		t,
		"roundtest.v1.RoundNotifyService", "ClientJoined",
		&roundtestpb.ClientJoinedEvent{
			ClientId: "client-1",
			RoundId:  "round-sqlite-bidir",
		},
	)

	require.Eventually(t, func() bool {
		return joinBehavior.received.entryCount() >= 1
	}, 10*time.Second, 50*time.Millisecond)

	joinMsg := joinBehavior.received.all()[0]
	require.Equal(t, "client-1", joinMsg.ClientIDVal)
	require.Equal(t, "round-sqlite-bidir", joinMsg.RoundID)

	// Phase 3: Server sends a unary RPC to the client.
	unary, ok := bridge.GetUnary(clientID)
	require.True(t, ok)

	rpcClient := roundtestpb.NewRoundNotifyServiceMailboxClient(
		unary,
	)
	resp, err := rpcClient.NotifyRoundStarted(
		ctx, &roundtestpb.RoundStartedNotification{
			RoundId: "round-sqlite-bidir",
		},
	)
	require.NoError(t, err)
	require.NotNil(t, resp)
}
