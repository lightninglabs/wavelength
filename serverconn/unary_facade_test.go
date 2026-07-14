package serverconn

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// TestUnaryFacade_SendRPC verifies that SendRPC constructs an envelope and
// sends it through the mailbox edge, returning the correlation and idempotency
// identifiers.
func TestUnaryFacade_SendRPC(t *testing.T) {
	t.Parallel()

	actor, mb, _ := newTestConnector(t, nil)
	facade := NewUnaryFacade(actor)

	method := mailboxrpc.ServiceMethod{
		Service: "test.Svc",
		Method:  "GetInfo",
	}

	req := wrapperspb.String("hello")

	result, err := facade.SendRPC(
		t.Context(), method, req, mailboxrpc.RPCOptions{},
	)
	require.NoError(t, err)
	require.NotEmpty(t, result.CorrelationID)
	require.NotEmpty(t, result.IdempotencyKey)

	// Verify the envelope was delivered to the server mailbox.
	mb.mu.Lock()
	envs := mb.mailboxes["server-1"]
	mb.mu.Unlock()

	require.Len(t, envs, 1)
	require.Equal(t, "client-1", envs[0].Sender)
	require.Equal(t, "server-1", envs[0].Recipient)
	require.Equal(t,
		mailboxpb.RpcMeta_KIND_REQUEST, envs[0].Rpc.Kind,
	)
	require.Equal(t, "test.Svc", envs[0].Rpc.Service)
	require.Equal(t, "GetInfo", envs[0].Rpc.Method)
	require.Equal(t,
		result.CorrelationID, envs[0].Rpc.CorrelationId,
	)
}

// TestUnaryFacade_SendRPC_ExplicitOptions verifies that caller-provided
// correlation ID and idempotency key are preserved.
func TestUnaryFacade_SendRPC_ExplicitOptions(t *testing.T) {
	t.Parallel()

	actor, _, _ := newTestConnector(t, nil)
	facade := NewUnaryFacade(actor)

	method := mailboxrpc.ServiceMethod{
		Service: "test.Svc",
		Method:  "GetInfo",
	}

	opts := mailboxrpc.RPCOptions{
		CorrelationID:  "my-corr-id",
		IdempotencyKey: "my-idemp-key",
	}

	result, err := facade.SendRPC(
		t.Context(), method, wrapperspb.String("hello"), opts,
	)
	require.NoError(t, err)
	require.Equal(t, "my-corr-id", result.CorrelationID)
	require.Equal(t, "my-idemp-key", result.IdempotencyKey)
}

// TestUnaryFacade_AwaitRPC verifies the full send-await round trip where the
// ingress loop delivers the response to the facade waiter.
func TestUnaryFacade_AwaitRPC(t *testing.T) {
	t.Parallel()

	actor, mb, _ := newTestConnector(t, nil)
	facade := NewUnaryFacade(actor)

	// Start ingress so responses can be pulled and delivered.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	require.NoError(t, actor.StartIngress(ctx))
	defer actor.StopIngress()

	// Send an RPC request.
	method := mailboxrpc.ServiceMethod{
		Service: "test.Svc",
		Method:  "GetInfo",
	}

	result, err := facade.SendRPC(
		t.Context(), method, wrapperspb.String("request"),
		mailboxrpc.RPCOptions{},
	)
	require.NoError(t, err)

	// Simulate a server response by injecting a KIND_RESPONSE envelope
	// into the client's mailbox with the matching correlation ID.
	responseMsg := wrapperspb.String("world")
	responseBytes, err := proto.Marshal(responseMsg)
	require.NoError(t, err)

	sendResponseToMailbox(
		t, mb, "client-1", result.CorrelationID, responseBytes,
	)

	// Await should unmarshal the response.
	var resp wrapperspb.StringValue
	err = facade.AwaitRPC(
		t.Context(), result.CorrelationID, &resp,
	)
	require.NoError(t, err)
	require.Equal(t, "world", resp.Value)
}

