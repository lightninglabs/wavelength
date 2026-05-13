package batchsweeper

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
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
	txid := chainhash.Hash{0xaa}
	a.pendingSweeps[batchID] = map[chainhash.Hash]*pendingSweep{
		txid: {
			txid:      txid,
			batchID:   batchID,
			feeRate:   btcutil.Amount(5),
			numInputs: 2,
		},
	}

	// Send SweepConfirmedEvent matching the broadcast txid.
	result := a.Receive(t.Context(), &SweepConfirmedEvent{
		BatchID:     batchID,
		Txid:        txid,
		BlockHeight: 120,
	})
	require.True(t, result.IsOk())

	// Verify tracking state is cleaned up.
	require.Empty(t, a.expired)
	require.Empty(t, a.pendingSweeps)
}

// TestSweepConfirmedIgnoresUnknownTxid verifies that a confirmation whose
// txid does not match the tracked pending sweep is ignored: pending and
// expired state must stay intact so the real confirmation can still clear
// them. This protects the actor against reorgs and against unrelated
// transactions confirming at a watched outpoint.
func TestSweepConfirmedIgnoresUnknownTxid(t *testing.T) {
	t.Parallel()

	batchID := batchwatcher.BatchID(uuid.New())

	cfg := &ActorConfig{
		Log:          fn.Some(btclog.Disabled),
		BatchWatcher: &mockBatchWatcherRef{},
		ChainSource:  &mockChainSourceRef{},
		SelfRef:      &nopSelfRef{},
	}

	a := NewActor(cfg)

	pendingTxid := chainhash.Hash{0x11}
	a.expired[batchID] = &expiredBatch{
		expiryHeight: 100,
		attempts:     2,
	}
	a.pendingSweeps[batchID] = map[chainhash.Hash]*pendingSweep{
		pendingTxid: {
			txid:      pendingTxid,
			batchID:   batchID,
			feeRate:   btcutil.Amount(5),
			numInputs: 2,
		},
	}

	// Send SweepConfirmedEvent for an UNRELATED txid.
	unrelatedTxid := chainhash.Hash{0x22}
	result := a.Receive(t.Context(), &SweepConfirmedEvent{
		BatchID:     batchID,
		Txid:        unrelatedTxid,
		BlockHeight: 120,
	})
	require.True(t, result.IsOk())

	// Pending and expired entries must still be present so the
	// legitimate confirmation can still clear them later.
	require.Contains(t, a.pendingSweeps, batchID)
	require.Contains(t, a.pendingSweeps[batchID], pendingTxid)
	require.Contains(t, a.expired, batchID)
}

