package serverconn

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/lightninglabs/darepo-client/baselib/actor"
	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/darepo-client/mailbox/rpc"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// testServerMessage is a minimal ServerMessage implementation for egress
// tests.
type testServerMessage struct {
	value string
}

// ServiceMethod returns deterministic routing metadata for tests.
func (m *testServerMessage) ServiceMethod() mailboxrpc.ServiceMethod {
	return mailboxrpc.ServiceMethod{
		Service: testEventService,
		Method:  testEventMethod,
	}
}

// ToProto converts the test message to a protobuf payload.
func (m *testServerMessage) ToProto() fn.Result[proto.Message] {
	return fn.Ok[proto.Message](wrapperspb.String(m.value))
}

// testDurableUnaryBuilder is a minimal builder stub for durable query tests.
type testDurableUnaryBuilder struct{}

// BuildListOORRecipientEventsByScriptRequest builds a deterministic proto body
// for recipient-events query tests.
func (b *testDurableUnaryBuilder) BuildListOORRecipientEventsByScriptRequest(
	_ context.Context, pkScript []byte, afterEventID uint64, limit uint32) (
	proto.Message, error) {

	return wrapperspb.String(
		fmt.Sprintf("recipient:%x:%d:%d", pkScript, afterEventID,
			limit),
	), nil
}

// BuildListVTXOsByScriptsRequest builds a deterministic proto body for
// VTXO-by-scripts query tests.
func (b *testDurableUnaryBuilder) BuildListVTXOsByScriptsRequest(
	_ context.Context, pkScripts [][]byte, afterCursor []byte,
	limit uint32) (proto.Message, error) {

	return wrapperspb.String(
		fmt.Sprintf(
			"vtxos:%d:%x:%d", len(pkScripts), afterCursor, limit,
		),
	), nil
}

// newTestConnector builds a ServerConnectionActor with in-memory test
// dependencies.
func newTestConnector(t *testing.T,
	dispatchers map[mailboxrpc.ServiceMethod]EnvelopeDispatcher) (
	*ServerConnectionActor, *inMemoryMailbox, *memCheckpointStore) {

	t.Helper()

	mb := newInMemoryMailbox()
	store := newMemCheckpointStore()

	cfg := newTestConnectorConfig(mb, store)
	cfg.Dispatchers = dispatchers
	cfg.RetryBaseDelay = 10 * time.Millisecond
	cfg.RetryMaxDelay = 50 * time.Millisecond

	actor := NewServerConnectionActor(cfg)

	return actor, mb, store
}

// sendResponseToMailbox injects a KIND_RESPONSE envelope into the given
// mailbox addressed to recipientID.
func sendResponseToMailbox(t *testing.T, mb *inMemoryMailbox, recipientID,
	correlationID string, payload []byte) {

	t.Helper()

	body := &anypb.Any{
		TypeUrl: "test/response",
		Value:   payload,
	}

	env := &mailboxpb.Envelope{
		ProtocolVersion: 1,
		Sender:          "server-1",
		Recipient:       recipientID,
		Body:            body,
		Rpc: &mailboxpb.RpcMeta{
			Kind:          mailboxpb.RpcMeta_KIND_RESPONSE,
			CorrelationId: correlationID,
			ReplyTo:       "server-1",
		},
	}

	status := mb.send(env)
	require.True(t, status.Ok, "send response failed: %s", status.Message)
}

// sendRoutedResponseToMailbox injects a KIND_RESPONSE envelope that carries
// service/method metadata so ingress can durably dispatch it when no unary
// waiter is registered.
func sendRoutedResponseToMailbox(t *testing.T, mb *inMemoryMailbox, recipientID,
	correlationID, service, method string, payload []byte) {

	t.Helper()

	body := &anypb.Any{
		TypeUrl: "test/response",
		Value:   payload,
	}

	env := &mailboxpb.Envelope{
		ProtocolVersion: 1,
		Sender:          "server-1",
		Recipient:       recipientID,
		Body:            body,
		Rpc: &mailboxpb.RpcMeta{
			Kind:          mailboxpb.RpcMeta_KIND_RESPONSE,
			CorrelationId: correlationID,
			Service:       service,
			Method:        method,
			ReplyTo:       "server-1",
		},
	}

	status := mb.send(env)
	require.True(
		t, status.Ok, "send routed response failed: %s", status.Message,
	)
}

