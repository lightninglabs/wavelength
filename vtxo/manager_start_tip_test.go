package vtxo

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/chainsource"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// orderLog records, in order, the store writes and tip asks a test cares
// about. The manager, the actor, and the AskThen helper write to it from
// different goroutines.
type orderLog struct {
	mu      sync.Mutex
	entries []string
	added   chan struct{}
}

// newOrderLog creates an empty log.
func newOrderLog() *orderLog {
	return &orderLog{
		added: make(chan struct{}, 16),
	}
}

// add appends an entry and wakes a waiting test.
func (l *orderLog) add(entry string) {
	l.mu.Lock()
	l.entries = append(l.entries, entry)
	l.mu.Unlock()

	l.added <- struct{}{}
}

// snapshot returns a copy of the entries so far.
func (l *orderLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return append([]string(nil), l.entries...)
}

// waitFor blocks until the log holds n entries.
func (l *orderLog) waitFor(t *testing.T, n int) []string {
	t.Helper()

	deadline := time.After(5 * time.Second)
	for {
		entries := l.snapshot()
		if len(entries) >= n {
			return entries
		}

		select {
		case <-l.added:
		case <-deadline:
			t.Fatalf("log has %v, want %d entries", entries, n)
		}
	}
}

// loggingTipChainSource accepts block subscriptions and answers best-height
// asks, recording each ask in the order log. A nil tip answers with an
// error, which the actor drops.
type loggingTipChainSource struct {
	noopChainSourceRef

	log    *orderLog
	height fn.Option[int32]
}

// Ask records and answers best-height asks and defers everything else to
// the no-op chain source.
func (c *loggingTipChainSource) Ask(ctx context.Context,
	msg chainsource.ChainSourceMsg,
) actor.Future[chainsource.ChainSourceResp] {

	if _, ok := msg.(*chainsource.BestHeightRequest); !ok {
		return c.noopChainSourceRef.Ask(ctx, msg)
	}

	c.log.add("tip-ask")

	resp := fn.Err[chainsource.ChainSourceResp](
		errors.New("no tip in this test"),
	)
	c.height.WhenSome(func(height int32) {
		resp = fn.Ok[chainsource.ChainSourceResp](
			&chainsource.BestHeightResponse{
				Height: height,
			},
		)
	})

	promise := actor.NewPromise[chainsource.ChainSourceResp]()
	promise.Complete(resp)

	return promise.Future()
}

// newStartTipTestManager builds a Manager that spawns real VTXO actors in
// its own actor system, with a chain source that records tip asks into log.
func newStartTipTestManager(t *testing.T, log *orderLog,
	tip fn.Option[int32]) (*Manager, *MockVTXOStore) {

	t.Helper()

	system := actor.NewActorSystem()
	t.Cleanup(func() {
		require.NoError(t, system.Shutdown(context.Background()))
	})

	store := &MockVTXOStore{}
	mgr := &Manager{
		cfg: &ManagerConfig{
			Store:        store,
			ActorSystem:  system,
			ExpiryConfig: DefaultExpiryConfig(),
			ChainSource: &loggingTipChainSource{
				log:    log,
				height: tip,
			},
			RoundActor: newMockRoundActorRef(t),
		},
		actors: make(map[wire.OutPoint]VTXOActorRef),
	}

	return mgr, store
}

// logStatusWrites records every status write for op into log.
func logStatusWrites(store *MockVTXOStore, log *orderLog, op wire.OutPoint,
	statuses ...VTXOStatus) {

	for _, status := range statuses {
		store.On(
			"UpdateVTXOStatus", mock.Anything, op, status,
		).Run(func(mock.Arguments) {
			log.add("status-" + status.String())
		}).Return(nil).Once()
	}
}

