package batchsweeper

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/google/uuid"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainsource"
	treepkg "github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo/batchwatcher"
	"github.com/lightninglabs/darepo/internal/testutils"
	"github.com/lightninglabs/darepo/timeout"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// completedFuture returns a Future that is already completed with the given
// response.
func completedFuture[R any](resp R) actor.Future[R] {
	promise := actor.NewPromise[R]()
	promise.Complete(fn.Ok(resp))
	return promise.Future()
}

// mockBatchWatcherRef is a test double for the BatchWatcher actor reference.
// It captures Ask messages and returns a preconfigured response.
type mockBatchWatcherRef struct {
	mu sync.Mutex

	lastAsk batchwatcher.BatchWatcherMsg
	resp    batchwatcher.BatchWatcherResp
}

// ID returns the ID of the mock actor reference.
func (m *mockBatchWatcherRef) ID() string {
	return "mock-batchwatcher"
}

// Tell is a no-op for this mock.
func (m *mockBatchWatcherRef) Tell(_ context.Context,
	_ batchwatcher.BatchWatcherMsg) {
}

// Ask captures the request and returns a completed future with the configured
// response.
func (m *mockBatchWatcherRef) Ask(_ context.Context,
	msg batchwatcher.BatchWatcherMsg) batchWatcherFuture {

	m.mu.Lock()
	defer m.mu.Unlock()

	m.lastAsk = msg

	return completedFuture[batchwatcher.BatchWatcherResp](m.resp)
}

// LastAsk returns the last Ask message captured by the mock.
func (m *mockBatchWatcherRef) LastAsk() batchwatcher.BatchWatcherMsg {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.lastAsk
}

// mockChainSourceRef is a test double for the ChainSource actor reference. It
// returns preconfigured responses for BestHeightRequest and FeeEstimateRequest
// and records broadcasts.
type mockChainSourceRef struct {
	mu sync.Mutex

	bestHeightResp  *chainsource.BestHeightResponse
	feeEstimateResp *chainsource.FeeEstimateResponse

	lastBroadcast *chainsource.BroadcastTxRequest
}

// ID returns the ID of the mock chain source.
func (m *mockChainSourceRef) ID() string {
	return "mock-chainsource"
}

// Tell is a no-op for this mock.
func (m *mockChainSourceRef) Tell(_ context.Context,
	_ chainsource.ChainSourceMsg) {
}

// Ask returns the configured response based on request type.
func (m *mockChainSourceRef) Ask(_ context.Context,
	msg chainsource.ChainSourceMsg) chainSourceFuture {

	m.mu.Lock()
	defer m.mu.Unlock()

	switch req := msg.(type) {
	case *chainsource.BestHeightRequest:
		return completedFuture[chainsource.ChainSourceResp](
			m.bestHeightResp,
		)

	case *chainsource.FeeEstimateRequest:
		return completedFuture[chainsource.ChainSourceResp](
			m.feeEstimateResp,
		)

	case *chainsource.BroadcastTxRequest:
		m.lastBroadcast = req

		return completedFuture[chainsource.ChainSourceResp](
			&chainsource.BroadcastTxResponse{},
		)

	default:
		return completedFuture[chainsource.ChainSourceResp](
			&chainsource.BroadcastTxResponse{},
		)
	}
}

// LastBroadcast returns the last broadcast request captured.
func (m *mockChainSourceRef) LastBroadcast() *chainsource.BroadcastTxRequest {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.lastBroadcast
}

// mockTimeoutRef is a test double for the timeout actor reference that captures
// scheduled timeouts.
type mockTimeoutRef struct {
	mu sync.Mutex

	lastSchedule *timeout.ScheduleTimeoutRequest
}

// ID returns the ID of the mock timeout actor.
func (m *mockTimeoutRef) ID() string {
	return "mock-timeout"
}

// Tell captures schedule requests.
func (m *mockTimeoutRef) Tell(_ context.Context, msg timeout.Msg) {
	m.mu.Lock()
	defer m.mu.Unlock()

	req, ok := msg.(*timeout.ScheduleTimeoutRequest)
	if !ok {
		return
	}

	m.lastSchedule = req
}