// sendRoutedErrorResponseToMailbox injects a KIND_RESPONSE envelope that
// carries service/method metadata and a gRPC error encoded in headers, but no
// response body.
func sendRoutedErrorResponseToMailbox(t *testing.T, mb *inMemoryMailbox,
	recipientID, correlationID, service, method string, err error) {

	t.Helper()

	env := &mailboxpb.Envelope{
		ProtocolVersion: 1,
		Sender:          "server-1",
		Recipient:       recipientID,
		Headers:         mailboxrpc.EncodeErrorHeaders(err),
		Rpc: &mailboxpb.RpcMeta{
			Kind:          mailboxpb.RpcMeta_KIND_RESPONSE,
			CorrelationId: correlationID,
			Service:       service,
			Method:        method,
			ReplyTo:       "server-1",
		},
	}

	status := mb.send(env)
	require.True(
		t, status.Ok, "send routed error response failed: %s",
		status.Message,
	)
}

// sendEventToMailbox injects a KIND_EVENT envelope into the given mailbox
// addressed to recipientID with the specified service/method.
func sendEventToMailbox(t *testing.T, mb *inMemoryMailbox, recipientID, service,
	method string) {

	t.Helper()

	body, err := anypb.New(wrapperspb.String("test-event"))
	require.NoError(t, err)

	env := &mailboxpb.Envelope{
		ProtocolVersion: 1,
		Sender:          "server-1",
		Recipient:       recipientID,
		Body:            body,
		Rpc: &mailboxpb.RpcMeta{
			Kind:    mailboxpb.RpcMeta_KIND_EVENT,
			Service: service,
			Method:  method,
			ReplyTo: "server-1",
		},
	}

	status := mb.send(env)
	require.True(t, status.Ok, "send event failed: %s", status.Message)
}

// TestServerConnectionActor_SendListOORRecipientEventsByScriptRequest verifies
// the transport-native durable recipient-events query is built and sent as a
// unary mailbox request with the expected route metadata.
func TestServerConnectionActor_SendListOORRecipientEventsByScriptRequest(
	t *testing.T) {

	t.Parallel()

	actor, mb, _ := newTestConnector(t, nil)
	actor.cfg.DurableUnaryBuilder = &testDurableUnaryBuilder{}

	result := actor.Receive(
		t.Context(),
		&SendListOORRecipientEventsByScriptRequest{
			PkScript:      []byte{0x51, 0x20, 0x01},
			AfterEventID:  7,
			Limit:         1,
			CorrelationID: "corr-recipient",
		},
	)
	require.NoError(t, result.Err())

	mb.mu.Lock()
	require.Len(t, mb.mailboxes["server-1"], 1)
	env, ok := proto.Clone(
		mb.mailboxes["server-1"][0],
	).(*mailboxpb.Envelope)
	require.True(t, ok)
	mb.mu.Unlock()

	require.Equal(
		t, "arkrpc.IndexerService", env.GetRpc().GetService(),
	)
	require.Equal(
		t, "ListOORRecipientEventsByScript", env.GetRpc().GetMethod(),
	)
	require.Equal(
		t, "corr-recipient", env.GetRpc().GetCorrelationId(),
	)

	payload := &wrapperspb.StringValue{}
	require.NoError(t, env.GetBody().UnmarshalTo(payload))
	require.Equal(t, "recipient:512001:7:1", payload.Value)
}

// TestIngress_DispatchAndAck verifies that the ingress loop pulls envelopes,
// dispatches them via the dispatch table, and acks the remote mailbox.
func TestIngress_DispatchAndAck(t *testing.T) {
	t.Parallel()

	var (
		dispatched   []*mailboxpb.Envelope
		dispatchedMu sync.Mutex
	)

	dispatchers := map[mailboxrpc.ServiceMethod]EnvelopeDispatcher{
		{
			Service: "test.Svc",
			Method:  "DoThing",
		}: func(
			ctx context.Context,
			env *mailboxpb.Envelope,
		) error {

			dispatchedMu.Lock()
			dispatched = append(dispatched, env)
			dispatchedMu.Unlock()

			return nil
		},
	}

	actor, mb, _ := newTestConnector(t, dispatchers)

	// Inject 3 events into the client's mailbox.
	for i := 0; i < 3; i++ {
		sendEventToMailbox(
			t, mb, "client-1", "test.Svc", "DoThing",
		)
	}

	// Start ingress.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	require.NoError(t, actor.StartIngress(ctx))
	defer actor.StopIngress()

	// Wait for dispatch to process all 3 envelopes.
	require.Eventually(t, func() bool {
		dispatchedMu.Lock()
		defer dispatchedMu.Unlock()

		return len(dispatched) == 3
	}, 5*time.Second, 10*time.Millisecond)

	// The ack watermark should have advanced past the envelopes.
	require.Eventually(t, func() bool {
		return mb.getAckedUpTo("client-1") > 0
	}, 5*time.Second, 10*time.Millisecond)
}