// TestRecoverExitedVTXOAsksTipAfterStatusWrite is the regression for a
// recovered coin whose stored status disagreed with its actor. The manager
// spawns the actor before restoring the Live status, so a tip ask sent on
// spawn could expire the coin and persist Expired before the manager's Live
// write landed on top of it. The actor, already past Live, never wrote
// again, and the wallet counted the expired value as spendable. The tip ask
// must follow the manager's write.
func TestRecoverExitedVTXOAsksTipAfterStatusWrite(t *testing.T) {
	t.Parallel()

	vtxo := makeDescriptor(t, 30_745, 1)
	vtxo.Status = VTXOStatusUnilateralExit

	log := newOrderLog()
	mgr, store := newStartTipTestManager(
		t, log, fn.Some(vtxo.BatchExpiry),
	)
	store.On("GetVTXO", mock.Anything, vtxo.Outpoint).Return(vtxo, nil)
	logStatusWrites(
		store, log, vtxo.Outpoint, VTXOStatusLive, VTXOStatusExpired,
	)

	_, err := mgr.Receive(t.Context(), &ExitOutcomeNotification{
		Outpoint: vtxo.Outpoint,
		Outcome:  ExitOutcomeRecoverable,
		Reason:   "exit never confirmed",
	}).Unpack()
	require.NoError(t, err)

	// The expired tip makes the actor write last, so the store ends up
	// agreeing with it.
	require.Equal(t, []string{
		"status-live", "tip-ask", "status-expired",
	}, log.waitFor(t, 3))
	store.AssertExpectations(t)
}

// TestConflictExitedVTXOAsksTipAfterStatusWrite pins the same ordering for a
// conflicted exit, which also spawns an actor before persisting its status:
// a reclaim the actor starts on the tip must not have its status overwritten
// with Expired.
func TestConflictExitedVTXOAsksTipAfterStatusWrite(t *testing.T) {
	t.Parallel()

	vtxo := makeDescriptor(t, 30_745, 1)
	vtxo.Status = VTXOStatusUnilateralExit

	log := newOrderLog()
	mgr, store := newStartTipTestManager(t, log, fn.None[int32]())
	store.On("GetVTXO", mock.Anything, vtxo.Outpoint).Return(vtxo, nil)
	logStatusWrites(store, log, vtxo.Outpoint, VTXOStatusExpired)

	_, err := mgr.Receive(t.Context(), &ExitOutcomeNotification{
		Outpoint: vtxo.Outpoint,
		Outcome:  ExitOutcomeConflicted,
		Reason:   "source batch swept by the operator",
	}).Unpack()
	require.NoError(t, err)

	require.Equal(t, []string{
		"status-expired", "tip-ask",
	}, log.waitFor(t, 2))
	store.AssertExpectations(t)
}

// TestRecoverExitedVTXOAsksTipAfterFailedWrite pins that a failed status
// write still lets the actor classify its expiry: nothing will overwrite the
// actor's own write then, so there is no reason to wait for the next block.
func TestRecoverExitedVTXOAsksTipAfterFailedWrite(t *testing.T) {
	t.Parallel()

	vtxo := makeDescriptor(t, 30_745, 1)
	vtxo.Status = VTXOStatusUnilateralExit

	log := newOrderLog()
	mgr, store := newStartTipTestManager(t, log, fn.None[int32]())
	store.On("GetVTXO", mock.Anything, vtxo.Outpoint).Return(vtxo, nil)
	store.On(
		"UpdateVTXOStatus", mock.Anything, vtxo.Outpoint,
		VTXOStatusLive,
	).Run(func(mock.Arguments) {
		log.add("status-live-failed")
	}).Return(errors.New("db unavailable")).Once()

	_, err := mgr.Receive(t.Context(), &ExitOutcomeNotification{
		Outpoint: vtxo.Outpoint,
		Outcome:  ExitOutcomeRecoverable,
		Reason:   "exit never confirmed",
	}).Unpack()
	require.ErrorContains(t, err, "db unavailable")
	require.Contains(t, mgr.actors, vtxo.Outpoint)

	require.Equal(t, []string{
		"status-live-failed", "tip-ask",
	}, log.waitFor(t, 2))
	store.AssertExpectations(t)
}
