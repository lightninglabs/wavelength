package rounds

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/google/uuid"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo/batchwatcher"
	dmetrics "github.com/lightninglabs/darepo/metrics"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// mockMetricsRef records every message sent to the metrics actor and
// satisfies actor.TellOnlyRef[dmetrics.Msg] so tests can assert that
// the rounds actor emits the expected metric events. We use a recording
// mock instead of the real MetricsActor so we have a deterministic
// view of what was sent (the real actor's Receive is synchronous and
// flushes to Prometheus globals, which is also fine but harder to
// assert against in isolation).
type mockMetricsRef struct {
	mu   sync.Mutex
	msgs []dmetrics.Msg
}

// ID satisfies actor.BaseActorRef.
func (m *mockMetricsRef) ID() string { return "mock-metrics" }

// Tell records the message.
func (m *mockMetricsRef) Tell(_ context.Context, msg dmetrics.Msg) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.msgs = append(m.msgs, msg)

	return nil
}

// snapshot returns a copy of all messages recorded so far.
func (m *mockMetricsRef) snapshot() []dmetrics.Msg {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]dmetrics.Msg, len(m.msgs))
	copy(out, m.msgs)

	return out
}

// installMockMetrics wires a recording metrics ref into the harness so
// tests can assert metric emission.
func installMockMetrics(h *actorTestHarness) *mockMetricsRef {
	mock := &mockMetricsRef{}
	h.cfg.MetricsActor = fn.Some(
		actor.TellOnlyRef[dmetrics.Msg](mock),
	)

	return mock
}

// safeBuffer is a goroutine-safe wrapper around bytes.Buffer used by
// installCapturingLog. The capturing log handler writes from whichever
// goroutine emits a log line (test goroutine calling
// registerBatchesWithWatcher directly, plus the actor's FSM goroutine
// driving the round started by h.start), so the underlying buffer must
// serialize concurrent Writes/Reads or the race detector flags every
// run. We only protect Write/String because those are the only
// operations the tests use; if a future caller needs more, add the
// method here rather than exposing the embedded buffer.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write implements io.Writer under the buffer's mutex so the btclog
// DefaultHandler (which calls Write from any logging goroutine) is
// safe to share between the test goroutine and the actor FSM
// goroutine.
func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

// String returns the captured log output. Holds the mutex so callers
// observe a consistent snapshot even while the actor goroutine is
// still emitting log lines.
func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// installCapturingLog replaces the harness logger with one that writes
// to an in-test buffer at LevelTrace so tests can grep the output for
// level markers ([WRN] / [ERR]). Both the harness config and the
// already-constructed Actor's log field are overwritten because the
// Actor captures the logger at construction time.
//
// The returned buffer is a *safeBuffer so the test goroutine (which
// calls registerBatchesWithWatcher directly to exercise the helper in
// isolation) and the actor's FSM goroutine (started by h.start, which
// also logs via a.log) can both write to it without tripping the race
// detector.
func installCapturingLog(h *actorTestHarness) *safeBuffer {
	buf := &safeBuffer{}
	handler := btclog.NewDefaultHandler(buf)
	logger := btclog.NewSLogger(handler.SubSystem(Subsystem))
	logger.SetLevel(btclog.LevelTrace)
	h.cfg.Log = fn.Some(logger)
	h.actor.log = logger

	return buf
}

// regBatchReq is a short local alias used so several helper signatures
// stay within the 80-char line budget without sacrificing readability.
type regBatchReq = batchwatcher.RegisterBatchRequest

// mockBatchWatcherRef is a hand-rolled actor.ActorRef test double for the
// batch watcher. It records every RegisterBatchRequest delivered via Tell
// and can be configured to fail a fixed number of times before
// succeeding, model permanent failures, or only fail for a specific
// batch ID. This lets unit tests assert the rounds actor's response to
// enqueue failures without spinning up a real actor system.
type mockBatchWatcherRef struct {
	mu sync.Mutex

	// telled records every successfully-enqueued RegisterBatchRequest.
	telled []*regBatchReq

	// callCount counts the total number of Tell invocations,
	// including failed ones. Useful for asserting retry behavior.
	callCount atomic.Int32

	// failFirstN, when > 0, causes the first N Tell calls (across
	// any batch) to return tellErr. Subsequent calls succeed.
	failFirstN int

	// failAlways, when true, causes every Tell call to return tellErr.
	failAlways bool

	// failForBatch, when non-nil, fails Tell only for the specified
	// batch ID; other batches succeed.
	failForBatch *batchwatcher.BatchID

	// tellErr is the error returned by failing Tell calls.
	tellErr error
}

