package serverconn

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mailboxconn "github.com/lightninglabs/wavelength/mailbox/conn"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// recordingEdge is a MailboxServiceClient that counts Send calls and returns a
// configurable status, used to prove permanent-error handling without a real
// transport.
type recordingEdge struct {
	sendCount  atomic.Int64
	sendStatus *mailboxpb.Status
}

func (e *recordingEdge) Send(_ context.Context, _ *mailboxpb.SendRequest,
	_ ...grpc.CallOption) (*mailboxpb.SendResponse, error) {

	e.sendCount.Add(1)

	return &mailboxpb.SendResponse{Status: e.sendStatus}, nil
}

func (e *recordingEdge) Pull(_ context.Context, _ *mailboxpb.PullRequest,
	_ ...grpc.CallOption) (*mailboxpb.PullResponse, error) {

	return &mailboxpb.PullResponse{Status: okStatus()}, nil
}

func (e *recordingEdge) AckUpTo(_ context.Context, _ *mailboxpb.AckUpToRequest,
	_ ...grpc.CallOption) (*mailboxpb.AckUpToResponse, error) {

	return &mailboxpb.AckUpToResponse{Status: okStatus()}, nil
}

// permanentStatus builds a permanent version status for tests.
func permanentStatus() *mailboxpb.Status {
	return &mailboxpb.Status{
		Ok:      false,
		Code:    mailboxconn.StatusUpgradeRequired,
		Message: "client must upgrade",
		SupportedArkVersions: []uint32{
			2,
		},
	}
}

// TestMarkIncompatibleOnce proves that concurrent permanent failures cause
// exactly one state transition and one callback, cache the typed error, and
// fail pending unary waiters with that error.
func TestMarkIncompatibleOnce(t *testing.T) {
	t.Parallel()

	var callbackCount atomic.Int64

	mb := newInMemoryMailbox()
	cfg := newTestConnectorConfig(mb, newMemCheckpointStore())
	cfg.OnIncompatible = func(*mailboxconn.StatusError) {
		callbackCount.Add(1)
	}

	conn := NewServerConnectionActor(cfg)

	// Register a unary waiter that must be failed by the transition.
	corrID := CorrelationID("corr-1")
	fut := conn.RegisterWaiter(corrID)

	statusErr := mailboxconn.NewStatusError("Send", permanentStatus())

	// Fire many concurrent permanent failures.
	const racers = 16
	var wg sync.WaitGroup
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func() {
			defer wg.Done()

			require.True(
				t,
				conn.checkPermanentStatus(
					t.Context(), statusErr,
				),
			)
		}()
	}
	wg.Wait()

	// Exactly one transition and one callback.
	require.Equal(t, int64(1), callbackCount.Load())
	require.NotNil(t, conn.compatibilityError())
	require.Equal(
		t, mailboxconn.StatusUpgradeRequired,
		conn.compatibilityError().Code(),
	)

	// The pending waiter received the same typed error.
	res := fut.Await(t.Context())
	require.Error(t, res.Err())
	require.True(t, mailboxconn.IsPermanentVersionError(res.Err()))
}

// TestTransientStatusDoesNotMarkIncompatible proves a transient status does not
// transition the connector, so the existing retry policy still applies.
func TestTransientStatusDoesNotMarkIncompatible(t *testing.T) {
	t.Parallel()

	mb := newInMemoryMailbox()
	cfg := newTestConnectorConfig(mb, newMemCheckpointStore())
	conn := NewServerConnectionActor(cfg)

	transient := mailboxconn.NewStatusError("Pull", &mailboxpb.Status{
		Ok:   false,
		Code: "UNAVAILABLE",
	})
	require.False(t, conn.checkPermanentStatus(t.Context(), transient))
	require.Nil(t, conn.compatibilityError())
}

// TestSendShortCircuitsAfterIncompatible proves future sends return the cached
// error without contacting the edge once the connector is incompatible.
func TestSendShortCircuitsAfterIncompatible(t *testing.T) {
	t.Parallel()

	edge := &recordingEdge{sendStatus: okStatus()}
	mb := newInMemoryMailbox()
	cfg := newTestConnectorConfig(mb, newMemCheckpointStore())
	cfg.Edge = edge

	conn := NewServerConnectionActor(cfg)

	// Transition to incompatible directly.
	conn.markIncompatible(
		t.Context(),
		mailboxconn.NewStatusError(
			"Send", permanentStatus(),
		),
	)

	// An event send must not contact the edge and must return the cached
	// error.
	res := conn.Receive(t.Context(), &SendClientEventRequest{
		Message: &testServerMessage{value: "after-incompat"},
	}, &fakeEgressExec{})
	require.Error(t, res.Err())
	require.True(t, mailboxconn.IsPermanentVersionError(res.Err()))

	// The unary facade and heartbeat paths must also avoid the edge.
	conn.sendHeartbeat(t.Context())

	require.Equal(t, int64(0), edge.sendCount.Load())
}