// TestIngress_ResponseDelivery verifies that KIND_RESPONSE envelopes are
// delivered to registered waiters via the response registry.
func TestIngress_ResponseDelivery(t *testing.T) {
	t.Parallel()

	actor, mb, _ := newTestConnector(t, nil)

	// Register a waiter for a specific correlation ID.
	corrID := CorrelationID("test-corr-123")
	future := actor.RegisterWaiter(corrID)

	// Start ingress.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	require.NoError(t, actor.StartIngress(ctx))
	defer actor.StopIngress()

	// Inject a response envelope.
	sendResponseToMailbox(
		t, mb, "client-1", string(corrID), []byte("response-payload"),
	)

	// The waiter should receive the envelope.
	awaitCtx, awaitCancel := context.WithTimeout(
		t.Context(), 5*time.Second,
	)
	defer awaitCancel()

	env := future.Await(awaitCtx).UnwrapOrFail(t)
	require.NotNil(t, env)
	require.Equal(
		t, string(corrID), env.Rpc.CorrelationId,
	)
}

// TestIngress_ResponseDispatchHeaderOnlyError verifies that routed response
// dispatch still reaches EventRouter handlers when the server encodes a gRPC
// error in headers and intentionally omits the response body.
func TestIngress_ResponseDispatchHeaderOnlyError(t *testing.T) {
	t.Parallel()

	mb := newInMemoryMailbox()
	store := newMemCheckpointStore()
	system := actor.NewActorSystem()

	greetingKey := actor.NewServiceKey[*helloStartedMsg, struct{}](
		"greeting-actor",
	)
	behavior := &greetingBehavior{
		received: make(chan *helloStartedMsg, 1),
	}
	actor.RegisterWithSystem(system, "greeting-1", greetingKey, behavior)

	router := NewEventRouter(system)
	AddEnvelopeRoute(
		router, EnvelopeRouteConfig[*helloStartedMsg, struct{}]{
			Service: "test.Svc",
			Method:  "Unary",
			NewEvent: func() proto.Message {
				return &wrapperspb.StringValue{}
			},
			Key: greetingKey,
			Adapt: func(env *mailboxpb.Envelope, _ proto.Message) (
				*helloStartedMsg, error) {

				rpcErr := mailboxrpc.DecodeErrorHeaders(
					env.GetHeaders(),
				)
				if rpcErr == nil {
					return nil, fmt.Errorf("expected " +
						"encoded rpc error")
				}

				return &helloStartedMsg{
					SessionID: rpcErr.Error(),
				}, nil
			},
		},
	)

	cfg := newTestConnectorConfig(mb, store)
	cfg.Dispatchers = router.AsDispatcherMap()
	connector := NewServerConnectionActor(cfg)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	require.NoError(t, connector.StartIngress(ctx))
	defer connector.StopIngress()

	sendRoutedErrorResponseToMailbox(
		t, mb, "client-1", "corr-routed", "test.Svc", "Unary",
		status.Error(codes.Internal, "boom"),
	)

	select {
	case msg := <-behavior.received:
		require.Contains(t, msg.SessionID, "boom")

	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for routed error response dispatch")
	}
}