// ID returns a stable identifier for the mock.
func (m *mockBatchWatcherRef) ID() string {
	return "mock-batch-watcher"
}

// Tell records the request (on success) and applies the configured
// failure policy.
func (m *mockBatchWatcherRef) Tell(_ context.Context,
	msg batchwatcher.BatchWatcherMsg) error {

	n := m.callCount.Add(1)

	m.mu.Lock()
	defer m.mu.Unlock()

	req, ok := msg.(*batchwatcher.RegisterBatchRequest)
	if !ok {
		return errors.New("unexpected message type")
	}

	if m.failAlways {
		return m.tellErr
	}

	if m.failForBatch != nil && req.BatchID == *m.failForBatch {
		return m.tellErr
	}

	if int(n) <= m.failFirstN {
		return m.tellErr
	}

	m.telled = append(m.telled, req)

	return nil
}

// Ask is unused by registerBatchesWithWatcher but is required to
// satisfy the actor.ActorRef interface.
func (m *mockBatchWatcherRef) Ask(_ context.Context,
	_ batchwatcher.BatchWatcherMsg,
) actor.Future[batchwatcher.BatchWatcherResp] {

	promise := actor.NewPromise[batchwatcher.BatchWatcherResp]()
	promise.Complete(
		fn.Err[batchwatcher.BatchWatcherResp](
			errors.New("Ask not implemented in mock"),
		),
	)

	return promise.Future()
}

// telledCopy returns a snapshot of all successfully enqueued requests.
func (m *mockBatchWatcherRef) telledCopy() []*regBatchReq {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]*regBatchReq, len(m.telled))
	copy(out, m.telled)

	return out
}

// installMockBatchWatcher wires the mock into the harness config as the
// BatchWatcher option.
func installMockBatchWatcher(h *actorTestHarness, mock *mockBatchWatcherRef) {
	type bwMsg = batchwatcher.BatchWatcherMsg
	type bwResp = batchwatcher.BatchWatcherResp

	h.cfg.BatchWatcher = fn.Some(actor.ActorRef[bwMsg, bwResp](mock))
}

// TestRegisterBatchesWithWatcherSuccess verifies that successful Tell
// calls enqueue exactly one RegisterBatchRequest per non-nil VTXO tree.
func TestRegisterBatchesWithWatcherSuccess(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	mock := &mockBatchWatcherRef{}
	installMockBatchWatcher(h, mock)
	h.setActiveRounds([]*Round{})
	h.start(h.ctx)

	roundID, err := NewRoundID()
	require.NoError(t, err)

	trees := map[int]*tree.Tree{
		0: {},
		1: nil, // should be skipped
		2: {},
	}

	err = h.actor.registerBatchesWithWatcher(
		h.ctx, roundID, 100, trees, keychain.KeyDescriptor{},
	)
	require.NoError(t, err)

	telled := mock.telledCopy()
	require.Len(t, telled, 2, "two non-nil trees should be registered")
}

// TestRegisterBatchesWithWatcherSurfacesFailure verifies that an
// always-failing watcher Tell causes registerBatchesWithWatcher to
// return a non-nil error covering every failed batch.
//
// This test fails without the fix (the function previously returned no
// error and swallowed Tell failures).
func TestRegisterBatchesWithWatcherSurfacesFailure(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	mock := &mockBatchWatcherRef{
		failAlways: true,
		tellErr:    actor.ErrActorTerminated,
	}
	installMockBatchWatcher(h, mock)
	h.setActiveRounds([]*Round{})
	h.start(h.ctx)

	roundID, err := NewRoundID()
	require.NoError(t, err)

	trees := map[int]*tree.Tree{
		0: {},
		1: {},
	}

	err = h.actor.registerBatchesWithWatcher(
		h.ctx, roundID, 100, trees, keychain.KeyDescriptor{},
	)
	require.Error(t, err, "enqueue failures must propagate to caller")
	require.ErrorIs(t, err, actor.ErrActorTerminated)

	// Each failed batch should produce its own retry burst, so the
	// total call count is at least the number of batches times the
	// max attempts.
	totalCalls := int(mock.callCount.Load())
	require.GreaterOrEqual(
		t, totalCalls, 2*batchWatcherEnqueueMaxAttempts, "each "+
			"failing batch should be retried up to "+
			"batchWatcherEnqueueMaxAttempts times",
	)

	require.Empty(
		t, mock.telledCopy(),
		"no batches should be recorded as registered on failure",
	)
}

