package serverconn

import (
	"fmt"
	"testing"

	mailboxrpc "github.com/lightninglabs/wavelength/mailbox/rpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// testUnaryMethod is the service method the in-flight and idempotency tests
// send against.
var testUnaryMethod = mailboxrpc.ServiceMethod{
	Service: "test.Svc",
	Method:  "GetInfo",
}

// newBoundedTestFacade builds a connector whose unary in-flight cap is limit,
// along with the facade in front of it and the mailbox behind it.
func newBoundedTestFacade(t *testing.T,
	limit int) (*UnaryFacade, *inMemoryMailbox) {

	t.Helper()

	mb := newInMemoryMailbox()
	cfg := newTestConnectorConfig(mb, newMemCheckpointStore())
	cfg.MaxInFlightUnary = limit

	return NewUnaryFacade(NewServerConnectionActor(cfg)), mb
}

// TestSendRPCReusesCallerIdempotencyKey pins that the transport forwards a
// caller-owned key verbatim on every attempt while still rotating the message
// ID. The stable key is what lets the operator recognize a retry, and the
// fresh message ID is what keeps the two sends distinct on the wire.
func TestSendRPCReusesCallerIdempotencyKey(t *testing.T) {
	t.Parallel()

	facade, mb := newBoundedTestFacade(t, DefaultMaxInFlightUnary)

	const key = "idem-one-logical-request"
	opts := mailboxrpc.RPCOptions{IdempotencyKey: key}

	for i := 0; i < 3; i++ {
		result, err := facade.SendRPC(
			t.Context(), testUnaryMethod,
			wrapperspb.String("request"), opts,
		)
		require.NoError(t, err)
		require.Equal(t, key, result.IdempotencyKey)

		// A retry reuses the correlation ID too, so an answer to the
		// abandoned attempt lands on the retry's waiter.
		require.Equal(t, key, result.CorrelationID)
	}

	mb.mu.Lock()
	envs := mb.mailboxes["server-1"]
	mb.mu.Unlock()

	require.Len(t, envs, 3)

	msgIDs := make(map[string]struct{}, len(envs))
	for _, env := range envs {
		require.Equal(t, key, env.IdempotencyKey)
		require.Equal(t, key, env.Rpc.CorrelationId)

		require.NotEmpty(t, env.MsgId)
		_, dup := msgIDs[env.MsgId]
		require.False(t, dup, "msg id %s reused", env.MsgId)

		msgIDs[env.MsgId] = struct{}{}
	}
}

// TestSendRPCDistinctRequestsDoNotCollide pins that two logical requests with
// identical payloads still get distinct keys. Collapsing them would let the
// operator answer the second read from the first read's cached result.
func TestSendRPCDistinctRequestsDoNotCollide(t *testing.T) {
	t.Parallel()

	facade, _ := newBoundedTestFacade(t, DefaultMaxInFlightUnary)

	seen := make(map[string]struct{})
	for i := 0; i < 32; i++ {
		result, err := facade.SendRPC(
			t.Context(), testUnaryMethod,
			wrapperspb.String("identical request"),
			mailboxrpc.RPCOptions{},
		)
		require.NoError(t, err)
		require.NotEmpty(t, result.IdempotencyKey)

		_, dup := seen[result.IdempotencyKey]
		require.False(
			t, dup, "idempotency key %s reused across distinct "+
				"logical requests", result.IdempotencyKey,
		)

		seen[result.IdempotencyKey] = struct{}{}
	}
}

// TestSendRPCBoundsInFlightRequests pins the client-side admission bound. With
// no cancel envelope in the protocol, an abandoned request keeps running
// remotely, so a client that never caps its outstanding set converts a remote
// that stopped answering into unbounded queued work.
func TestSendRPCBoundsInFlightRequests(t *testing.T) {
	t.Parallel()

	const limit = 3

	facade, mb := newBoundedTestFacade(t, limit)

	// Nothing drains the responses here, so every send leaves its waiter
	// registered, exactly like a caller still blocked on its deadline.
	for i := 0; i < limit; i++ {
		_, err := facade.SendRPC(
			t.Context(), testUnaryMethod,
			wrapperspb.String("request"), mailboxrpc.RPCOptions{},
		)
		require.NoError(t, err)
	}

	_, err := facade.SendRPC(
		t.Context(), testUnaryMethod, wrapperspb.String("request"),
		mailboxrpc.RPCOptions{},
	)
	require.Error(t, err)

	// The refusal must be indistinguishable from an operator shed so
	// callers back off on it without knowing where it came from.
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
	require.True(t, mailboxrpc.IsShedError(err))

	// The refused request must not reach the wire at all; sending it and
	// then failing locally would defeat the point of the bound.
	mb.mu.Lock()
	envs := mb.mailboxes["server-1"]
	mb.mu.Unlock()

	require.Len(t, envs, limit)
}

// TestSendRPCAdmitsAfterWaiterCompletes pins that the bound is a live measure
// of outstanding work rather than a lifetime quota: once a caller collects its
// answer, the slot is free again.
func TestSendRPCAdmitsAfterWaiterCompletes(t *testing.T) {
	t.Parallel()

	const limit = 1

	facade, _ := newBoundedTestFacade(t, limit)

	result, err := facade.SendRPC(
		t.Context(), testUnaryMethod, wrapperspb.String("request"),
		mailboxrpc.RPCOptions{},
	)
	require.NoError(t, err)

	_, err = facade.SendRPC(
		t.Context(), testUnaryMethod, wrapperspb.String("request"),
		mailboxrpc.RPCOptions{},
	)
	require.Equal(t, codes.ResourceExhausted, status.Code(err))

	// Retiring the first caller's waiter is what an AwaitRPC return (or a
	// deadline giving up) does.
	facade.connector.removeWaiter(CorrelationID(result.CorrelationID))

	_, err = facade.SendRPC(
		t.Context(), testUnaryMethod, wrapperspb.String("request"),
		mailboxrpc.RPCOptions{},
	)
	require.NoError(t, err)
}

// TestSendRPCDefaultsInFlightLimit pins that a hand-rolled config with no cap
// still gets one. An unset knob must not mean unbounded.
func TestSendRPCDefaultsInFlightLimit(t *testing.T) {
	t.Parallel()

	facade, _ := newBoundedTestFacade(t, 0)

	require.NoError(t, facade.connector.admitUnary())

	// Fill the registry past the default cap without going through the
	// wire, then confirm admission closes.
	for i := 0; i < DefaultMaxInFlightUnary; i++ {
		facade.connector.RegisterWaiter(
			CorrelationID(
				fmt.Sprintf("corr-%d", i),
			),
		)
	}

	require.Equal(
		t, codes.ResourceExhausted,
		status.Code(
			facade.connector.admitUnary(),
		),
	)
}