// TestIngressAckHandledResponseWithoutActorDelivery verifies a routed response
// can be consumed by the route and acked without enqueueing an actor message.
func TestIngressAckHandledResponseWithoutActorDelivery(t *testing.T) {
	t.Parallel()

	mb := newInMemoryMailbox()
	store := newMemCheckpointStore()
	system := actor.NewActorSystem()

	routeKey := actor.NewServiceKey[*helloStartedMsg, struct{}](
		"greeting-actor",
	)
	router := NewEventRouter(system)
	AddEnvelopeRoute(
		router, EnvelopeRouteConfig[*helloStartedMsg, struct{}]{
			Service: "test.Svc",
			Method:  "Unary",
			NewEvent: func() proto.Message {
				return &wrapperspb.StringValue{}
			},
			Key: routeKey,
			Adapt: func(_ *mailboxpb.Envelope, _ proto.Message) (
				*helloStartedMsg, error) {

				return nil, ErrEnvelopeHandled
			},
		},
	)

	cfg := newTestConnectorConfig(mb, store)
	cfg.Dispatchers = router.AsDispatcherMap()
	cfg.RetryBaseDelay = 10 * time.Millisecond
	cfg.RetryMaxDelay = 50 * time.Millisecond

	connector := NewServerConnectionActor(cfg)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	require.NoError(t, connector.StartIngress(ctx))
	defer connector.StopIngress()

	body, err := anypb.New(wrapperspb.String("stale"))
	require.NoError(t, err)

	status := mb.send(&mailboxpb.Envelope{
		ProtocolVersion: 1,
		Sender:          "server-1",
		Recipient:       "client-1",
		Body:            body,
		Rpc: &mailboxpb.RpcMeta{
			Kind:          mailboxpb.RpcMeta_KIND_RESPONSE,
			CorrelationId: "stale-corr",
			Service:       "test.Svc",
			Method:        "Unary",
			ReplyTo:       "server-1",
		},
	})
	require.True(t, status.Ok, "send response failed: %s", status.Message)

	require.Eventually(t, func() bool {
		return mb.getAckedUpTo("client-1") > 0
	}, 5*time.Second, 10*time.Millisecond)
}

// TestIngress_ResponseDispatchWithoutWaiter verifies that a KIND_RESPONSE
// envelope without an in-memory unary waiter falls back to the durable
// dispatcher map keyed by service and method.
func TestIngress_ResponseDispatchWithoutWaiter(t *testing.T) {
	t.Parallel()

	var (
		dispatched   []*mailboxpb.Envelope
		dispatchedMu sync.Mutex
	)

	dispatchers := map[mailboxrpc.ServiceMethod]EnvelopeDispatcher{
		{
			Service: "test.Svc",
			Method:  "Unary",
		}: func(
			ctx context.Context,
			env *mailboxpb.Envelope,
		) error {

			dispatchedMu.Lock()
			dispatched = append(dispatched, env)
			dispatchedMu.Unlock()

			return nil
		},
	}

	actor, mb, _ := newTestConnector(t, dispatchers)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	require.NoError(t, actor.StartIngress(ctx))
	defer actor.StopIngress()

	sendRoutedResponseToMailbox(
		t, mb, "client-1", "corr-routed", "test.Svc", "Unary",
		[]byte("response-payload"),
	)

	require.Eventually(t, func() bool {
		dispatchedMu.Lock()
		defer dispatchedMu.Unlock()

		return len(dispatched) == 1
	}, 5*time.Second, 10*time.Millisecond)
}

// TestIngress_NoAckOnDispatchFailure verifies that when a dispatcher returns
// an error, the ack watermark does not advance past the failed envelope.
func TestIngress_NoAckOnDispatchFailure(t *testing.T) {
	t.Parallel()

	var callCount int
	var callCountMu sync.Mutex

	dispatchers := map[mailboxrpc.ServiceMethod]EnvelopeDispatcher{
		{
			Service: "test.Svc",
			Method:  "Fail",
		}: func(
			ctx context.Context,
			env *mailboxpb.Envelope,
		) error {

			callCountMu.Lock()
			callCount++
			count := callCount
			callCountMu.Unlock()

			// Fail the first attempt, succeed thereafter.
			if count == 1 {
				return &statusError{
					Op: "dispatch",
					Status: &mailboxpb.Status{
						Ok:      false,
						Code:    "INTERNAL",
						Message: "test failure",
					},
				}
			}

			return nil
		},
	}

	actor, mb, _ := newTestConnector(t, dispatchers)

	// Inject one event.
	sendEventToMailbox(t, mb, "client-1", "test.Svc", "Fail")

	// Start ingress.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	require.NoError(t, actor.StartIngress(ctx))
	defer actor.StopIngress()

	// The dispatcher should eventually succeed on retry.
	require.Eventually(t, func() bool {
		callCountMu.Lock()
		defer callCountMu.Unlock()

		return callCount >= 2
	}, 5*time.Second, 10*time.Millisecond)

	// The ack should eventually advance after the retry succeeds.
	require.Eventually(t, func() bool {
		return mb.getAckedUpTo("client-1") > 0
	}, 5*time.Second, 10*time.Millisecond)
}