// TestRegisterBatchesWithWatcherPartialFailure verifies that even if
// only one batch fails to enqueue, the function returns an error.
// Silently registering the rest while leaving one batch unwatched is
// the exact money-loss scenario the issue describes.
func TestRegisterBatchesWithWatcherPartialFailure(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	roundID, err := NewRoundID()
	require.NoError(t, err)

	// Pre-compute the failing batch's ID so the mock can target it
	// deterministically (map iteration order is random).
	failBatchID := batchwatcher.BatchIDForRoundOutput(
		uuid.UUID(roundID), 1,
	)

	mock := &mockBatchWatcherRef{
		failForBatch: &failBatchID,
		tellErr:      actor.ErrActorTerminated,
	}
	installMockBatchWatcher(h, mock)
	h.setActiveRounds([]*Round{})
	h.start(h.ctx)

	trees := map[int]*tree.Tree{
		0: {},
		1: {},
		2: {},
	}

	err = h.actor.registerBatchesWithWatcher(
		h.ctx, roundID, 100, trees, keychain.KeyDescriptor{},
	)
	require.Error(t, err, "partial failures must propagate to caller")

	telled := mock.telledCopy()
	require.Len(
		t, telled, 2,
		"the two healthy batches should still be registered",
	)
}

// TestRegisterBatchesWithWatcherTransientRetry verifies that a
// transient Tell failure is absorbed by the bounded retry: the second
// attempt succeeds and the function returns no error.
func TestRegisterBatchesWithWatcherTransientRetry(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	mock := &mockBatchWatcherRef{
		failFirstN: 1,
		tellErr:    actor.ErrMailboxClosed,
	}
	installMockBatchWatcher(h, mock)
	h.setActiveRounds([]*Round{})
	h.start(h.ctx)

	roundID, err := NewRoundID()
	require.NoError(t, err)

	trees := map[int]*tree.Tree{
		0: {},
	}

	err = h.actor.registerBatchesWithWatcher(
		h.ctx, roundID, 100, trees, keychain.KeyDescriptor{},
	)
	require.NoError(
		t, err,
		"a transient failure must be absorbed by the bounded retry",
	)

	telled := mock.telledCopy()
	require.Len(t, telled, 1)
	require.GreaterOrEqual(
		t,
		int(
			mock.callCount.Load(),
		),
		2, "retry must have produced at least one extra call",
	)
}

// TestRegisterBatchesWithWatcherCtxCanceled verifies that a cancelled
// caller context does NOT trigger retries — cancellation is terminal.
func TestRegisterBatchesWithWatcherCtxCanceled(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	mock := &mockBatchWatcherRef{
		failAlways: true,
		tellErr:    context.Canceled,
	}
	installMockBatchWatcher(h, mock)
	h.setActiveRounds([]*Round{})
	h.start(h.ctx)

	roundID, err := NewRoundID()
	require.NoError(t, err)

	trees := map[int]*tree.Tree{
		0: {},
	}

	err = h.actor.registerBatchesWithWatcher(
		h.ctx, roundID, 100, trees, keychain.KeyDescriptor{},
	)
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)

	// Exactly one attempt — cancellation is not retried.
	require.Equal(
		t, int32(1), mock.callCount.Load(),
		"cancellation must be terminal, no retry",
	)
}

// TestRegisterBatchesWithWatcherNoWatcher verifies that when no
// BatchWatcher is configured, registration is a no-op success. This
// supports test deployments and operators who run without on-chain
// monitoring (the BatchWatcher field is fn.Option).
func TestRegisterBatchesWithWatcherNoWatcher(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	h.setActiveRounds([]*Round{})
	h.start(h.ctx)

	roundID, err := NewRoundID()
	require.NoError(t, err)

	trees := map[int]*tree.Tree{0: {}}
	err = h.actor.registerBatchesWithWatcher(
		h.ctx, roundID, 100, trees, keychain.KeyDescriptor{},
	)
	require.NoError(t, err)
}