// TestConcurrentSweepsTrackedIndependently verifies that two sweeps in flight
// for the same batch (for example a subtree-branch sweep and a separate root
// sweep, or a fee-bump rebroadcast) are tracked independently and each clears
// only when its own confirmation arrives.
func TestConcurrentSweepsTrackedIndependently(t *testing.T) {
	t.Parallel()

	batchID := batchwatcher.BatchID(uuid.New())

	cfg := &ActorConfig{
		Log:          fn.Some(btclog.Disabled),
		BatchWatcher: &mockBatchWatcherRef{},
		ChainSource:  &mockChainSourceRef{},
		SelfRef:      &nopSelfRef{},
	}

	a := NewActor(cfg)

	txidA := chainhash.Hash{0xaa}
	txidB := chainhash.Hash{0xbb}

	a.expired[batchID] = &expiredBatch{
		expiryHeight: 100,
		attempts:     1,
	}
	a.pendingSweeps[batchID] = map[chainhash.Hash]*pendingSweep{
		txidA: {
			txid:      txidA,
			batchID:   batchID,
			feeRate:   btcutil.Amount(5),
			numInputs: 1,
		},
		txidB: {
			txid:      txidB,
			batchID:   batchID,
			feeRate:   btcutil.Amount(7),
			numInputs: 1,
		},
	}

	// Confirm sweep A. Only its entry should clear; sweep B and the
	// per-batch expired bookkeeping must remain so B can still confirm.
	resA := a.Receive(t.Context(), &SweepConfirmedEvent{
		BatchID:     batchID,
		Txid:        txidA,
		BlockHeight: 120,
	})
	require.True(t, resA.IsOk())

	require.Contains(t, a.pendingSweeps, batchID)
	require.NotContains(t, a.pendingSweeps[batchID], txidA)
	require.Contains(t, a.pendingSweeps[batchID], txidB)
	require.Contains(t, a.expired, batchID)

	// Confirm sweep B. Now the batch is fully cleared.
	resB := a.Receive(t.Context(), &SweepConfirmedEvent{
		BatchID:     batchID,
		Txid:        txidB,
		BlockHeight: 121,
	})
	require.True(t, resB.IsOk())

	require.Empty(t, a.pendingSweeps)
	require.Empty(t, a.expired)
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
	txid := chainhash.Hash{0xcc}
	a.pendingSweeps[batchID] = map[chainhash.Hash]*pendingSweep{
		txid: {
			txid:    txid,
			batchID: batchID,
			feeRate: btcutil.Amount(10), // Higher than current (5)
		},
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
	oldTxid := chainhash.Hash{0xdd}
	a.pendingSweeps[batchID] = map[chainhash.Hash]*pendingSweep{
		oldTxid: {
			txid:    oldTxid,
			batchID: batchID,
			// Lower than current (15).
			feeRate: btcutil.Amount(5),
		},
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

	// The rebroadcast is tracked as a new pending sweep keyed by its
	// own txid; the original entry stays in-flight until its own
	// confirmation arrives. The new sweep must reflect the bumped rate.
	pending, ok := a.pendingSweeps[batchID]
	require.True(t, ok)
	var foundBumped bool
	for _, p := range pending {
		if p.feeRate == btcutil.Amount(15) {
			foundBumped = true
		}
	}
	require.True(t, foundBumped, "expected bumped sweep at fee rate 15")
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

// newSingleLeafTree constructs a minimal one-leaf tree suitable for driving
// handleBatchSwept. Returning a real tree (instead of a hand-rolled struct)
// keeps the test honest with respect to the leaf-iteration code path.
func newSingleLeafTree(t *testing.T) *treepkg.Tree {
	t.Helper()

	operatorKey, _ := testutils.CreateKey(1)
	clientKey, _ := testutils.CreateKey(100)

	batchOutpoint := wire.OutPoint{
		Hash: [32]byte{
			1,
			2,
			3,
			4,
		},
		Index: 0,
	}

	leafAmount := btcutil.Amount(100_000)
	batchOutput := wire.NewTxOut(int64(leafAmount), []byte{0x51})

	leaf := treepkg.LeafDescriptor{
		CoSignerKey: clientKey,
		Amount:      leafAmount,
		PkScript: []byte{
			0x51,
			0x20,
			0x01,
			0x02,
		},
	}

	tree, err := treepkg.NewTree(
		batchOutpoint, batchOutput, []treepkg.LeafDescriptor{leaf},
		operatorKey, []byte{0xaa, 0xbb, 0xcc}, 2,
	)
	require.NoError(t, err)

	return tree
}

// TestHandleBatchSweptCallbackErrorKeepsState is the regression test for the
// security half of issue #364 on the sweeper side: if OnBatchSwept returns
// an error (e.g. a transient DB failure marking VTXOs expired), the actor
// must NOT drop its expired/pendingSweeps tracking entries. The original
// code deleted them up-front and then bubbled the error, so any retry of
// the message would find the batch already forgotten and would silently
// succeed — leaving the VTXOs in "live" status forever.
//
// This test also asserts that the sweeper persists the derived outpoints
// in pendingSweptCallbacks and schedules a timer-driven retry, so the
// callback is automatically re-attempted in-process without relying on
// upstream (already-unregistered) redelivery from the watcher.
func TestHandleBatchSweptCallbackErrorKeepsState(t *testing.T) {
	t.Parallel()

	batchID := batchwatcher.BatchID(uuid.New())

	callbackErr := errors.New("transient db failure")
	var callbackInvocations int
	mockTimeout := &mockTimeoutRef{}
	cfg := &ActorConfig{
		Log:          fn.Some(btclog.Disabled),
		BatchWatcher: &mockBatchWatcherRef{},
		ChainSource:  &mockChainSourceRef{},
		SelfRef:      &nopSelfRef{},
		TimeoutActor: fn.Some[actor.TellOnlyRef[timeout.Msg]](
			mockTimeout,
		),
		OnBatchSwept: func(_ context.Context, _ []wire.OutPoint) error {
			callbackInvocations++

			return callbackErr
		},
	}

	a := NewActor(cfg)

	// Seed tracking state as if a sweep was previously broadcast.
	a.expired[batchID] = &expiredBatch{
		expiryHeight: 100,
		attempts:     1,
	}
	a.pendingSweeps[batchID] = map[chainhash.Hash]*pendingSweep{
		{}: {
			batchID: batchID,
		},
	}

	tree := newSingleLeafTree(t)

	result := a.Receive(t.Context(), &BatchSweptEvent{
		Notification: &batchwatcher.BatchSweptNotification{
			BatchID: batchID,
			Tree:    tree,
		},
	})
	require.True(
		t, result.IsErr(),
		"callback failure must surface to the caller so the "+
			"message can be retried",
	)
	require.ErrorIs(t, result.Err(), callbackErr)
	require.Equal(t, 1, callbackInvocations)

	// Tracking state must remain so a retry can re-invoke the callback.
	require.Contains(
		t, a.expired, batchID,
		"expired entry must remain so retries can replay",
	)
	require.Contains(
		t, a.pendingSweeps, batchID,
		"pendingSweeps entry must remain so retries can replay",
	)
	require.Contains(
		t, a.pendingSweptCallbacks, batchID, "derived outpoints "+
			"must be persisted so the retry path can replay "+
			"without re-deriving from the tree",
	)
	require.Equal(t, uint32(1), a.pendingSweptCallbacks[batchID].attempts)

	// A retry timer must be scheduled — the watcher already unregistered
	// the batch upstream, so this is the only path to re-attempt.
	scheduled := mockTimeout.LastSchedule()
	require.NotNil(
		t, scheduled,
		"a retry timer must be scheduled on callback failure",
	)
}

// TestHandleBatchSweptMissingCallbackErrors verifies that a nil OnBatchSwept
// returns an error rather than silently succeeding. The earlier code
// returned OK in this case, which masked a wiring bug that would leave
// VTXOs live after every swept batch.
func TestHandleBatchSweptMissingCallbackErrors(t *testing.T) {
	t.Parallel()

	batchID := batchwatcher.BatchID(uuid.New())

	cfg := &ActorConfig{
		Log:          fn.Some(btclog.Disabled),
		BatchWatcher: &mockBatchWatcherRef{},
		ChainSource:  &mockChainSourceRef{},
		SelfRef:      &nopSelfRef{},
		// OnBatchSwept intentionally left nil to simulate a wiring
		// regression.
	}

	a := NewActor(cfg)
	a.expired[batchID] = &expiredBatch{expiryHeight: 100}
	a.pendingSweeps[batchID] = map[chainhash.Hash]*pendingSweep{
		{}: {
			batchID: batchID,
		},
	}

	tree := newSingleLeafTree(t)

	result := a.Receive(t.Context(), &BatchSweptEvent{
		Notification: &batchwatcher.BatchSweptNotification{
			BatchID: batchID,
			Tree:    tree,
		},
	})
	require.True(
		t, result.IsErr(),
		"missing OnBatchSwept must surface as an error, not be "+
			"silently swallowed",
	)

	// Tracking state must remain so a subsequent restart with proper
	// wiring can complete the VTXO marking.
	require.Contains(t, a.expired, batchID)
	require.Contains(t, a.pendingSweeps, batchID)
}

// TestHandleBatchSweptSuccessClearsState verifies the happy path: a
// successful callback advances VTXO marking and only then drops local
// tracking state.
func TestHandleBatchSweptSuccessClearsState(t *testing.T) {
	t.Parallel()

	batchID := batchwatcher.BatchID(uuid.New())

	var observed []wire.OutPoint
	cfg := &ActorConfig{
		Log:          fn.Some(btclog.Disabled),
		BatchWatcher: &mockBatchWatcherRef{},
		ChainSource:  &mockChainSourceRef{},
		SelfRef:      &nopSelfRef{},
		OnBatchSwept: func(_ context.Context,
			ops []wire.OutPoint) error {

			observed = append(observed, ops...)

			return nil
		},
	}

	a := NewActor(cfg)
	a.expired[batchID] = &expiredBatch{expiryHeight: 100}
	a.pendingSweeps[batchID] = map[chainhash.Hash]*pendingSweep{
		{}: {
			batchID: batchID,
		},
	}

	tree := newSingleLeafTree(t)
	subtreeTxid, err := tree.Root.TXID()
	require.NoError(t, err)

	a.pendingSubtreeSweptCallbacks[subtreeSweptKey{
		batchID:     batchID,
		subtreeTxid: subtreeTxid,
	}] = &pendingSweptCallback{}

	result := a.Receive(t.Context(), &BatchSweptEvent{
		Notification: &batchwatcher.BatchSweptNotification{
			BatchID: batchID,
			Tree:    tree,
		},
	})
	require.True(t, result.IsOk())
	require.Len(
		t, observed, 1, "each leaf must be reported exactly once",
	)
	require.NotContains(t, a.expired, batchID)
	require.NotContains(t, a.pendingSweeps, batchID)
	require.NotContains(t, a.pendingSweptCallbacks, batchID)
	require.Empty(
		t, a.pendingSubtreeSweptCallbacks,
		"root sweep must clear redundant subtree retry callbacks",
	)
}

// TestHandleBatchSweptCallbackRetryEventuallySucceeds is the regression
// test for the in-process retry concern raised on PR #425: when the first
// OnBatchSwept attempt fails, the watcher (which has already enqueued the
// notification via Tell) cannot redeliver. The sweeper must therefore
// drive its own retry loop until the callback succeeds; only then may it
// drop the batch tracking state. This test injects a failing callback for
// the first invocation, simulates the timer-driven retry, and asserts:
//
//   - the watcher-side Tell is NOT what unblocks recovery (we never call it
//     again here)
//   - the DB mark eventually commits (callback observes the leaves)
//   - all tracking state (expired/pendingSweeps/pendingSweptCallbacks) is
//     cleared only once the mark succeeds.
func TestHandleBatchSweptCallbackRetryEventuallySucceeds(t *testing.T) {
	t.Parallel()

	batchID := batchwatcher.BatchID(uuid.New())

	var (
		callbackInvocations int
		observed            []wire.OutPoint
	)
	transientErr := errors.New("transient db failure")
	mockTimeout := &mockTimeoutRef{}
	cfg := &ActorConfig{
		Log:          fn.Some(btclog.Disabled),
		BatchWatcher: &mockBatchWatcherRef{},
		ChainSource:  &mockChainSourceRef{},
		SelfRef:      &nopSelfRef{},
		TimeoutActor: fn.Some[actor.TellOnlyRef[timeout.Msg]](
			mockTimeout,
		),
		OnBatchSwept: func(_ context.Context,
			ops []wire.OutPoint) error {

			callbackInvocations++

			// First attempt fails; subsequent retries succeed.
			if callbackInvocations == 1 {
				return transientErr
			}

			observed = append(observed, ops...)

			return nil
		},
	}

	a := NewActor(cfg)
	a.expired[batchID] = &expiredBatch{expiryHeight: 100}
	a.pendingSweeps[batchID] = map[chainhash.Hash]*pendingSweep{
		{}: {
			batchID: batchID,
		},
	}

	tree := newSingleLeafTree(t)

	// First delivery — callback fails. Watcher-side bookkeeping in the
	// production wiring would already have moved on by this point, so
	// the sweeper must own recovery from here.
	first := a.Receive(t.Context(), &BatchSweptEvent{
		Notification: &batchwatcher.BatchSweptNotification{
			BatchID: batchID,
			Tree:    tree,
		},
	})
	require.True(t, first.IsErr())
	require.ErrorIs(t, first.Err(), transientErr)

	// Tracking state survives the failure, and a retry timer is armed.
	require.Contains(t, a.expired, batchID)
	require.Contains(t, a.pendingSweeps, batchID)
	require.Contains(t, a.pendingSweptCallbacks, batchID)
	require.Empty(
		t, observed,
		"the failing first attempt must not have advanced the mark",
	)

	scheduled := mockTimeout.LastSchedule()
	require.NotNil(t, scheduled, "retry timer must be scheduled")

	// Fire the retry the way the timeout actor would in production: by
	// Telling the sweeper the BatchSweptCallbackRetryEvent its mapped
	// callback constructs.
	retry := a.Receive(t.Context(), &BatchSweptCallbackRetryEvent{
		BatchID: batchID,
	})
	require.True(
		t, retry.IsOk(),
		"retry attempt must succeed once the transient failure clears",
	)

	require.Equal(
		t, 2, callbackInvocations,
		"retry must invoke OnBatchSwept exactly once more",
	)
	require.Len(
		t, observed, 1, "the successful retry must surface the "+
			"leaf outpoint to the durable callback",
	)

	// All tracking state must be cleared only AFTER the DB mark commits.
	require.NotContains(t, a.expired, batchID)
	require.NotContains(t, a.pendingSweeps, batchID)
	require.NotContains(t, a.pendingSweptCallbacks, batchID)
}

// TestHandleBatchSweptCallbackRetryNoOpAfterClear verifies that a stale
// retry event for a batch whose callback already succeeded is a benign
// no-op rather than re-invoking the callback or panicking. This guards
// against double-marking when a restart-driven replay races a timer.
func TestHandleBatchSweptCallbackRetryNoOpAfterClear(t *testing.T) {
	t.Parallel()

	batchID := batchwatcher.BatchID(uuid.New())

	var callbackInvocations int
	cfg := &ActorConfig{
		Log:          fn.Some(btclog.Disabled),
		BatchWatcher: &mockBatchWatcherRef{},
		ChainSource:  &mockChainSourceRef{},
		SelfRef:      &nopSelfRef{},
		OnBatchSwept: func(_ context.Context, _ []wire.OutPoint) error {
			callbackInvocations++

			return nil
		},
	}

	a := NewActor(cfg)

	result := a.Receive(t.Context(), &BatchSweptCallbackRetryEvent{
		BatchID: batchID,
	})
	require.True(t, result.IsOk())
	require.Zero(
		t, callbackInvocations,
		"retry for an unknown batch must not invoke the callback",
	)
}

// TestBatchSubtreeSweptMarksDescendantVTXOsExpired verifies that the
// BatchSubtreeSweptEvent handler extracts every descendant VTXO leaf
// outpoint from the swept subtree and feeds them to OnBatchSwept so the
// storage layer can mark them expired. This is the load-bearing
// invariant that prevents already-swept descendants from re-entering a
// later round as forfeit inputs.
func TestBatchSubtreeSweptMarksDescendantVTXOsExpired(t *testing.T) {
	t.Parallel()

	batchID := batchwatcher.BatchID(uuid.New())

	// Build a small VTXO tree so we have a real *tree.Node with two
	// descendant leaves under the root.
	operatorKey, _ := testutils.CreateKey(1)
	clientKeyA, _ := testutils.CreateKey(2)
	clientKeyB, _ := testutils.CreateKey(3)

	leaves := []treepkg.LeafDescriptor{
		{
			CoSignerKey: clientKeyA,
			Amount:      btcutil.Amount(50_000),
			PkScript: []byte{
				0x51, 0x20, 0x01, 0x02,
			},
		},
		{
			CoSignerKey: clientKeyB,
			Amount:      btcutil.Amount(30_000),
			PkScript: []byte{
				0x51, 0x20, 0x03, 0x04,
			},
		},
	}

	batchOutpoint := wire.OutPoint{
		Hash: [32]byte{
			0xab, 0xcd,
		},
		Index: 0,
	}
	batchOutput := wire.NewTxOut(80_000, []byte{0x51})

	testTree, err := treepkg.NewTree(
		batchOutpoint, batchOutput, leaves, operatorKey,
		[]byte{0xaa, 0xbb, 0xcc}, 2,
	)
	require.NoError(t, err)
	require.False(t, testTree.Root.IsLeaf())

	// expectedOutpoints collects what OnBatchSwept must be called with.
	var expectedOutpoints []wire.OutPoint
	for leaf := range testTree.Root.LeavesIter() {
		txid, txErr := leaf.TXID()
		require.NoError(t, txErr)

		expectedOutpoints = append(expectedOutpoints, wire.OutPoint{
			Hash:  txid,
			Index: 0,
		})
	}
	require.Len(t, expectedOutpoints, 2)

	var (
		mu              sync.Mutex
		onSweptOutpoint []wire.OutPoint
		onSweptCalls    int
	)
	cfg := &ActorConfig{
		Log:          fn.Some(btclog.Disabled),
		BatchWatcher: &mockBatchWatcherRef{},
		ChainSource:  &mockChainSourceRef{},
		SelfRef:      &nopSelfRef{},
		OnBatchSwept: func(_ context.Context,
			outpoints []wire.OutPoint) error {

			mu.Lock()
			defer mu.Unlock()

			onSweptCalls++
			onSweptOutpoint = append(
				onSweptOutpoint, outpoints...,
			)

			return nil
		},
	}

	a := NewActor(cfg)

	result := a.Receive(t.Context(), &BatchSubtreeSweptEvent{
		Notification: &batchwatcher.BatchSubtreeSweptNotification{
			BatchID:     batchID,
			SubtreeRoot: testTree.Root,
		},
	})
	require.True(t, result.IsOk())

	mu.Lock()
	defer mu.Unlock()

	require.Equal(t, 1, onSweptCalls)
	require.ElementsMatch(t, expectedOutpoints, onSweptOutpoint)
}

// TestBatchSubtreeSweptNoCallbackIsError asserts that the BatchSubtreeSwept
// handler surfaces a missing OnBatchSwept callback as an error, mirroring
// the root-sweep guard. Without this guard a wiring bug would silently
// leave the descendant VTXOs in "live" status after their ancestor was
// swept on-chain.
func TestBatchSubtreeSweptNoCallbackIsError(t *testing.T) {
	t.Parallel()

	batchID := batchwatcher.BatchID(uuid.New())
	tree := newSingleLeafTree(t)

	cfg := &ActorConfig{
		Log:          fn.Some(btclog.Disabled),
		BatchWatcher: &mockBatchWatcherRef{},
		ChainSource:  &mockChainSourceRef{},
		SelfRef:      &nopSelfRef{},
		// OnBatchSwept intentionally unset.
	}

	a := NewActor(cfg)

	result := a.Receive(t.Context(), &BatchSubtreeSweptEvent{
		Notification: &batchwatcher.BatchSubtreeSweptNotification{
			BatchID:     batchID,
			SubtreeRoot: tree.Root,
		},
	})
	require.True(t, result.IsErr())
	require.ErrorContains(t, result.Err(), "OnBatchSwept")
}

// TestBatchSubtreeSweptNilSubtreeIsError asserts that the
// BatchSubtreeSwept handler surfaces a nil subtree root as an error rather
// than silently dropping the VTXO-expiry signal.
func TestBatchSubtreeSweptNilSubtreeIsError(t *testing.T) {
	t.Parallel()

	batchID := batchwatcher.BatchID(uuid.New())

	cfg := &ActorConfig{
		Log:          fn.Some(btclog.Disabled),
		BatchWatcher: &mockBatchWatcherRef{},
		ChainSource:  &mockChainSourceRef{},
		SelfRef:      &nopSelfRef{},
		OnBatchSwept: func(_ context.Context, _ []wire.OutPoint) error {
			return nil
		},
	}

	a := NewActor(cfg)

	result := a.Receive(t.Context(), &BatchSubtreeSweptEvent{
		Notification: &batchwatcher.BatchSubtreeSweptNotification{
			BatchID:     batchID,
			SubtreeRoot: nil,
		},
	})
	require.True(t, result.IsErr())
	require.ErrorContains(t, result.Err(), "nil subtree root")
}

// TestHandleBatchSubtreeSweptCallbackRetryEventuallySucceeds is the
// regression test mirroring TestHandleBatchSweptCallbackRetryEventuallySucceeds
// for the subtree-sweep path: when OnBatchSwept fails on the first attempt,
// the sweeper must retain the derived outpoints in
// pendingSubtreeSweptCallbacks and drive a timer-based retry until the
// callback succeeds. The watcher does not redeliver subtree-sweep
// notifications, so without this loop a transient DB error would silently
// leave the descendant VTXOs marked live.
func TestHandleBatchSubtreeSweptCallbackRetryEventuallySucceeds(t *testing.T) {
	t.Parallel()

	batchID := batchwatcher.BatchID(uuid.New())

	var (
		callbackInvocations int
		observed            []wire.OutPoint
	)
	transientErr := errors.New("transient db failure")
	mockTimeout := &mockTimeoutRef{}
	cfg := &ActorConfig{
		Log:          fn.Some(btclog.Disabled),
		BatchWatcher: &mockBatchWatcherRef{},
		ChainSource:  &mockChainSourceRef{},
		SelfRef:      &nopSelfRef{},
		TimeoutActor: fn.Some[actor.TellOnlyRef[timeout.Msg]](
			mockTimeout,
		),
		OnBatchSwept: func(_ context.Context,
			ops []wire.OutPoint) error {

			callbackInvocations++

			// First attempt fails; subsequent retries succeed.
			if callbackInvocations == 1 {
				return transientErr
			}

			observed = append(observed, ops...)

			return nil
		},
	}

	a := NewActor(cfg)

	tree := newSingleLeafTree(t)
	subtreeTxid, err := tree.Root.TXID()
	require.NoError(t, err)

	// First delivery — callback fails. The watcher's notification has
	// already been consumed at this point, so the sweeper owns recovery.
	first := a.Receive(t.Context(), &BatchSubtreeSweptEvent{
		Notification: &batchwatcher.BatchSubtreeSweptNotification{
			BatchID:     batchID,
			SubtreeRoot: tree.Root,
		},
	})
	require.True(t, first.IsErr())
	require.ErrorIs(t, first.Err(), transientErr)

	key := subtreeSweptKey{
		batchID:     batchID,
		subtreeTxid: subtreeTxid,
	}
	require.Contains(t, a.pendingSubtreeSweptCallbacks, key)
	require.Empty(
		t, observed,
		"the failing first attempt must not have advanced the mark",
	)

	scheduled := mockTimeout.LastSchedule()
	require.NotNil(t, scheduled, "retry timer must be scheduled")

	// Fire the retry as the timeout actor would.
	retry := a.Receive(
		t.Context(), &BatchSubtreeSweptCallbackRetryEvent{
			BatchID:     batchID,
			SubtreeTxid: subtreeTxid,
		},
	)
	require.True(
		t, retry.IsOk(),
		"retry must succeed once the transient failure clears",
	)

	require.Equal(
		t, 2, callbackInvocations,
		"retry must invoke OnBatchSwept exactly once more",
	)
	require.Len(
		t, observed, 1, "the successful retry must surface the "+
			"leaf outpoint to the durable callback",
	)
	require.NotContains(t, a.pendingSubtreeSweptCallbacks, key)
}

// TestHandleBatchSubtreeSweptCallbackRetryNoOpAfterClear verifies that a
// stale subtree-sweep retry event for a (batch, subtree) whose callback
// already succeeded is a benign no-op.
func TestHandleBatchSubtreeSweptCallbackRetryNoOpAfterClear(t *testing.T) {
	t.Parallel()

	batchID := batchwatcher.BatchID(uuid.New())

	var callbackInvocations int
	cfg := &ActorConfig{
		Log:          fn.Some(btclog.Disabled),
		BatchWatcher: &mockBatchWatcherRef{},
		ChainSource:  &mockChainSourceRef{},
		SelfRef:      &nopSelfRef{},
		OnBatchSwept: func(_ context.Context, _ []wire.OutPoint) error {
			callbackInvocations++

			return nil
		},
	}

	a := NewActor(cfg)

	result := a.Receive(
		t.Context(), &BatchSubtreeSweptCallbackRetryEvent{
			BatchID:     batchID,
			SubtreeTxid: chainhash.Hash{0x01},
		},
	)
	require.True(t, result.IsOk())
	require.Zero(
		t, callbackInvocations,
		"retry for an unknown subtree must not invoke the callback",
	)
}
