package serverconn

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/baselib/actor"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// rpcSendAttempt records the stable envelope identity observed by one edge
// call. Durable retries must preserve both fields.
type rpcSendAttempt struct {
	msgID          string
	idempotencyKey string
}

// blackHoleRPCSendEdge blocks the first attempt for selected message IDs until
// the send context ends. Later attempts succeed so tests can prove that a
// timed-out durable message remains retryable.
type blackHoleRPCSendEdge struct {
	mailboxpb.MailboxServiceClient

	mu sync.Mutex

	blockedIDs map[string]struct{}
	attempts   map[string][]rpcSendAttempt
	succeeded  map[string]bool
	started    chan string
}

// newBlackHoleRPCSendEdge constructs an edge that black-holes the first send
// for each supplied message ID.
func newBlackHoleRPCSendEdge(blockedIDs ...string) *blackHoleRPCSendEdge {
	blocked := make(map[string]struct{}, len(blockedIDs))
	for _, id := range blockedIDs {
		blocked[id] = struct{}{}
	}

	return &blackHoleRPCSendEdge{
		blockedIDs: blocked,
		attempts:   make(map[string][]rpcSendAttempt),
		succeeded:  make(map[string]bool),
		started:    make(chan string, len(blockedIDs)),
	}
}

// Send records the envelope identity, waits for ctx.Done on the first selected
// attempt, and accepts every other attempt.
func (e *blackHoleRPCSendEdge) Send(ctx context.Context,
	in *mailboxpb.SendRequest, _ ...grpc.CallOption) (
	*mailboxpb.SendResponse, error) {

	envelope := in.GetEnvelope()
	attempt := rpcSendAttempt{
		msgID:          envelope.GetMsgId(),
		idempotencyKey: envelope.GetIdempotencyKey(),
	}

	e.mu.Lock()
	e.attempts[attempt.msgID] = append(
		e.attempts[attempt.msgID], attempt,
	)
	attemptNumber := len(e.attempts[attempt.msgID])
	_, shouldBlock := e.blockedIDs[attempt.msgID]
	e.mu.Unlock()

	if shouldBlock && attemptNumber == 1 {
		e.started <- attempt.msgID
		<-ctx.Done()

		return nil, ctx.Err()
	}

	e.mu.Lock()
	e.succeeded[attempt.msgID] = true
	e.mu.Unlock()

	return &mailboxpb.SendResponse{
		Status: &mailboxpb.Status{
			Ok: true,
		},
	}, nil
}

// snapshot returns copies of the observed attempts and successful message IDs.
func (e *blackHoleRPCSendEdge) snapshot() (map[string][]rpcSendAttempt,
	map[string]bool) {

	e.mu.Lock()
	defer e.mu.Unlock()

	attempts := make(map[string][]rpcSendAttempt, len(e.attempts))
	for id, values := range e.attempts {
		attempts[id] = append([]rpcSendAttempt(nil), values...)
	}

	succeeded := make(map[string]bool, len(e.succeeded))
	for id, value := range e.succeeded {
		succeeded[id] = value
	}

	return attempts, succeeded
}

// countingLeaseStore records lease extensions while preserving the in-memory
// delivery store's normal fencing behavior.
type countingLeaseStore struct {
	*memCheckpointStore

	extensions atomic.Int64
}

// ExtendLease counts successful heartbeats from the in-memory store.
func (s *countingLeaseStore) ExtendLease(ctx context.Context, id,
	leaseToken string, extension time.Duration) (int64, error) {

	rows, err := s.memCheckpointStore.ExtendLease(
		ctx, id, leaseToken, extension,
	)
	if err == nil && rows > 0 {
		s.extensions.Add(1)
	}

	return rows, err
}