// TestHandleConfirmationFailsLoudlyOnWatcherFailure verifies the
// end-to-end contract of the fix:
//
//  1. handleConfirmation propagates the watcher-registration error so
//     the failure surfaces upstream instead of being silently dropped.
//  2. The round is UNTRACKED on failure (no per-round state leak).
//     ConfirmedState is terminal so leaving the round in a.rounds
//     would be an unrecoverable memory leak with no retry path.
//  3. The dedicated BatchWatcherRegisterFailedMsg metric IS emitted so
//     operators have an alertable signal independent of log volume.
//  4. The terminal RoundCompletedMsg IS still emitted (with a
//     distinguishing "confirmed_watcher_failed" status) so dashboards
//     see the round leave the active set.
//
// The previous behaviour kept the round tracked "for operator
// inspection" -- that was reviewed as a memory leak masquerading as
// observability and replaced with the dedicated metric.
func TestHandleConfirmationFailsLoudlyOnWatcherFailure(t *testing.T) {
	// No t.Parallel — BatchWatcherRegisterFailures is a process-wide
	// Prometheus counter and the test asserts an exact delta.

	h := newActorTestHarness(t)
	mock := &mockBatchWatcherRef{
		failAlways: true,
		tellErr:    errors.New("watcher boom"),
	}
	installMockBatchWatcher(h, mock)

	metricsRef := installMockMetrics(h)

	// Build a finalized round with a non-nil VTXO tree so the
	// ConfirmedState produced by the FSM has a tree to register.
	roundID, err := NewRoundID()
	require.NoError(t, err)

	finalTx := wire.NewMsgTx(2)
	finalTx.AddTxOut(&wire.TxOut{
		Value:    100000,
		PkScript: []byte{0x00, 0x14, 0x01, 0x02, 0x03},
	})

	persistedRound := &Round{
		RoundID: roundID,
		FinalTx: finalTx,
		VTXOTrees: map[int]*tree.Tree{
			0: {},
		},
		ClientRegistrations: map[ClientID]*ClientRegistration{
			"client1": {},
		},
		SweepKey: h.cfg.Terms.SweepKey.PubKey,
		CSVDelay: h.cfg.Terms.SweepDelay,
	}

	h.setActiveRounds([]*Round{persistedRound})
	h.start(h.ctx)

	// Manually associate a client with the round so we can later
	// assert client tracking state was cleaned up.
	h.actor.trackClientJoin(h.ctx, "client1", roundID)

	// Snapshot the registered Prometheus counter so we can assert an
	// exact delta even when other tests in this process have already
	// incremented it.
	before := testutil.ToFloat64(dmetrics.BatchWatcherRegisterFailures)

	confMsg := &ConfirmationMsg{
		RoundID:     roundID,
		BlockHeight: 100,
		BlockHash:   chainhash.HashH([]byte("blk")),
		NumConfs:    1,
	}

	result := h.actor.Receive(h.ctx, confMsg)
	_, err = result.Unpack()
	require.Error(
		t, err,
		"handleConfirmation must surface watcher enqueue failure",
	)

	// Client-round mapping MUST be cleaned up (same path as the
	// success branch's untrackRound call). Leaving entries in
	// a.clientRounds for a terminal round is a leak with no recovery
	// path -- the operator-visible signal is the
	// BatchWatcherRegisterFailures counter, not lingering state.
	clientRounds := h.getClientRounds("client1")
	require.NotContains(
		t, clientRounds, roundID,
		"client must be untracked even on watcher failure",
	)

	// The round FSM itself MUST also be dropped from a.rounds.
	// untrackRound only clears the per-client mapping; without an
	// explicit delete the terminal ConfirmedState round would linger
	// in the FSM map forever (no retry path exists for the
	// watcher-failure branch).
	_, stillTracked := h.actor.rounds[roundID]
	require.False(
		t, stillTracked,
		"round FSM must be removed from a.rounds on watcher failure",
	)

	// The dedicated failure metric MUST be emitted via the metrics
	// actor. We assert both on the recording mock (proving the
	// rounds actor emitted the message) and on the underlying
	// Prometheus counter through a real metrics actor pass below.
	var (
		sawBatchFailed   *dmetrics.BatchWatcherRegisterFailedMsg
		sawRoundComplete *dmetrics.RoundCompletedMsg
	)
	for _, m := range metricsRef.snapshot() {
		switch tm := m.(type) {
		case *dmetrics.BatchWatcherRegisterFailedMsg:
			sawBatchFailed = tm

		case *dmetrics.RoundCompletedMsg:
			sawRoundComplete = tm
		}
	}
	require.NotNil(
		t, sawBatchFailed,
		"BatchWatcherRegisterFailedMsg must be emitted on failure",
	)
	require.Equal(t, roundID.String(), sawBatchFailed.RoundID)
	require.GreaterOrEqual(
		t, sawBatchFailed.BatchCount, 1,
		"BatchCount must reflect at least one failed batch",
	)
	require.NotNil(
		t, sawRoundComplete, "RoundCompletedMsg must still be "+
			"emitted so dashboards see the round leave the "+
			"active set",
	)
	require.Equal(
		t, "confirmed_watcher_failed", sawRoundComplete.Status, "wat"+
			"cher-failure path must use a distinct status so "+
			"dashboards differentiate it from clean confirms",
	)

	// Round through the real MetricsActor to verify the counter
	// actually advances when the message is processed end-to-end.
	// This catches plumbing regressions (wrong message type wired in
	// metrics/actor.go, etc.) that the mock cannot.
	metricsActor := dmetrics.NewMetricsActor(dmetrics.ActorConfig{})
	metricsActor.Receive(h.ctx, sawBatchFailed)

	after := testutil.ToFloat64(dmetrics.BatchWatcherRegisterFailures)
	require.Equal(
		t, before+float64(sawBatchFailed.BatchCount), after, "counte"+
			"r must advance by BatchCount when message is "+
			"processed by the metrics actor",
	)
}