// TestUnaryFacade_ResponseBeforeAwait verifies that a response arriving before
// AwaitRPC is still delivered to the caller.
func TestUnaryFacade_ResponseBeforeAwait(t *testing.T) {
	t.Parallel()

	actor, mb, _ := newTestConnector(t, nil)
	facade := NewUnaryFacade(actor)

	// Start ingress so responses can be pulled and buffered.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	require.NoError(t, actor.StartIngress(ctx))
	defer actor.StopIngress()

	method := mailboxrpc.ServiceMethod{
		Service: "test.Svc",
		Method:  "GetInfo",
	}

	result, err := facade.SendRPC(
		t.Context(), method, wrapperspb.String("request"),
		mailboxrpc.RPCOptions{},
	)
	require.NoError(t, err)

	responseMsg := wrapperspb.String("early")
	responseBytes, err := proto.Marshal(responseMsg)
	require.NoError(t, err)

	sendResponseToMailbox(
		t, mb, "client-1", result.CorrelationID, responseBytes,
	)

	// Ensure ingress had a chance to pull and process the response before
	// we start awaiting it.
	require.Eventually(t, func() bool {
		return mb.getAckedUpTo("client-1") > 0
	}, 5*time.Second, 10*time.Millisecond)

	awaitCtx, awaitCancel := context.WithTimeout(
		t.Context(), 5*time.Second,
	)
	defer awaitCancel()

	var resp wrapperspb.StringValue
	err = facade.AwaitRPC(awaitCtx, result.CorrelationID, &resp)
	require.NoError(t, err)
	require.Equal(t, "early", resp.Value)
}

// TestUnaryFacade_AwaitRPC_CancelledContext verifies that AwaitRPC returns
// the context error when the context is cancelled.
func TestUnaryFacade_AwaitRPC_CancelledContext(t *testing.T) {
	t.Parallel()

	actor, _, _ := newTestConnector(t, nil)
	facade := NewUnaryFacade(actor)

	// Start ingress.
	ingressCtx, ingressCancel := context.WithCancel(
		t.Context(),
	)
	defer ingressCancel()

	require.NoError(t, actor.StartIngress(ingressCtx))
	defer actor.StopIngress()

	// Create a context that we cancel immediately.
	awaitCtx, awaitCancel := context.WithCancel(t.Context())
	awaitCancel()

	var resp wrapperspb.StringValue
	err := facade.AwaitRPC(awaitCtx, "no-such-corr", &resp)
	require.ErrorIs(t, err, context.Canceled)
}

// TestUnaryFacade_ConcurrentInflight verifies that multiple concurrent
// send/await pairs do not lose or misroute responses.
func TestUnaryFacade_ConcurrentInflight(t *testing.T) {
	t.Parallel()

	actor, mb, _ := newTestConnector(t, nil)
	facade := NewUnaryFacade(actor)

	// Start ingress.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	require.NoError(t, actor.StartIngress(ctx))
	defer actor.StopIngress()

	const numRequests = 20
	method := mailboxrpc.ServiceMethod{
		Service: "test.Svc",
		Method:  "Echo",
	}

	type roundTrip struct {
		corrID string
		input  string
	}

	// Send all requests first and collect correlation IDs.
	trips := make([]roundTrip, numRequests)
	for i := 0; i < numRequests; i++ {
		input := wrapperspb.String(
			fmt.Sprintf("req-%d", i),
		)

		result, err := facade.SendRPC(
			t.Context(), method, input, mailboxrpc.RPCOptions{},
		)
		require.NoError(t, err)

		trips[i] = roundTrip{
			corrID: result.CorrelationID,
			input:  input.Value,
		}
	}

	// Start all await goroutines first, then inject responses. This
	// ensures waiters are registered before responses arrive.
	var wg sync.WaitGroup
	errors := make([]error, numRequests)
	results := make([]string, numRequests)

	for i := 0; i < numRequests; i++ {
		i := i
		wg.Add(1)

		go func() {
			defer wg.Done()

			awaitCtx, awaitCancel := context.WithTimeout(
				t.Context(), 15*time.Second,
			)
			defer awaitCancel()

			var resp wrapperspb.StringValue
			errors[i] = facade.AwaitRPC(
				awaitCtx, trips[i].corrID, &resp,
			)
			results[i] = resp.Value
		}()
	}

	// Brief pause to let all waiters register before injecting
	// responses.
	time.Sleep(50 * time.Millisecond)

	// Inject responses in reverse order to stress routing.
	for i := numRequests - 1; i >= 0; i-- {
		responseMsg := wrapperspb.String("resp-" + trips[i].input)
		responseBytes, err := proto.Marshal(responseMsg)
		require.NoError(t, err)

		sendResponseToMailbox(
			t, mb, "client-1", trips[i].corrID, responseBytes,
		)
	}

	wg.Wait()

	for i := 0; i < numRequests; i++ {
		require.NoError(t, errors[i], "request %d failed", i)
		require.Equal(
			t, "resp-"+trips[i].input, results[i], "response "+
				"mismatch for request %d", i,
		)
	}
}

