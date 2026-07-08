package fraud

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/chainsource"
	"github.com/lightninglabs/darepo-client/unroll"
	"github.com/lightninglabs/darepo-client/vtxo"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

const testTimeout = time.Second

// The fraud watcher is intentionally a one-way alarm: it triggers
// unroll on every observed ancestor spend and does not subscribe to
// the chainsource reorg-aware lifecycle. The reorg-safety roadmap's
// third invariant — "unroll reconciliation resumes safely" — is owned
// by the unroll package and exercised there
// (see unroll/reorg_safety_test.go::TestOfflineReorgRestartReconciles
// BeforeSideEffects and the related restart-reconcile tests). The
// fraud-side tests in this file pin only the parts of the invariant
// the fraud watcher itself owns: spend watch is not reorg-aware,
// triggering the unroll does not unregister the watch, repeated
// observations are forwarded, and benign edge cases (unknown
// outpoint, shutdown) do not falsely raise a fraud alarm.

// spendRef aliases the chainsource spend notification target.
type spendRef = actor.TellOnlyRef[chainsource.SpendEvent]

type fakeChainSourceRef struct {
	mu          sync.Mutex
	spendReqs   []*chainsource.RegisterSpendRequest
	spendRefs   map[wire.OutPoint]spendRef
	unregisters []wire.OutPoint
}

// ID returns the fake actor ID.
func (f *fakeChainSourceRef) ID() string {
	return "fake-chain"
}

// Tell is unused by these tests.
func (f *fakeChainSourceRef) Tell(context.Context,
	chainsource.ChainSourceMsg) error {

	return nil
}

// Ask records spend registration requests.
func (f *fakeChainSourceRef) Ask(_ context.Context,
	msg chainsource.ChainSourceMsg,
) actor.Future[chainsource.ChainSourceResp] {

	promise := actor.NewPromise[chainsource.ChainSourceResp]()
	switch msg := msg.(type) {
	case *chainsource.RegisterSpendRequest:
		if msg.Outpoint == nil {
			promise.Complete(
				fn.Err[chainsource.ChainSourceResp](
					fmt.Errorf("outpoint required"),
				),
			)

			return promise.Future()
		}

		f.mu.Lock()
		if f.spendRefs == nil {
			f.spendRefs = make(map[wire.OutPoint]spendRef)
		}
		f.spendReqs = append(f.spendReqs, msg)
		f.spendRefs[*msg.Outpoint] = msg.NotifyActor.UnwrapOr(nil)
		f.mu.Unlock()
		promise.Complete(
			fn.Ok[chainsource.ChainSourceResp](
				&chainsource.RegisterSpendResponse{},
			),
		)

	case *chainsource.UnregisterSpendRequest:
		if msg.Outpoint != nil {
			f.mu.Lock()
			f.unregisters = append(f.unregisters, *msg.Outpoint)
			f.mu.Unlock()
		}
		promise.Complete(
			fn.Ok[chainsource.ChainSourceResp](
				&chainsource.UnregisterSpendResponse{},
			),
		)

	default:
		promise.Complete(
			fn.Err[chainsource.ChainSourceResp](
				fmt.Errorf("unexpected chainsource msg %T",
					msg),
			),
		)
	}

	return promise.Future()
}

func (f *fakeChainSourceRef) spendWatchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.spendReqs)
}

func (f *fakeChainSourceRef) emitSpend(t *testing.T, outpoint wire.OutPoint) {
	t.Helper()

	f.mu.Lock()
	ref := f.spendRefs[outpoint]
	f.mu.Unlock()

	require.NotNil(t, ref)
	require.NoError(
		t,
		ref.Tell(
			t.Context(), chainsource.SpendEvent{
				Outpoint:       outpoint,
				SpendingTxid:   testInput(99).Hash,
				SpendingHeight: 33,
			},
		),
	)
}

type fakeUnrollRef struct {
	mu       sync.Mutex
	requests []*unroll.EnsureUnrollRequest
	err      error
}

// ID returns the fake actor ID.
func (f *fakeUnrollRef) ID() string {
	return "fake-unroll"
}

