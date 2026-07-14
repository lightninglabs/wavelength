package serverconn

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/baselib/actor"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// failFirstSendEdge wraps the in-memory mailbox edge and fails the first Send
// call after recording the outbound identifiers.
type failFirstSendEdge struct {
	*fakeMailboxServiceClient

	mu sync.Mutex

	sendAttempts int
	firstMsgID   string
	firstIdemKey string
}

// newFailFirstSendEdge creates an edge that fails its first Send call.
func newFailFirstSendEdge(mb *inMemoryMailbox) *failFirstSendEdge {
	return &failFirstSendEdge{
		fakeMailboxServiceClient: &fakeMailboxServiceClient{
			mb: mb,
		},
	}
}

// Send records attempt metadata and fails once before succeeding thereafter.
func (e *failFirstSendEdge) Send(ctx context.Context, in *mailboxpb.SendRequest,
	opts ...grpc.CallOption) (*mailboxpb.SendResponse, error) {

	e.mu.Lock()
	defer e.mu.Unlock()

	if in == nil || in.Envelope == nil {
		return nil, fmt.Errorf("nil send request")
	}

	e.sendAttempts++
	if e.sendAttempts == 1 {
		e.firstMsgID = in.Envelope.MsgId
		e.firstIdemKey = in.Envelope.IdempotencyKey

		return nil, fmt.Errorf("injected first send failure")
	}

	return e.fakeMailboxServiceClient.Send(ctx, in, opts...)
}

// Snapshot returns the current edge attempt counters and first-attempt IDs.
func (e *failFirstSendEdge) Snapshot() (int, string, string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	return e.sendAttempts, e.firstMsgID, e.firstIdemKey
}

// newDurableConnectorForTest creates a DurableActor wrapper around
// ServerConnectionActor with an explicit Tell retry delay.
func newDurableConnectorForTest(
	t *testing.T,
	cfg ConnectorConfig,
	retryDelay time.Duration,
) *actor.DurableActor[ServerConnMsg, ServerConnResp] {

	connector := NewServerConnectionActor(cfg)

	durableCfg := actor.DefaultDurableTxActorConfig[
		ServerConnMsg, ServerConnResp, egressTx,
	](
		DurableActorID(cfg.LocalMailboxID), connector,
		connector.bindStores, cfg.Store, cfg.Codec,
	)

	durableCfg.PollInterval = 10 * time.Millisecond
	durableCfg.LeaseDuration = 500 * time.Millisecond
	durableCfg.HeartbeatInterval = 100 * time.Millisecond
	durableCfg.TellRetryPolicy = func(err error, attempts int) (bool,
		time.Duration) {

		if attempts >= 5 {
			return false, 0
		}

		return true, retryDelay
	}

	return actor.NewDurableActor(durableCfg).UnwrapOrFail(t)
}

// TestEgress_RestartReplayPreservesStableIDs verifies that a failed egress send
// replayed after actor restart reuses the same MsgId and IdempotencyKey.
func TestEgress_RestartReplayPreservesStableIDs(t *testing.T) {
	t.Parallel()

	mb := newInMemoryMailbox()
	edge := newFailFirstSendEdge(mb)
	store := newMemCheckpointStore()

	cfg := newTestConnectorConfig(mb, store)
	cfg.Edge = edge
	cfg.Codec = NewServerConnCodec()

	// First actor instance fails once and nacks the message for retry.
	durable1 := newDurableConnectorForTest(t, cfg, 500*time.Millisecond)
	durable1.Start()

	err := durable1.TellRef().Tell(t.Context(), &SendClientEventRequest{
		Message: &testServerMessage{value: "restart-event"},
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		attempts, _, _ := edge.Snapshot()

		return attempts >= 1
	}, 5*time.Second, 10*time.Millisecond)

	durable1.Stop()

	attempts, firstMsgID, firstIdemKey := edge.Snapshot()
	require.Equal(t, 1, attempts)
	require.NotEmpty(t, firstMsgID)
	require.NotEmpty(t, firstIdemKey)

	mb.mu.Lock()
	firstRunEnvs := append(
		[]*mailboxpb.Envelope(nil), mb.mailboxes["server-1"]...,
	)
	mb.mu.Unlock()
	require.Empty(t, firstRunEnvs)

	// Second actor instance replays from the same durable store and then
	// succeeds.
	durable2 := newDurableConnectorForTest(t, cfg, 20*time.Millisecond)
	durable2.Start()
	defer durable2.Stop()

	require.Eventually(t, func() bool {
		mb.mu.Lock()
		defer mb.mu.Unlock()

		return len(mb.mailboxes["server-1"]) == 1
	}, 8*time.Second, 10*time.Millisecond)

	mb.mu.Lock()
	replayed := mb.mailboxes["server-1"][0]
	mb.mu.Unlock()

	require.Equal(t, firstMsgID, replayed.MsgId)
	require.Equal(t, firstIdemKey, replayed.IdempotencyKey)
}