// TestUnaryFacade_HighConcurrencyOutOfOrder verifies that many in-flight unary
// requests can be resolved correctly when responses arrive out of order before
// AwaitRPC begins.
func TestUnaryFacade_HighConcurrencyOutOfOrder(t *testing.T) {
	t.Parallel()

	actor, mb, _ := newTestConnector(t, nil)
	facade := NewUnaryFacade(actor)

	ingressCtx, ingressCancel := context.WithCancel(t.Context())
	defer ingressCancel()

	require.NoError(t, actor.StartIngress(ingressCtx))
	defer actor.StopIngress()

	const numRequests = 200
	method := mailboxrpc.ServiceMethod{
		Service: "test.Svc",
		Method:  "Bulk",
	}

	type requestPair struct {
		correlationID string
		expected      string
	}

	pairs := make([]requestPair, numRequests)
	for i := 0; i < numRequests; i++ {
		reqValue := fmt.Sprintf("bulk-req-%03d", i)
		res, err := facade.SendRPC(
			t.Context(), method, wrapperspb.String(reqValue),
			mailboxrpc.RPCOptions{},
		)
		require.NoError(t, err)

		pairs[i] = requestPair{
			correlationID: res.CorrelationID,
			expected:      "bulk-resp-" + reqValue,
		}
	}

	order := rand.New(rand.NewSource(1337)).Perm(numRequests)
	for _, idx := range order {
		respBytes, err := proto.Marshal(
			wrapperspb.String(pairs[idx].expected),
		)
		require.NoError(t, err)

		sendResponseToMailbox(
			t, mb, "client-1", pairs[idx].correlationID, respBytes,
		)
	}

	var (
		wg      sync.WaitGroup
		errs    = make([]error, numRequests)
		outputs = make([]string, numRequests)
	)

	for i := 0; i < numRequests; i++ {
		i := i
		wg.Add(1)

		go func() {
			defer wg.Done()

			awaitCtx, cancel := context.WithTimeout(
				t.Context(), 15*time.Second,
			)
			defer cancel()

			var resp wrapperspb.StringValue
			errs[i] = facade.AwaitRPC(
				awaitCtx, pairs[i].correlationID, &resp,
			)
			outputs[i] = resp.Value
		}()
	}

	wg.Wait()

	for i := 0; i < numRequests; i++ {
		require.NoError(t, errs[i], "await failed for request %d", i)
		require.Equal(t, pairs[i].expected, outputs[i])
	}
}

// TestUnaryFacade_RPCClientInterface verifies the compile-time interface
// compliance check is satisfied.
func TestUnaryFacade_RPCClientInterface(t *testing.T) {
	t.Parallel()

	// This test simply verifies that the compile-time check in
	// unary_facade.go is valid.
	var _ mailboxrpc.RPCClient = (*UnaryFacade)(nil)
}

// TestUnaryFacade_AwaitRPC_NilBody verifies that AwaitRPC returns an error
// when the response envelope has a nil body.
func TestUnaryFacade_AwaitRPC_NilBody(t *testing.T) {
	t.Parallel()

	actor, _, _ := newTestConnector(t, nil)
	facade := NewUnaryFacade(actor)

	corrID := CorrelationID("nil-body-test")

	// Deliver an envelope with nil body directly via the response
	// registry. We schedule the delivery after a short delay so that
	// AwaitRPC has time to register its waiter.
	go func() {
		time.Sleep(50 * time.Millisecond)

		// Use deliverResponse which looks up and signals the
		// waiter channel internally.
		actor.deliverResponse(corrID, &mailboxpb.Envelope{
			Rpc: &mailboxpb.RpcMeta{
				Kind:          mailboxpb.RpcMeta_KIND_RESPONSE,
				CorrelationId: string(corrID),
			},
			// Body is nil.
		})
	}()

	ctx, cancel := context.WithTimeout(
		t.Context(), 5*time.Second,
	)
	defer cancel()

	var resp wrapperspb.StringValue
	err := facade.AwaitRPC(ctx, string(corrID), &resp)
	require.Error(t, err)
	require.Contains(t, err.Error(), "nil body")
}