// Tell is unused by these tests.
func (f *fakeUnrollRef) Tell(context.Context, unroll.RegistryMsg) error {
	return nil
}

// Ask records ensure-unroll requests.
func (f *fakeUnrollRef) Ask(_ context.Context,
	msg unroll.RegistryMsg) actor.Future[unroll.RegistryResp] {

	promise := actor.NewPromise[unroll.RegistryResp]()
	req, ok := msg.(*unroll.EnsureUnrollRequest)
	if !ok {
		promise.Complete(
			fn.Err[unroll.RegistryResp](
				fmt.Errorf("unexpected unroll msg %T", msg),
			),
		)

		return promise.Future()
	}

	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	if f.err != nil {
		promise.Complete(fn.Err[unroll.RegistryResp](f.err))

		return promise.Future()
	}

	promise.Complete(
		fn.Ok[unroll.RegistryResp](
			&unroll.EnsureUnrollResp{
				ActorID: "child",
				Created: true,
			},
		),
	)

	return promise.Future()
}

func (f *fakeUnrollRef) lastRequest(t *testing.T) *unroll.EnsureUnrollRequest {
	t.Helper()

	f.mu.Lock()
	defer f.mu.Unlock()

	require.NotEmpty(t, f.requests)

	return f.requests[len(f.requests)-1]
}

// requestCount returns the number of recorded ensure-unroll requests.
func (f *fakeUnrollRef) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.requests)
}

// TestWatcherTriggersUnrollOnAncestorSpend verifies the passive watcher calls
// into unroll only after a watched ancestor materializes.
func TestWatcherTriggersUnrollOnAncestorSpend(t *testing.T) {
	treePath, source := testLeafTree(t, 1)
	target := testInput(2)
	desc := testDescriptor(target, treePath)

	chainRef := &fakeChainSourceRef{}
	unrollRef := &fakeUnrollRef{}
	watcher := NewWatcherActor(WatcherConfig{
		ChainSource: chainRef,
		UnrollRef:   unrollRef,
		Log:         fn.None[btclog.Logger](),
	})
	t.Cleanup(watcher.Stop)

	resp, err := watcher.Ref().Ask(t.Context(), &TrackVTXOsRequest{
		VTXOs: []*vtxo.Descriptor{desc},
	}).Await(t.Context()).Unpack()
	require.NoError(t, err)
	trackResp, ok := resp.(*TrackVTXOsResp)
	require.True(t, ok)
	require.Equal(t, 1, trackResp.Tracked)
	require.Equal(t, 2, chainRef.spendWatchCount())

	chainRef.emitSpend(t, source)

	require.Eventually(t, func() bool {
		return unrollRef.requestCount() == 1
	}, testTimeout, 10*time.Millisecond)

	req := unrollRef.lastRequest(t)
	require.Equal(t, target, req.Outpoint)
	require.Equal(t, unroll.TriggerFraudSpend, req.Trigger)

	untrackResp, err := watcher.Ref().Ask(
		t.Context(), &UntrackRequest{TargetOutpoint: target},
	).Await(t.Context()).Unpack()
	require.NoError(t, err)
	typedUntrack, ok := untrackResp.(*UntrackResp)
	require.True(t, ok)
	require.True(t, typedUntrack.Removed)

	chainRef.mu.Lock()
	require.Len(t, chainRef.unregisters, 2)
	chainRef.mu.Unlock()
}