// TestDurableUnary_RestartReplayPreservesStableIDs verifies a durable unary
// query replayed after actor restart reuses the same MsgId and
// IdempotencyKey while preserving route/correlation metadata.
func TestDurableUnary_RestartReplayPreservesStableIDs(t *testing.T) {
	t.Parallel()

	mb := newInMemoryMailbox()
	edge := newFailFirstSendEdge(mb)
	store := newMemCheckpointStore()

	cfg := newTestConnectorConfig(mb, store)
	cfg.Edge = edge
	cfg.Codec = NewServerConnCodec()
	cfg.DurableUnaryBuilder = &testDurableUnaryBuilder{}

	// First actor instance fails once and leaves the durable unary
	// send queued for replay.
	durable1 := newDurableConnectorForTest(t, cfg, 500*time.Millisecond)
	durable1.Start()

	err := durable1.TellRef().Tell(
		t.Context(), &SendListVTXOsByScriptsRequest{
			PkScripts: [][]byte{
				{0x51, 0x20, 0x01},
			},
			AfterCursor:   []byte("cursor-11"),
			Limit:         5,
			CorrelationID: "corr-vtxo-restart",
		},
	)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		attempts, _, _ := edge.Snapshot()

		return attempts >= 1
	}, 5*time.Second, 10*time.Millisecond)

	durable1.Stop()

	attempts, firstMsgID, firstIdemKey := edge.Snapshot()
	require.Equal(t, 1, attempts)
	require.NotEmpty(t, firstMsgID)
	require.NotEmpty(t, firstIdemKey)

	mb.mu.Lock()
	firstRunEnvs := append(
		[]*mailboxpb.Envelope(nil), mb.mailboxes["server-1"]...,
	)
	mb.mu.Unlock()
	require.Empty(t, firstRunEnvs)

	// Second actor instance replays from the same durable store and then
	// succeeds.
	durable2 := newDurableConnectorForTest(t, cfg, 20*time.Millisecond)
	durable2.Start()
	defer durable2.Stop()

	require.Eventually(t, func() bool {
		mb.mu.Lock()
		defer mb.mu.Unlock()

		return len(mb.mailboxes["server-1"]) == 1
	}, 8*time.Second, 10*time.Millisecond)

	mb.mu.Lock()
	replayed := mb.mailboxes["server-1"][0]
	mb.mu.Unlock()

	require.Equal(t, firstMsgID, replayed.MsgId)
	require.Equal(t, firstIdemKey, replayed.IdempotencyKey)
	require.Equal(
		t, "arkrpc.IndexerService", replayed.GetRpc().GetService(),
	)
	require.Equal(t, "ListVTXOsByScripts", replayed.GetRpc().GetMethod())
	require.Equal(
		t, "corr-vtxo-restart", replayed.GetRpc().GetCorrelationId(),
	)

	payload := &wrapperspb.StringValue{}
	require.NoError(t, replayed.GetBody().UnmarshalTo(payload))
	require.Equal(t, "vtxos:1:637572736f722d3131:5", payload.GetValue())
}