// TestIngress_Shutdown_NoGoroutineLeak verifies that StopIngress cleanly
// terminates the ingress loop goroutine.
func TestIngress_Shutdown_NoGoroutineLeak(t *testing.T) {
	t.Parallel()

	actor, _, _ := newTestConnector(t, nil)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	require.NoError(t, actor.StartIngress(ctx))

	// Give the loop a moment to start.
	time.Sleep(50 * time.Millisecond)

	// StopIngress should return promptly.
	done := make(chan struct{})
	go func() {
		actor.StopIngress()
		close(done)
	}()

	select {
	case <-done:
		// Clean shutdown.

	case <-time.After(5 * time.Second):
		t.Fatal("StopIngress did not return within timeout")
	}
}

// TestIngress_CheckpointSurvivesRestart verifies that after processing and
// acking envelopes, the checkpoint can be loaded to restore state.
func TestIngress_CheckpointSurvivesRestart(t *testing.T) {
	t.Parallel()

	dispatchers := map[mailboxrpc.ServiceMethod]EnvelopeDispatcher{
		{
			Service: "test.Svc",
			Method:  "DoThing",
		}: func(
			ctx context.Context,
			env *mailboxpb.Envelope,
		) error {

			return nil
		},
	}

	actor, mb, store := newTestConnector(t, dispatchers)

	// Inject an event.
	sendEventToMailbox(t, mb, "client-1", "test.Svc", "DoThing")

	// Run ingress long enough to process and checkpoint.
	ctx, cancel := context.WithCancel(t.Context())
	require.NoError(t, actor.StartIngress(ctx))

	require.Eventually(t, func() bool {
		return mb.getAckedUpTo("client-1") > 0
	}, 5*time.Second, 10*time.Millisecond)

	cancel()
	actor.StopIngress()

	// Verify checkpoint was persisted.
	actorID := "serverconn-client-1"
	cp, err := store.LoadCheckpoint(t.Context(), actorID)
	require.NoError(t, err)
	require.NotNil(t, cp, "checkpoint should be persisted")
	require.NotEmpty(t, cp.StateData)
}

// TestEgress_EventRetriesPreserveIdempotencyKey verifies that egress sends for
// the same semantic event use stable message and idempotency identifiers.
func TestEgress_EventRetriesPreserveIdempotencyKey(t *testing.T) {
	t.Parallel()

	actor, mb, _ := newTestConnector(t, nil)

	req1 := &SendClientEventRequest{
		Message: &testServerMessage{
			value: "same-event",
		},
	}
	req2 := &SendClientEventRequest{
		Message: &testServerMessage{
			value: "same-event",
		},
	}

	require.NoError(t, actor.Receive(t.Context(), req1).Err())
	require.NoError(t, actor.Receive(t.Context(), req2).Err())

	mb.mu.Lock()
	envs := append(
		[]*mailboxpb.Envelope(nil), mb.mailboxes["server-1"]...,
	)
	mb.mu.Unlock()

	require.Len(t, envs, 2)
	require.NotEmpty(t, envs[0].MsgId)
	require.NotEmpty(t, envs[0].IdempotencyKey)
	require.Equal(t, envs[0].MsgId, envs[1].MsgId)
	require.Equal(
		t, envs[0].IdempotencyKey, envs[1].IdempotencyKey,
	)
	require.NotNil(t, envs[0].Rpc)
	require.NotNil(t, envs[1].Rpc)
	require.Equal(t, testEventService, envs[0].Rpc.Service)
	require.Equal(t, testEventMethod, envs[0].Rpc.Method)
	require.Equal(t, testEventService, envs[1].Rpc.Service)
	require.Equal(t, testEventMethod, envs[1].Rpc.Method)
}