// TestWatcherTracksOnlyLiveOORVTXOs verifies admission keeps passive fraud
// watches limited to live out-of-round VTXOs.
func TestWatcherTracksOnlyLiveOORVTXOs(t *testing.T) {
	treePath, _ := testLeafTree(t, 10)
	liveOOR := testDescriptor(testInput(20), treePath)

	liveRound := testDescriptor(testInput(21), treePath)
	liveRound.ChainDepth = 0

	spentOOR := testDescriptor(testInput(22), treePath)
	spentOOR.Status = vtxo.VTXOStatusSpent

	chainRef := &fakeChainSourceRef{}
	watcher := NewWatcherActor(WatcherConfig{
		ChainSource: chainRef,
		UnrollRef:   &fakeUnrollRef{},
		Log:         fn.None[btclog.Logger](),
	})
	t.Cleanup(watcher.Stop)

	resp, err := watcher.Ref().Ask(t.Context(), &TrackVTXOsRequest{
		VTXOs: []*vtxo.Descriptor{
			liveRound,
			spentOOR,
			liveOOR,
			nil,
		},
	}).Await(t.Context()).Unpack()
	require.NoError(t, err)

	trackResp, ok := resp.(*TrackVTXOsResp)
	require.True(t, ok)
	require.Equal(t, 1, trackResp.Tracked)
	require.Equal(t, 2, chainRef.spendWatchCount())
}

// TestWatcherRefcountsSharedWatchOutpoints verifies shared ancestry produces
// one chainsource watch per outpoint and unregisters only after all targets
// release interest.
func TestWatcherRefcountsSharedWatchOutpoints(t *testing.T) {
	treePath, _ := testLeafTree(t, 30)
	targetOne := testInput(31)
	targetTwo := testInput(32)

	chainRef := &fakeChainSourceRef{}
	watcher := NewWatcherActor(WatcherConfig{
		ChainSource: chainRef,
		UnrollRef:   &fakeUnrollRef{},
		Log:         fn.None[btclog.Logger](),
	})
	t.Cleanup(watcher.Stop)

	resp, err := watcher.Ref().Ask(t.Context(), &TrackVTXOsRequest{
		VTXOs: []*vtxo.Descriptor{
			testDescriptor(targetOne, treePath),
			testDescriptor(targetTwo, treePath),
		},
	}).Await(t.Context()).Unpack()
	require.NoError(t, err)

	trackResp, ok := resp.(*TrackVTXOsResp)
	require.True(t, ok)
	require.Equal(t, 2, trackResp.Tracked)
	require.Equal(t, 2, chainRef.spendWatchCount())

	_, err = watcher.Ref().Ask(
		t.Context(), &UntrackRequest{TargetOutpoint: targetOne},
	).Await(t.Context()).Unpack()
	require.NoError(t, err)
	chainRef.mu.Lock()
	require.Empty(t, chainRef.unregisters)
	chainRef.mu.Unlock()

	_, err = watcher.Ref().Ask(
		t.Context(), &UntrackRequest{TargetOutpoint: targetTwo},
	).Await(t.Context()).Unpack()
	require.NoError(t, err)
	chainRef.mu.Lock()
	require.Len(t, chainRef.unregisters, 2)
	chainRef.mu.Unlock()
}

// TestWatcherBestEffortTrackKeepsValidDescriptors verifies one malformed
// descriptor does not roll back unrelated valid watch registrations.
func TestWatcherBestEffortTrackKeepsValidDescriptors(t *testing.T) {
	treePath, _ := testLeafTree(t, 40)
	good := testDescriptor(testInput(41), treePath)
	bad := testDescriptor(testInput(42), treePath)
	bad.Ancestry[0].TreePath = nil

	chainRef := &fakeChainSourceRef{}
	watcher := NewWatcherActor(WatcherConfig{
		ChainSource: chainRef,
		UnrollRef:   &fakeUnrollRef{},
		Log:         fn.None[btclog.Logger](),
	})
	t.Cleanup(watcher.Stop)

	_, err := watcher.Ref().Ask(t.Context(), &TrackVTXOsRequest{
		VTXOs: []*vtxo.Descriptor{bad, good},
	}).Await(t.Context()).Unpack()
	require.Error(t, err)
	require.Equal(t, 2, chainRef.spendWatchCount())
}

