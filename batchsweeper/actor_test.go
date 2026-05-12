package batchsweeper

import (
	"context"
	"errors"
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
	"github.com/lightninglabs/darepo-client/timeout"
	"github.com/lightninglabs/darepo/batchwatcher"
	"github.com/lightninglabs/darepo/internal/testutils"
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
	_ batchwatcher.BatchWatcherMsg) error {

	return nil
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

	lastBroadcast        *chainsource.BroadcastTxRequest
	lastConfRegistration *chainsource.RegisterConfRequest
}

// ID returns the ID of the mock chain source.
func (m *mockChainSourceRef) ID() string {
	return "mock-chainsource"
}

// Tell captures fire-and-forget messages like RegisterConfRequest.
func (m *mockChainSourceRef) Tell(_ context.Context,
	msg chainsource.ChainSourceMsg) error {

	m.mu.Lock()
	defer m.mu.Unlock()

	if req, ok := msg.(*chainsource.RegisterConfRequest); ok {
		m.lastConfRegistration = req
	}

	return nil
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

// LastConfRegistration returns the last confirmation registration request.
func (m *mockChainSourceRef) LastConfRegistration() *chainsource.RegisterConfRequest { //nolint:ll
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.lastConfRegistration
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
func (m *mockTimeoutRef) Tell(_ context.Context, msg timeout.Msg) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	req, ok := msg.(*timeout.ScheduleTimeoutRequest)
	if !ok {
		return nil
	}

	m.lastSchedule = req

	return nil
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
func (n *nopSelfRef) Tell(_ context.Context, _ Msg) error {
	return nil
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

	mockChainSource := &mockChainSourceRef{}

	cfg := &ActorConfig{
		Log:          fn.Some(btclog.Disabled),
		BatchWatcher: mockWatcher,
		ChainSource:  mockChainSource,
		SelfRef:      &nopSelfRef{},
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
		CoSigners: []*btcec.PublicKey{
			internalKey,
		},
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
		Log:          fn.Some(btclog.Disabled),
		BatchWatcher: mockWatcher,
		ChainSource:  mockChainSource,
		SweepDelay:   10,
		TimeoutActor: fn.Some[actor.TellOnlyRef[timeout.Msg]](
			mockTimeout,
		),
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

// TestRepeatedBatchExpiredPreservesAttempts verifies that receiving repeated
// BatchExpiredEvent (per-block retry from BatchWatcher) does not reset the
// attempt counter for an already-expired batch.
func TestRepeatedBatchExpiredPreservesAttempts(t *testing.T) {
	t.Parallel()

	batchID := batchwatcher.BatchID(uuid.New())

	mockWatcher := &mockBatchWatcherRef{
		resp: &batchwatcher.GetTreeStateResponse{
			Found: false,
		},
	}

	mockChainSource := &mockChainSourceRef{}

	cfg := &ActorConfig{
		Log:          fn.Some(btclog.Disabled),
		BatchWatcher: mockWatcher,
		ChainSource:  mockChainSource,
		SelfRef:      &nopSelfRef{},
	}

	a := NewActor(cfg)

	// First expiry notification.
	result := a.Receive(t.Context(), &BatchExpiredEvent{
		Notification: &batchwatcher.BatchExpiredNotification{
			BatchID:      batchID,
			ExpiryHeight: 100,
		},
	})
	require.True(t, result.IsOk())
	require.EqualValues(t, 0, a.expired[batchID].attempts)

	// Simulate some failures to increment attempts.
	a.expired[batchID].attempts = 5

	// Repeated expiry notification (per-block retry from BatchWatcher).
	result = a.Receive(t.Context(), &BatchExpiredEvent{
		Notification: &batchwatcher.BatchExpiredNotification{
			BatchID:      batchID,
			ExpiryHeight: 100,
		},
	})
	require.True(t, result.IsOk())

	// Verify attempts were NOT reset.
	require.EqualValues(t, 5, a.expired[batchID].attempts)
}

// TestSweepConfirmedCleansUp verifies that a sweep confirmation event cleans
// up the tracking state for the batch. The watcher self-unregisters, so the
// sweeper only needs to clean up its own maps.
func TestSweepConfirmedCleansUp(t *testing.T) {
	t.Parallel()

	batchID := batchwatcher.BatchID(uuid.New())

	mockWatcher := &mockBatchWatcherRef{}
	mockChainSource := &mockChainSourceRef{}

	cfg := &ActorConfig{
		Log:          fn.Some(btclog.Disabled),
		BatchWatcher: mockWatcher,
		ChainSource:  mockChainSource,
		SelfRef:      &nopSelfRef{},
	}

	a := NewActor(cfg)

	// Set up tracking state as if a sweep was broadcast.
	a.expired[batchID] = &expiredBatch{
		expiryHeight: 100,
		attempts:     3,
	}
	a.pendingSweeps[batchID] = &pendingSweep{
		batchID:   batchID,
		feeRate:   btcutil.Amount(5),
		numInputs: 2,
	}

	// Send SweepConfirmedEvent.
	result := a.Receive(t.Context(), &SweepConfirmedEvent{
		BatchID:     batchID,
		BlockHeight: 120,
	})
	require.True(t, result.IsOk())

	// Verify tracking state is cleaned up.
	require.Empty(t, a.expired)
	require.Empty(t, a.pendingSweeps)
}

// TestRepeatedBatchExpiredSkipsWhenFeeNotHigher verifies that batches with
// pending sweeps are skipped when the current fee rate is not higher.
func TestRepeatedBatchExpiredSkipsWhenFeeNotHigher(t *testing.T) {
	t.Parallel()

	batchID := batchwatcher.BatchID(uuid.New())

	mockWatcher := &mockBatchWatcherRef{
		resp: &batchwatcher.GetTreeStateResponse{
			Found: true,
		},
	}

	mockChainSource := &mockChainSourceRef{
		feeEstimateResp: &chainsource.FeeEstimateResponse{
			SatPerVByte: btcutil.Amount(5),
		},
	}

	cfg := &ActorConfig{
		Log:          fn.Some(btclog.Disabled),
		BatchWatcher: mockWatcher,
		ChainSource:  mockChainSource,
		SelfRef:      &nopSelfRef{},
	}

	a := NewActor(cfg)

	// Set up tracking state with a pending sweep at higher fee rate.
	a.expired[batchID] = &expiredBatch{
		expiryHeight: 100,
		attempts:     1,
	}
	a.pendingSweeps[batchID] = &pendingSweep{
		batchID: batchID,
		feeRate: btcutil.Amount(10), // Higher than current (5)
	}

	// Send BatchExpiredEvent (per-block retry from BatchWatcher).
	result := a.Receive(t.Context(), &BatchExpiredEvent{
		Notification: &batchwatcher.BatchExpiredNotification{
			BatchID:      batchID,
			ExpiryHeight: 100,
		},
	})
	require.True(t, result.IsOk())

	// Verify watcher was NOT queried (fee bump not needed).
	require.Nil(t, mockWatcher.LastAsk())
}

// TestRepeatedBatchExpiredBumpsFeeWhenHigher verifies that batches with
// pending sweeps trigger a rebroadcast when the current fee rate is higher.
func TestRepeatedBatchExpiredBumpsFeeWhenHigher(t *testing.T) {
	t.Parallel()

	batchID := batchwatcher.BatchID(uuid.New())

	internalKey, _ := testutils.CreateKey(1)
	node := &treepkg.Node{
		CoSigners: []*btcec.PublicKey{
			internalKey,
		},
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
			Height: 115,
		},
		feeEstimateResp: &chainsource.FeeEstimateResponse{
			// Higher than pending fee rate of 5.
			SatPerVByte: btcutil.Amount(15),
		},
	}

	cfg := &ActorConfig{
		Log:          fn.Some(btclog.Disabled),
		BatchWatcher: mockWatcher,
		ChainSource:  mockChainSource,
		SweepDelay:   10,
		SelfRef:      &nopSelfRef{},
		BuildSweepTx: func(_ []*batchwatcher.Output, _ btcutil.Amount) (
			*wire.MsgTx, error) {

			tx := wire.NewMsgTx(2)
			tx.AddTxOut(wire.NewTxOut(900, []byte{0x51}))

			return tx, nil
		},
	}

	a := NewActor(cfg)

	// Set up tracking state with a pending sweep at lower fee rate.
	a.expired[batchID] = &expiredBatch{
		expiryHeight: 100,
		attempts:     1,
	}
	a.pendingSweeps[batchID] = &pendingSweep{
		batchID: batchID,
		feeRate: btcutil.Amount(5), // Lower than current (15)
	}

	// Send BatchExpiredEvent (per-block retry from BatchWatcher).
	result := a.Receive(t.Context(), &BatchExpiredEvent{
		Notification: &batchwatcher.BatchExpiredNotification{
			BatchID:      batchID,
			ExpiryHeight: 100,
		},
	})
	require.True(t, result.IsOk())

	// Verify broadcast was triggered (fee bump).
	broadcast := mockChainSource.LastBroadcast()
	require.NotNil(t, broadcast)

	// Verify pending sweep was updated with new fee rate.
	require.Equal(t, btcutil.Amount(15), a.pendingSweeps[batchID].feeRate)
}

// TestAlertOnPersistentFailure verifies that an alert is logged when sweep
// failures exceed the configured threshold.
func TestAlertOnPersistentFailure(t *testing.T) {
	t.Parallel()

	batchID := batchwatcher.BatchID(uuid.New())

	mockWatcher := &mockBatchWatcherRef{
		resp: &batchwatcher.GetTreeStateResponse{
			Found: false,
		},
	}

	mockChainSource := &mockChainSourceRef{}

	logger := &errorCountingLogger{Logger: btclog.Disabled}

	cfg := &ActorConfig{
		Log:            fn.Some[btclog.Logger](logger),
		BatchWatcher:   mockWatcher,
		ChainSource:    mockChainSource,
		SelfRef:        &nopSelfRef{},
		AlertThreshold: 3,
	}

	a := NewActor(cfg)

	// Set up batch with attempts just below threshold.
	a.expired[batchID] = &expiredBatch{
		expiryHeight: 100,
		attempts:     2,
	}

	// Simulate a failure that should trigger alert at threshold.
	testErr := errors.New("test broadcast failure")
	a.handleSweepAttemptError(t.Context(), batchID, testErr)

	// Verify attempt was incremented to threshold, lastError was
	// captured, and the initial alert was emitted exactly once.
	require.EqualValues(t, 3, a.expired[batchID].attempts)
	require.Equal(t, testErr, a.expired[batchID].lastError)
	require.Equal(
		t, 1, logger.Count(),
		"initial alert expected at threshold",
	)
}

// errorCountingLogger embeds a btclog.Logger and counts structured
// error-level calls. Non-error methods inherit the embedded logger's
// behavior. Intended for asserting that maybeAlert fires the expected
// number of times under sustained sweep failures.
type errorCountingLogger struct {
	btclog.Logger

	mu    sync.Mutex
	count int
}

// ErrorS records the structured error-level call and discards the payload.
func (l *errorCountingLogger) ErrorS(_ context.Context, _ string, _ error,
	_ ...any) {

	l.mu.Lock()
	defer l.mu.Unlock()
	l.count++
}

// CriticalS records the structured critical-level call and discards the
// payload.
func (l *errorCountingLogger) CriticalS(_ context.Context, _ string, _ error,
	_ ...any) {

	l.mu.Lock()
	defer l.mu.Unlock()
	l.count++
}

// Count returns the number of structured error-level calls captured.
func (l *errorCountingLogger) Count() int {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.count
}

// TestAlertRepeatsAtInterval verifies that after the initial alert fires at
// AlertThreshold consecutive failed attempts, subsequent alerts fire every
// AlertRepeatInterval failures and not in between.
func TestAlertRepeatsAtInterval(t *testing.T) {
	t.Parallel()

	batchID := batchwatcher.BatchID(uuid.New())

	mockWatcher := &mockBatchWatcherRef{
		resp: &batchwatcher.GetTreeStateResponse{
			Found: false,
		},
	}

	mockChainSource := &mockChainSourceRef{}

	logger := &errorCountingLogger{Logger: btclog.Disabled}

	const (
		threshold      uint32 = 3
		repeatInterval uint32 = 5
	)

	cfg := &ActorConfig{
		Log:                 fn.Some[btclog.Logger](logger),
		BatchWatcher:        mockWatcher,
		ChainSource:         mockChainSource,
		SelfRef:             &nopSelfRef{},
		AlertThreshold:      threshold,
		AlertRepeatInterval: repeatInterval,
	}

	a := NewActor(cfg)
	a.expired[batchID] = &expiredBatch{
		expiryHeight: 100,
		attempts:     0,
	}

	testErr := errors.New("test broadcast failure")

	// Drive attempts up to threshold-1: no alert expected yet.
	for i := uint32(0); i < threshold-1; i++ {
		a.handleSweepAttemptError(t.Context(), batchID, testErr)
	}
	require.EqualValues(t, threshold-1, a.expired[batchID].attempts)
	require.Equal(t, 0, logger.Count(),
		"no alert expected below threshold")

	// The next failure brings attempts to threshold and should fire the
	// initial alert.
	a.handleSweepAttemptError(t.Context(), batchID, testErr)
	require.EqualValues(t, threshold, a.expired[batchID].attempts)
	require.Equal(
		t, 1, logger.Count(),
		"initial alert expected at threshold",
	)

	// Between the initial alert and the first repeat, no further alerts
	// should fire even though failures keep happening.
	for i := uint32(0); i < repeatInterval-1; i++ {
		a.handleSweepAttemptError(t.Context(), batchID, testErr)
	}
	require.EqualValues(
		t, threshold+repeatInterval-1, a.expired[batchID].attempts,
	)
	require.Equal(
		t, 1, logger.Count(),
		"no repeat alert before reaching the repeat interval",
	)

	// The next failure brings attempts to threshold+repeatInterval and
	// should fire the first repeat alert.
	a.handleSweepAttemptError(t.Context(), batchID, testErr)
	require.EqualValues(
		t, threshold+repeatInterval, a.expired[batchID].attempts,
	)
	require.Equal(
		t, 2, logger.Count(),
		"repeat alert expected at threshold + interval",
	)

	// Drive through a second quiet window: no further alerts until the
	// next repeat boundary. Exercising two repeat cycles proves the
	// modulo arithmetic holds beyond the first boundary and isn't a
	// coincidental off-by-one.
	for i := uint32(0); i < repeatInterval-1; i++ {
		a.handleSweepAttemptError(t.Context(), batchID, testErr)
	}
	require.EqualValues(
		t, threshold+2*repeatInterval-1, a.expired[batchID].attempts,
	)
	require.Equal(t, 2, logger.Count(),
		"no alert in second quiet window")

	// The next failure brings attempts to threshold+2*repeatInterval
	// and should fire the second repeat alert.
	a.handleSweepAttemptError(t.Context(), batchID, testErr)
	require.EqualValues(
		t, threshold+2*repeatInterval, a.expired[batchID].attempts,
	)
	require.Equal(
		t, 3, logger.Count(),
		"second repeat alert expected at threshold + 2*interval",
	)
}