// TestEventRoutingMetadata verifies how SendClientEventRequest route fields are
// resolved when explicit values and ServerMessage metadata are combined.
func TestEventRoutingMetadata(t *testing.T) {
	t.Parallel()

	service, method := eventRoutingMetadata(nil)
	require.Equal(t, "", service)
	require.Equal(t, "", method)

	service, method = eventRoutingMetadata(&SendClientEventRequest{
		Service: "svc.explicit",
		Method:  "method.explicit",
		Message: &testServerMessage{value: "ignored"},
	})
	require.Equal(t, "svc.explicit", service)
	require.Equal(t, "method.explicit", method)

	service, method = eventRoutingMetadata(&SendClientEventRequest{
		Message: &testServerMessage{value: "fallback"},
	})
	require.Equal(t, testEventService, service)
	require.Equal(t, testEventMethod, method)

	service, method = eventRoutingMetadata(&SendClientEventRequest{
		Service: "svc.partial",
		Message: &testServerMessage{value: "fill-method"},
	})
	require.Equal(t, "svc.partial", service)
	require.Equal(t, testEventMethod, method)

	service, method = eventRoutingMetadata(&SendClientEventRequest{
		Method:  "method.partial",
		Message: &testServerMessage{value: "fill-service"},
	})
	require.Equal(t, testEventService, service)
	require.Equal(t, "method.partial", method)

	service, method = eventRoutingMetadata(&SendClientEventRequest{
		Service: "svc.only",
	})
	require.Equal(t, "svc.only", service)
	require.Equal(t, "", method)
}

// TestIngress_PartialDispatch_NoDuplicateRedelivery verifies that when
// a batch dispatch fails mid-way, the already-dispatched envelopes are
// not re-dispatched on the next loop iteration. This is a regression test
// for the off-by-one where the inclusive event_seq returned on the error
// path was used directly as PullCursor, causing the last committed
// envelope to be re-pulled and re-dispatched.
func TestIngress_PartialDispatch_NoDuplicateRedelivery(t *testing.T) {
	t.Parallel()

	var (
		// Track dispatch count per event_seq to detect duplicates.
		dispatchCounts   = make(map[uint64]int)
		dispatchCountsMu sync.Mutex
		callCount        int
	)

	dispatchers := map[mailboxrpc.ServiceMethod]EnvelopeDispatcher{
		{
			Service: "test.Svc",
			Method:  "Batch",
		}: func(
			ctx context.Context,
			env *mailboxpb.Envelope,
		) error {

			dispatchCountsMu.Lock()
			callCount++
			count := callCount
			dispatchCounts[env.EventSeq]++
			dispatchCountsMu.Unlock()

			// Fail on the second envelope in the first batch.
			// The first envelope (count==1) succeeds, and the
			// second (count==2) fails. On retry, we expect the
			// second to be dispatched (count==3) but NOT the
			// first again.
			if count == 2 {
				return &statusError{
					Op: "dispatch",
					Status: &mailboxpb.Status{
						Ok:      false,
						Code:    "INTERNAL",
						Message: "injected failure",
					},
				}
			}

			return nil
		},
	}

	actor, mb, _ := newTestConnector(t, dispatchers)

	// Inject 2 events so the batch has 2 envelopes.
	sendEventToMailbox(t, mb, "client-1", "test.Svc", "Batch")
	sendEventToMailbox(t, mb, "client-1", "test.Svc", "Batch")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	require.NoError(t, actor.StartIngress(ctx))
	defer actor.StopIngress()

	// Wait until the second envelope has been retried (dispatched
	// twice: once failed, once succeeded).
	require.Eventually(t, func() bool {
		dispatchCountsMu.Lock()
		defer dispatchCountsMu.Unlock()

		return dispatchCounts[2] >= 2
	}, 5*time.Second, 10*time.Millisecond)

	dispatchCountsMu.Lock()
	defer dispatchCountsMu.Unlock()

	// The first envelope (event_seq=1) must have been dispatched
	// exactly once — not re-dispatched after the partial failure.
	require.Equal(
		t, 1, dispatchCounts[1],
		"first envelope should be dispatched exactly once",
	)

	// The second envelope (event_seq=2) should be dispatched
	// exactly twice: once failed, once succeeded on retry.
	require.Equal(
		t, 2, dispatchCounts[2], "second envelope should be "+
			"dispatched exactly twice (1 fail + 1 retry)",
	)
}

// Backoff formula tests now live in serverconn/mailboxpull/pull_test.go
// (TestRetryDelayClampsToMax, TestRetryDelayUsesDefaults) -- the formula
// itself moved to that subpackage so the SDK pull loop and this actor's
// ingress loop share one schedule.