// TestWatcherSpendFanoutBestEffort verifies one unroll admission failure does
// not prevent other targets sharing the same watched outpoint from being
// attempted.
func TestWatcherSpendFanoutBestEffort(t *testing.T) {
	treePath, source := testLeafTree(t, 50)
	unrollRef := &fakeUnrollRef{err: fmt.Errorf("admission failed")}
	chainRef := &fakeChainSourceRef{}
	watcher := NewWatcherActor(WatcherConfig{
		ChainSource: chainRef,
		UnrollRef:   unrollRef,
		Log:         fn.None[btclog.Logger](),
	})
	t.Cleanup(watcher.Stop)

	_, err := watcher.Ref().Ask(t.Context(), &TrackVTXOsRequest{
		VTXOs: []*vtxo.Descriptor{
			testDescriptor(testInput(51), treePath),
			testDescriptor(testInput(52), treePath),
		},
	}).Await(t.Context()).Unpack()
	require.NoError(t, err)

	_, err = watcher.Ref().Ask(t.Context(), &SpendObservedMsg{
		Outpoint:     source,
		SpendingTxid: testInput(53).Hash,
		Height:       33,
	}).Await(t.Context()).Unpack()
	require.Error(t, err)
	require.Equal(t, 2, unrollRef.requestCount())
}

// TestFraudWatcherSpendWatchIsNotReorgAware pins the fraud-watcher
// semantics flagged by the reorg-safety roadmap: the chainsource
// spend watch is intentionally a one-way alarm, NOT reorg-aware. A
// fraud trigger that fires on a transient (later-reorged) ancestor
// spend is acceptable because the unroll the trigger spawns is itself
// idempotent and reorg-safe, and the unroll's restart reconciliation
// path will land the recovery on the canonical chain regardless of
// the trigger's chain history. Wiring NotifyReorged / NotifyDone here
// would only let a transient ancestor-spend reorg unwind the fraud
// alarm, which we explicitly do not want.
//
// This test exists so that "fraud watcher must not be reversible"
// stays a documented, enforceable invariant rather than an
// undocumented assumption.
func TestFraudWatcherSpendWatchIsNotReorgAware(t *testing.T) {
	treePath, _ := testLeafTree(t, 60)
	target := testInput(61)
	desc := testDescriptor(target, treePath)

	chainRef := &fakeChainSourceRef{}
	unrollRef := &fakeUnrollRef{}
	watcher := NewWatcherActor(WatcherConfig{
		ChainSource: chainRef,
		UnrollRef:   unrollRef,
		Log:         fn.None[btclog.Logger](),
	})
	t.Cleanup(watcher.Stop)

	_, err := watcher.Ref().Ask(t.Context(), &TrackVTXOsRequest{
		VTXOs: []*vtxo.Descriptor{desc},
	}).Await(t.Context()).Unpack()
	require.NoError(t, err)

	chainRef.mu.Lock()
	defer chainRef.mu.Unlock()
	require.NotEmpty(t, chainRef.spendReqs)

	for i, req := range chainRef.spendReqs {
		require.True(
			t, req.NotifyActor.IsSome(),
			"req[%d] must always wire NotifyActor", i,
		)
		require.True(
			t, req.NotifyReorged.IsNone(),
			"req[%d] must NOT wire NotifyReorged — fraud watch "+
				"is intentionally one-way; unroll's own "+
				"reorg-aware lifecycle handles "+
				"canonical-chain reconciliation downstream", i,
		)
		require.True(
			t, req.NotifyDone.IsNone(),
			"req[%d] must NOT wire NotifyDone for the same reason",
			i,
		)
	}
}