// TestPermanentStatusFromEdgeMarksIncompatible proves that a permanent status
// returned by the edge transitions the connector and invokes the callback
// once, while the edge is contacted exactly once.
func TestPermanentStatusFromEdgeMarksIncompatible(t *testing.T) {
	t.Parallel()

	var callbackCount atomic.Int64

	edge := &recordingEdge{sendStatus: permanentStatus()}
	mb := newInMemoryMailbox()
	cfg := newTestConnectorConfig(mb, newMemCheckpointStore())
	cfg.Edge = edge
	cfg.OnIncompatible = func(*mailboxconn.StatusError) {
		callbackCount.Add(1)
	}

	conn := NewServerConnectionActor(cfg)

	res := conn.Receive(t.Context(), &SendClientEventRequest{
		Message: &testServerMessage{value: "permanent"},
	}, &fakeEgressExec{})
	require.Error(t, res.Err())
	require.True(t, mailboxconn.IsPermanentVersionError(res.Err()))

	require.Equal(t, int64(1), edge.sendCount.Load())
	require.Equal(t, int64(1), callbackCount.Load())
	require.NotNil(t, conn.compatibilityError())
}

// TestStartIngressRefusesWhenIncompatible proves that if the connector became
// incompatible before ingress starts (e.g. a durable egress replay hit a
// permanent error between StartEgress and StartIngress), StartIngress returns
// the cached error and never starts polling — so the caller cannot mark the
// connection healthy.
func TestStartIngressRefusesWhenIncompatible(t *testing.T) {
	t.Parallel()

	mb := newInMemoryMailbox()
	cfg := newTestConnectorConfig(mb, newMemCheckpointStore())
	conn := NewServerConnectionActor(cfg)

	conn.markIncompatible(
		t.Context(),
		mailboxconn.NewStatusError(
			"Send", permanentStatus(),
		),
	)

	err := conn.StartIngress(t.Context())
	require.Error(t, err)
	require.True(t, mailboxconn.IsPermanentVersionError(err))
}

// TestAwaitRPCShortCircuitsAfterIncompatible proves AwaitRPC returns the cached
// error instead of blocking when incompatibility lands after a waiter was
// pre-registered (by SendRPC) but before AwaitRPC runs.
func TestAwaitRPCShortCircuitsAfterIncompatible(t *testing.T) {
	t.Parallel()

	mb := newInMemoryMailbox()
	cfg := newTestConnectorConfig(mb, newMemCheckpointStore())
	conn := NewServerConnectionActor(cfg)
	facade := NewUnaryFacade(conn)

	// Simulate SendRPC pre-registering the waiter, then incompatibility
	// landing (which drains waiters via FailAll).
	conn.RegisterWaiter(CorrelationID("corr-1"))
	conn.markIncompatible(
		t.Context(),
		mailboxconn.NewStatusError(
			"Send", permanentStatus(),
		),
	)

	// A bounded context ensures a regression (blocking) fails fast instead
	// of hanging the suite.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	err := facade.AwaitRPC(ctx, "corr-1", &mailboxpb.Status{})
	require.Error(t, err)
	require.True(t, mailboxconn.IsPermanentVersionError(err))
}

// TestRuntimeMarkIncompatible proves the exported Runtime.MarkIncompatible
// entry point drives the connector to its terminal state and fires the
// callback, so side-channel detectors (such as a refresh-only GetInfo) can
// transition the runtime.
func TestRuntimeMarkIncompatible(t *testing.T) {
	t.Parallel()

	var callbackCount atomic.Int64

	mb := newInMemoryMailbox()
	cfg := newTestConnectorConfig(mb, newMemCheckpointStore())
	cfg.OnIncompatible = func(*mailboxconn.StatusError) {
		callbackCount.Add(1)
	}

	rt, err := NewRuntime(cfg)
	require.NoError(t, err)

	rt.MarkIncompatible(
		t.Context(),
		mailboxconn.NewStatusError(
			"refresh", permanentStatus(),
		),
	)

	require.Equal(t, int64(1), callbackCount.Load())
	require.NotNil(t, rt.Connector().compatibilityError())
}

// TestPermanentAwareTellRetryPolicy proves the durable retry policy treats a
// permanent version error as non-retryable (dead-letter) while a transient
// error keeps the default retry behavior.
func TestPermanentAwareTellRetryPolicy(t *testing.T) {
	t.Parallel()

	permErr := mailboxconn.NewStatusError("Send", permanentStatus())
	retry, _ := permanentAwareTellRetryPolicy(permErr, 0)
	require.False(t, retry, "permanent error must not be retried")

	transient := mailboxconn.NewStatusError("Send", &mailboxpb.Status{
		Ok:   false,
		Code: "UNAVAILABLE",
	})
	retry, _ = permanentAwareTellRetryPolicy(transient, 0)
	require.True(t, retry, "transient error must still retry")
}

// TestDurableSendDeadLettersPermanentError proves a durable event whose send
// returns a permanent version error is dead-lettered immediately rather than
// retried.
func TestDurableSendDeadLettersPermanentError(t *testing.T) {
	t.Parallel()

	edge := &recordingEdge{sendStatus: permanentStatus()}
	mb := newInMemoryMailbox()
	store := newMemCheckpointStore()
	cfg := newTestConnectorConfig(mb, store)
	cfg.Edge = edge

	rt, err := NewRuntime(cfg)
	require.NoError(t, err)

	rt.StartEgress()
	defer rt.Stop()

	err = rt.TellRef().Tell(t.Context(), &SendClientEventRequest{
		Message: &testServerMessage{value: "dead-letter"},
	})
	require.NoError(t, err)

	actorID := DurableActorID(cfg.LocalMailboxID)
	require.Eventually(t, func() bool {
		letters, lErr := store.ListDeadLetters(t.Context(), actorID, 10)
		require.NoError(t, lErr)

		return len(letters) == 1
	}, 5*time.Second, 10*time.Millisecond)

	// The edge was contacted at most once: there was no retry storm.
	require.LessOrEqual(t, edge.sendCount.Load(), int64(1))
}