// TestSendRPCRequestOwnsBoundedSendContext proves a pre-built durable send
// ignores cancellation of its actor turn but still ends at its own deadline.
// The black-holed edge waits on ctx.Done, so the test also pins that the
// timeout reaches the transport call and that a timed-out send is not
// committed.
func TestSendRPCRequestOwnsBoundedSendContext(t *testing.T) {
	const sendTimeout = 50 * time.Millisecond

	var (
		hasDeadline        bool
		deadline           time.Time
		deadlineObservedAt time.Time
	)
	edge := &mailboxClientStub{
		sendFn: func(ctx context.Context, _ *mailboxpb.SendRequest,
			_ ...grpc.CallOption) (*mailboxpb.SendResponse, error) {

			deadlineObservedAt = time.Now()
			deadline, hasDeadline = ctx.Deadline()
			<-ctx.Done()

			return nil, ctx.Err()
		},
	}

	connector := newErrorPathActor(edge, newMemCheckpointStore())
	require.Equal(t, defaultSendEventTimeout, connector.sendTimeout)
	connector.sendTimeout = sendTimeout

	turnCtx, cancelTurn := context.WithCancel(t.Context())
	cancelTurn()

	started := time.Now()
	ax := &fakeEgressExec{}
	result := connector.Receive(turnCtx, &SendRPCRequest{
		Envelope: &mailboxpb.Envelope{
			MsgId:          "bounded-send",
			IdempotencyKey: "bounded-send-idempotency",
		},
	}, ax)
	elapsed := time.Since(started)

	require.ErrorIs(t, result.Err(), context.DeadlineExceeded)
	require.True(t, hasDeadline)
	require.WithinDuration(
		t, deadlineObservedAt.Add(sendTimeout), deadline,
		20*time.Millisecond,
	)
	require.GreaterOrEqual(t, elapsed, sendTimeout/2)
	require.Zero(t, ax.commits)
}

// TestSendRPCRequestDeadlineReleasesWorkerPool fills the four-worker durable
// egress pool with black-holed pre-built RPC sends. Lease heartbeats stay
// active while each delivery is blocked. The timeout then nacks the messages,
// frees workers for a fifth request, and retries every failed envelope with
// the same message and idempotency identifiers.
func TestSendRPCRequestDeadlineReleasesWorkerPool(t *testing.T) {
	const (
		numWorkers  = 4
		sendTimeout = 100 * time.Millisecond
	)

	blockedIDs := []string{
		"blocked-1",
		"blocked-2",
		"blocked-3",
		"blocked-4",
	}
	edge := newBlackHoleRPCSendEdge(blockedIDs...)
	store := &countingLeaseStore{
		memCheckpointStore: newMemCheckpointStore(),
	}

	cfg := newTestConnectorConfig(
		newInMemoryMailbox(), store.memCheckpointStore,
	)
	cfg.Edge = edge
	cfg.Store = store
	cfg.Codec = NewServerConnCodec()

	connector := NewServerConnectionActor(cfg)
	connector.sendTimeout = sendTimeout

	durableCfg := actor.DefaultDurableTxActorConfig[
		ServerConnMsg, ServerConnResp, egressTx,
	](
		DurableActorID(cfg.LocalMailboxID), connector,
		connector.bindStores, store, cfg.Codec,
	)
	durableCfg.NumWorkers = numWorkers
	durableCfg.PollInterval = 5 * time.Millisecond
	// Keep the lease comfortably beyond the injected send timeout. The test
	// observes successful heartbeat extensions without making exact retry
	// counts depend on sub-100ms scheduler timing under the race detector.
	durableCfg.LeaseDuration = time.Second
	durableCfg.HeartbeatInterval = 10 * time.Millisecond
	durableCfg.TellRetryPolicy = func(error, int) (bool, time.Duration) {
		return true, 20 * time.Millisecond
	}

	durable := actor.NewDurableActor(durableCfg).UnwrapOrFail(t)
	durable.Start()
	defer durable.Stop()

	for _, id := range blockedIDs {
		err := durable.TellRef().Tell(t.Context(), &SendRPCRequest{
			Envelope: &mailboxpb.Envelope{
				MsgId:          id,
				IdempotencyKey: id + "-idempotency",
			},
		})
		require.NoError(t, err)
	}

	started := make(map[string]bool, numWorkers)
	for len(started) < numWorkers {
		select {
		case id := <-edge.started:
			started[id] = true

		case <-time.After(time.Second):
			t.Fatal(
				"four black-holed sends did not occupy the " +
					"worker pool",
			)
		}
	}

	const probeID = "probe"
	err := durable.TellRef().Tell(t.Context(), &SendRPCRequest{
		Envelope: &mailboxpb.Envelope{
			MsgId:          probeID,
			IdempotencyKey: probeID + "-idempotency",
		},
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		_, succeeded := edge.snapshot()

		return len(succeeded) == numWorkers+1
	}, 3*time.Second, 10*time.Millisecond)

	attempts, succeeded := edge.snapshot()
	require.True(t, succeeded[probeID])
	require.Len(t, attempts[probeID], 1)

	for _, id := range blockedIDs {
		require.True(t, succeeded[id])
		require.Len(t, attempts[id], 2)
		require.Equal(t, attempts[id][0], attempts[id][1])
		require.Equal(t, id, attempts[id][0].msgID)
		require.Equal(
			t, id+"-idempotency", attempts[id][0].idempotencyKey,
		)
	}

	require.Positive(t, store.extensions.Load())
}