// TestFraudWatcherSpendTriggerDoesNotUnregisterWatch verifies that
// observing a watched ancestor spend (and firing the fraud unroll
// trigger) does NOT tear down the chainsource spend watch for that
// outpoint. The watch stays armed so a subsequent re-confirmation /
// re-spend of the same ancestor would re-trigger; only an explicit
// UntrackRequest on the dependent target tears the watch down (see
// TestWatcherTriggersUnrollOnAncestorSpend's untrack assertion).
//
// This is the structural complement to
// TestFraudWatcherSpendWatchIsNotReorgAware: the watch is one-way
// at the wiring level AND retained at the lifecycle level. Together
// they prove that a transient ancestor-spend reorg cannot terminally
// fail the fraud defenses — the watch is still listening and will
// re-fire the trigger on the next observation.
func TestFraudWatcherSpendTriggerDoesNotUnregisterWatch(t *testing.T) {
	treePath, source := testLeafTree(t, 70)
	target := testInput(71)
	desc := testDescriptor(target, treePath)

	chainRef := &fakeChainSourceRef{}
	unrollRef := &fakeUnrollRef{}
	watcher := NewWatcherActor(WatcherConfig{
		ChainSource: chainRef,
		UnrollRef:   unrollRef,
		Log:         fn.None[btclog.Logger](),
	})
	t.Cleanup(watcher.Stop)

	_, err := watcher.Ref().Ask(t.Context(), &TrackVTXOsRequest{
		VTXOs: []*vtxo.Descriptor{desc},
	}).Await(t.Context()).Unpack()
	require.NoError(t, err)

	chainRef.emitSpend(t, source)

	require.Eventually(t, func() bool {
		return unrollRef.requestCount() == 1
	}, testTimeout, 10*time.Millisecond)

	// Watch must still be registered after the trigger fires.
	chainRef.mu.Lock()
	require.Empty(
		t, chainRef.unregisters, "firing the unroll trigger must "+
			"not unregister the watch; the watch has to stay "+
			"armed so a subsequent observation of the same "+
			"ancestor still re-triggers",
	)
	chainRef.mu.Unlock()
}

// TestFraudWatcherSpendTriggerIsIdempotentOnRepeatedObservations
// verifies that observing the same ancestor spend twice fires the
// unroll trigger twice. The fraud watcher does not need to dedupe
// itself; downstream idempotency in the unroll registry (tested in
// `unroll/`) absorbs the duplicate without spawning a second child
// actor. This test only pins the FRAUD-side behavior — the fake
// unroll ref here records both calls without enforcing the registry's
// dedup contract, which is the right scope: that contract is owned
// by the unroll package.
//
// The scenario this anticipates is a backend that re-delivers a
// `SpendEvent` for the same outpoint after a reorg-and-reconfirm of
// the spending block. The fraud watcher's `RegisterSpendRequest`
// deliberately does not subscribe to the chainsource reorg-aware
// lifecycle (see TestFraudWatcherSpendWatchIsNotReorgAware), so this
// test simulates what would happen IF a duplicate `SpendEvent`
// reached the watcher mailbox by another route (e.g. a backend that
// natively redelivers positive events on canonical-chain change).
func TestFraudWatcherSpendTriggerIsIdempotentOnRepeatedObservations(
	t *testing.T) {

	treePath, source := testLeafTree(t, 80)
	target := testInput(81)
	desc := testDescriptor(target, treePath)

	chainRef := &fakeChainSourceRef{}
	unrollRef := &fakeUnrollRef{}
	watcher := NewWatcherActor(WatcherConfig{
		ChainSource: chainRef,
		UnrollRef:   unrollRef,
		Log:         fn.None[btclog.Logger](),
	})
	t.Cleanup(watcher.Stop)

	_, err := watcher.Ref().Ask(t.Context(), &TrackVTXOsRequest{
		VTXOs: []*vtxo.Descriptor{desc},
	}).Await(t.Context()).Unpack()
	require.NoError(t, err)

	// First observation.
	chainRef.emitSpend(t, source)
	require.Eventually(t, func() bool {
		return unrollRef.requestCount() == 1
	}, testTimeout, 10*time.Millisecond)

	// Second observation (simulates the reorg-aware path where the
	// SpendActor re-fires after a reorg-then-reconfirm of the
	// spending block — even though fraud watch itself is not wired
	// for reorgs, a real reorg-aware backend could re-deliver the
	// underlying SpendEvent on the new canonical chain).
	chainRef.emitSpend(t, source)
	require.Eventually(t, func() bool {
		return unrollRef.requestCount() == 2
	}, testTimeout, 10*time.Millisecond)

	// Both requests must target the same VTXO with the FraudSpend
	// trigger; the downstream unroll registry's idempotency makes
	// the second a no-op.
	chainRef.mu.Lock()
	require.Empty(
		t, chainRef.unregisters,
		"repeated observations must keep the watch armed",
	)
	chainRef.mu.Unlock()

	unrollRef.mu.Lock()
	for i, req := range unrollRef.requests {
		require.Equal(
			t, target, req.Outpoint, "request[%d] must target "+
				"the same outpoint", i,
		)
		require.Equal(
			t, unroll.TriggerFraudSpend, req.Trigger, "request[%"+
				"d] must use the FraudSpend trigger", i,
		)
	}
	unrollRef.mu.Unlock()
}