// TestRegisterBatchesWithWatcherShutdownErrorsWarnNotError verifies
// the log-level classification fix: externally-triggered errors
// (actor terminated, mailbox closed, context cancel/deadline) must be
// logged at Warn, not Error. CLAUDE.md mandates `error` level is
// reserved for internal bugs; logging shutdown noise at error spams
// alerts during graceful drain. The operator-alert signal is the
// BatchWatcherRegisterFailures counter, which fires regardless of
// log level.
func TestRegisterBatchesWithWatcherShutdownErrorsWarnNotError(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
	}{
		{
			"actor_terminated",
			actor.ErrActorTerminated,
		},
		{
			"mailbox_closed",
			actor.ErrMailboxClosed,
		},
		{
			"context_canceled",
			context.Canceled,
		},
		{
			"context_deadline",
			context.DeadlineExceeded,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newActorTestHarness(t)
			buf := installCapturingLog(h)

			mock := &mockBatchWatcherRef{
				failAlways: true,
				tellErr:    tc.err,
			}
			installMockBatchWatcher(h, mock)
			h.setActiveRounds([]*Round{})
			h.start(h.ctx)

			roundID, err := NewRoundID()
			require.NoError(t, err)

			err = h.actor.registerBatchesWithWatcher(
				h.ctx, roundID, 100, map[int]*tree.Tree{
					0: {},
				},
				keychain.KeyDescriptor{},
			)
			require.Error(t, err)

			out := buf.String()
			require.Contains(
				t, out, "[WRN]", "shutdown error %q must "+
					"log at warn level", tc.err,
			)
			require.NotContains(
				t, out, "[ERR]", "shutdown error %q must "+
					"NOT log at error level (CLAUDE.md: "+
					"error is for internal bugs only)",
				tc.err,
			)
		})
	}
}

// TestRegisterBatchesWithWatcherInternalErrorLogsAsError is the dual
// of the shutdown-warns test: a non-shutdown error (e.g., a generic
// internal failure) MUST log at Error level so internal bugs page
// operators. Without this, demoting all errors to Warn would hide
// real bugs.
func TestRegisterBatchesWithWatcherInternalErrorLogsAsError(t *testing.T) {
	t.Parallel()

	h := newActorTestHarness(t)
	buf := installCapturingLog(h)

	mock := &mockBatchWatcherRef{
		failAlways: true,
		tellErr:    errors.New("some internal bug"),
	}
	installMockBatchWatcher(h, mock)
	h.setActiveRounds([]*Round{})
	h.start(h.ctx)

	roundID, err := NewRoundID()
	require.NoError(t, err)

	err = h.actor.registerBatchesWithWatcher(
		h.ctx, roundID, 100, map[int]*tree.Tree{
			0: {},
		},
		keychain.KeyDescriptor{},
	)
	require.Error(t, err)

	out := buf.String()
	// Trim WRN from match: a non-shutdown internal error must hit the
	// ErrorS path. We tolerate WRN noise from other log sites but the
	// batch-watcher line specifically should be [ERR].
	require.Contains(
		t, out, "[ERR]", "non-shutdown error must log at error level",
	)
	require.True(
		t, strings.Contains(out, "some internal bug"),
		"the underlying error must appear in the error log line "+
			"for debugging",
	)
}
