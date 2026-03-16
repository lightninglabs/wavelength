package serverconn

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/darepo-client/mailbox/rpc"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
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
	_ context.Context, pkScript []byte, afterEventID uint64, limit uint32,
) (proto.Message, error) {

	return wrapperspb.String(fmt.Sprintf(
		"recipient:%x:%d:%d", pkScript, afterEventID, limit,
	)), nil
}

// BuildListVTXOsByScriptsRequest builds a deterministic proto body for
// VTXO-by-scripts query tests.
func (b *testDurableUnaryBuilder) BuildListVTXOsByScriptsRequest(
	_ context.Context, pkScripts [][]byte, afterCursor uint64, limit uint32,
) (proto.Message, error) {

	return wrapperspb.String(fmt.Sprintf(
		"vtxos:%d:%d:%d", len(pkScripts), afterCursor, limit,
	)), nil
}

// newTestConnector builds a ServerConnectionActor with in-memory test
// dependencies.
func newTestConnector(
	t *testing.T,
	dispatchers map[mailboxrpc.ServiceMethod]EnvelopeDispatcher,
) (*ServerConnectionActor, *inMemoryMailbox, *memCheckpointStore) {

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
func sendResponseToMailbox(
	t *testing.T, mb *inMemoryMailbox,
	recipientID, correlationID string, payload []byte,
) {

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
func sendRoutedResponseToMailbox(
	t *testing.T, mb *inMemoryMailbox, recipientID, correlationID,
	service, method string, payload []byte,
) {

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
	require.True(t, status.Ok, "send routed response failed: %s",
		status.Message)
}

// sendEventToMailbox injects a KIND_EVENT envelope into the given mailbox
// addressed to recipientID with the specified service/method.
func sendEventToMailbox(
	t *testing.T, mb *inMemoryMailbox,
	recipientID, service, method string,
) {

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
		t, "ListOORRecipientEventsByScript",
		env.GetRpc().GetMethod(),
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
		{Service: "test.Svc", Method: "DoThing"}: func(
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
		t, mb, "client-1", string(corrID),
		[]byte("response-payload"),
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
		{Service: "test.Svc", Method: "Unary"}: func(
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
		{Service: "test.Svc", Method: "Fail"}: func(
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
		{Service: "test.Svc", Method: "DoThing"}: func(
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
		Message: &testServerMessage{value: "same-event"},
	}
	req2 := &SendClientEventRequest{
		Message: &testServerMessage{value: "same-event"},
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
		{Service: "test.Svc", Method: "Batch"}: func(
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
		t, 2, dispatchCounts[2],
		"second envelope should be dispatched exactly twice "+
			"(1 fail + 1 retry)",
	)
}

// TestRetryDelay verifies the exponential backoff formula with jitter.
func TestRetryDelay(t *testing.T) {
	t.Parallel()

	base := 100 * time.Millisecond
	maxDelay := 5 * time.Second

	// First attempt should be approximately base (within jitter range).
	d := retryDelay(base, maxDelay, 1)
	require.GreaterOrEqual(t, d, base/2)
	require.LessOrEqual(t, d, base)

	// High attempt should be capped at maxDelay.
	d = retryDelay(base, maxDelay, 100)
	require.LessOrEqual(t, d, maxDelay)
	require.GreaterOrEqual(t, d, maxDelay/2)
}

// TestRetryDelay_DefaultsOnZero verifies that zero base/max get defaults.
func TestRetryDelay_DefaultsOnZero(t *testing.T) {
	t.Parallel()

	d := retryDelay(0, 0, 1)
	require.Greater(t, d, time.Duration(0))
}