// TestFraudWatcherOnStopUnregistersWithoutTriggering verifies the
// shutdown path: stopping the watcher tears down every active
// chainsource spend watch but does NOT fire any unroll trigger.
// A future refactor that accidentally re-uses the spend-event path
// for shutdown cleanup would falsely trigger fraud unrolls on every
// daemon shutdown.
func TestFraudWatcherOnStopUnregistersWithoutTriggering(t *testing.T) {
	treePath, _ := testLeafTree(t, 90)
	target := testInput(91)
	desc := testDescriptor(target, treePath)

	chainRef := &fakeChainSourceRef{}
	unrollRef := &fakeUnrollRef{}
	watcher := NewWatcherActor(WatcherConfig{
		ChainSource: chainRef,
		UnrollRef:   unrollRef,
		Log:         fn.None[btclog.Logger](),
	})

	_, err := watcher.Ref().Ask(t.Context(), &TrackVTXOsRequest{
		VTXOs: []*vtxo.Descriptor{desc},
	}).Await(t.Context()).Unpack()
	require.NoError(t, err)

	require.Greater(
		t, chainRef.spendWatchCount(), 0,
		"setup: watcher must have registered at least one watch",
	)

	// Shutdown.
	watcher.Stop()

	// On shutdown the watcher should release its watches via the
	// chainsource Unregister path, but must not have triggered any
	// unroll calls.
	require.Eventually(t, func() bool {
		chainRef.mu.Lock()
		defer chainRef.mu.Unlock()

		return len(chainRef.unregisters) > 0
	}, testTimeout, 10*time.Millisecond,
		"OnStop must unregister the active spend watches")

	require.Zero(
		t, unrollRef.requestCount(),
		"OnStop must NOT fire any unroll trigger — shutdown is not "+
			"a fraud event",
	)
}

// TestFraudWatcherSpendObservedForUnknownOutpointIsBenign verifies
// that a SpendObservedMsg for an outpoint with no tracked targets
// (e.g. a late chainsource delivery for an outpoint that has since
// been untracked, or a backend that natively re-delivers a positive
// event after a reorg-and-reconfirm on a target that the watcher no
// longer cares about) returns AckResp{} without firing any unroll
// trigger. This is the load-bearing safety case for the "transient
// ancestor-spend reorg does not terminally fail anything" invariant:
// a re-delivered spend on a now-untracked outpoint must be silently
// dropped, not erroneously promoted into a fraud alarm.
func TestFraudWatcherSpendObservedForUnknownOutpointIsBenign(t *testing.T) {
	chainRef := &fakeChainSourceRef{}
	unrollRef := &fakeUnrollRef{}
	watcher := NewWatcherActor(WatcherConfig{
		ChainSource: chainRef,
		UnrollRef:   unrollRef,
		Log:         fn.None[btclog.Logger](),
	})
	t.Cleanup(watcher.Stop)

	unknownOutpoint := testInput(99)

	resp, err := watcher.Ref().Ask(
		t.Context(), &SpendObservedMsg{
			Outpoint:     unknownOutpoint,
			SpendingTxid: testInput(100).Hash,
			Height:       42,
		},
	).Await(t.Context()).Unpack()
	require.NoError(
		t, err, "SpendObservedMsg for an unknown outpoint must ack "+
			"without error — the late-delivery scenario is benign",
	)
	_, ok := resp.(*AckResp)
	require.True(
		t, ok,
		"SpendObservedMsg for an unknown outpoint must return AckResp",
	)

	require.Zero(
		t, unrollRef.requestCount(),
		"SpendObservedMsg for an unknown outpoint must NOT trigger "+
			"any unroll call",
	)
}