// LastSchedule returns the last scheduled timeout request.
func (m *mockTimeoutRef) LastSchedule() *timeout.ScheduleTimeoutRequest {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.lastSchedule
}

// nopSelfRef is a no-op reference used to satisfy the actor's SelfRef field.
type nopSelfRef struct{}

// ID returns the ID of the no-op self reference.
func (n *nopSelfRef) ID() string {
	return "nop-self"
}

// Tell discards all messages.
func (n *nopSelfRef) Tell(_ context.Context, _ Msg) {
}

// TestBatchExpiredQueriesWatcher verifies that an expiry notification causes
// the actor to query the BatchWatcher for tree state.
func TestBatchExpiredQueriesWatcher(t *testing.T) {
	t.Parallel()

	batchID := batchwatcher.BatchID(uuid.New())

	mockWatcher := &mockBatchWatcherRef{
		resp: &batchwatcher.GetTreeStateResponse{
			Found: true,
		},
	}

	cfg := &ActorConfig{
		Logger:       btclog.Disabled,
		BatchWatcher: mockWatcher,
	}

	a := NewActor(cfg)

	result := a.Receive(t.Context(), &BatchExpiredEvent{
		Notification: &batchwatcher.BatchExpiredNotification{
			BatchID:      batchID,
			ExpiryHeight: 123,
		},
	})
	require.True(t, result.IsOk())

	ask := mockWatcher.LastAsk()
	req, ok := ask.(*batchwatcher.GetTreeStateRequest)
	require.True(t, ok)
	require.Equal(t, batchID, req.BatchID)
}

// TestBatchExpiredSchedulesRetryForImmatureOutputs verifies that the actor
// schedules retries when operator-controlled outputs exist but are not yet
// CSV-mature.
func TestBatchExpiredSchedulesRetryForImmatureOutputs(t *testing.T) {
	t.Parallel()

	batchID := batchwatcher.BatchID(uuid.New())

	internalKey, _ := testutils.CreateKey(1)
	node := &treepkg.Node{
		CoSigners: []*btcec.PublicKey{internalKey},
	}

	txOut := wire.NewTxOut(1000, []byte{0x51})

	outpoint := wire.OutPoint{Index: 0}
	treeState := &batchwatcher.BatchTreeState{
		ExistingOutputs: map[wire.OutPoint]*batchwatcher.Output{
			outpoint: {
				Outpoint:        outpoint,
				TxOut:           txOut,
				ConfirmedHeight: 100,
				IsVTXO:          false,
				TreeNode:        node,
				OutputIndex:     0,
			},
		},
	}

	mockWatcher := &mockBatchWatcherRef{
		resp: &batchwatcher.GetTreeStateResponse{
			Found:     true,
			TreeState: treeState,
		},
	}

	mockChainSource := &mockChainSourceRef{
		bestHeightResp: &chainsource.BestHeightResponse{
			Height: 105,
		},
		feeEstimateResp: &chainsource.FeeEstimateResponse{
			SatPerVByte: btcutil.Amount(1),
		},
	}

	mockTimeout := &mockTimeoutRef{}

	cfg := &ActorConfig{
		Logger:            btclog.Disabled,
		BatchWatcher:      mockWatcher,
		ChainSource:       mockChainSource,
		SweepDelay:        10,
		TimeoutActor:      fn.Some[actor.TellOnlyRef[timeout.Msg]](mockTimeout),
		MaxRetryDelay:     time.Hour,
		InitialRetryDelay: time.Second,
		SelfRef:           &nopSelfRef{},
	}

	a := NewActor(cfg)

	result := a.Receive(t.Context(), &BatchExpiredEvent{
		Notification: &batchwatcher.BatchExpiredNotification{
			BatchID:      batchID,
			ExpiryHeight: 123,
		},
	})
	require.True(t, result.IsOk())

	schedule := mockTimeout.LastSchedule()
	require.NotNil(t, schedule)

	require.Equal(t, time.Duration(50)*time.Minute, schedule.Duration)

	expired := a.expired[batchID]
	require.NotNil(t, expired)
	require.EqualValues(t, 0, expired.attempts)
}
