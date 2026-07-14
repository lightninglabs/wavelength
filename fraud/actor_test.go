package fraud

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightninglabs/wavelength/vtxo"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

const testTimeout = time.Second

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

// fakeManagerRef stands in for the VTXO manager: it records the
// ForceUnrollRequests the fraud watcher sends and replies with a
// ForceUnrollResponse. A non-nil err makes the Ask fail so the fanout
// best-effort behavior can be exercised.
type fakeManagerRef struct {
	mu       sync.Mutex
	requests []*actormsg.ForceUnrollRequest
	err      error
}

// ID returns the fake actor ID.
func (f *fakeManagerRef) ID() string {
	return "fake-vtxo-manager"
}

// Tell is unused by these tests.
func (f *fakeManagerRef) Tell(context.Context, vtxo.ManagerMsg) error {
	return nil
}

// Ask records force-unroll requests.
func (f *fakeManagerRef) Ask(_ context.Context,
	msg vtxo.ManagerMsg) actor.Future[vtxo.ManagerResp] {

	promise := actor.NewPromise[vtxo.ManagerResp]()
	req, ok := msg.(*actormsg.ForceUnrollRequest)
	if !ok {
		promise.Complete(
			fn.Err[vtxo.ManagerResp](
				fmt.Errorf("unexpected manager msg %T", msg),
			),
		)

		return promise.Future()
	}

	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	if f.err != nil {
		promise.Complete(fn.Err[vtxo.ManagerResp](f.err))

		return promise.Future()
	}

	promise.Complete(
		fn.Ok[vtxo.ManagerResp](
			&actormsg.ForceUnrollResponse{
				Accepted: true,
			},
		),
	)

	return promise.Future()
}

func (f *fakeManagerRef) lastRequest(
	t *testing.T) *actormsg.ForceUnrollRequest {

	t.Helper()

	f.mu.Lock()
	defer f.mu.Unlock()

	require.NotEmpty(t, f.requests)

	return f.requests[len(f.requests)-1]
}

// requestCount returns the number of recorded force-unroll requests.
func (f *fakeManagerRef) requestCount() int {
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
	managerRef := &fakeManagerRef{}
	watcher := NewWatcherActor(WatcherConfig{
		ChainSource:    chainRef,
		VTXOManagerRef: managerRef,
		Log:            fn.None[btclog.Logger](),
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
		return managerRef.requestCount() == 1
	}, testTimeout, 10*time.Millisecond)

	req := managerRef.lastRequest(t)
	require.Equal(t, target, req.Outpoint)
	require.Equal(t, actormsg.UnrollTriggerFraudSpend, req.Trigger)

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
		ChainSource:    chainRef,
		VTXOManagerRef: &fakeManagerRef{},
		Log:            fn.None[btclog.Logger](),
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
		ChainSource:    chainRef,
		VTXOManagerRef: &fakeManagerRef{},
		Log:            fn.None[btclog.Logger](),
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
		ChainSource:    chainRef,
		VTXOManagerRef: &fakeManagerRef{},
		Log:            fn.None[btclog.Logger](),
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
	managerRef := &fakeManagerRef{err: fmt.Errorf("admission failed")}
	chainRef := &fakeChainSourceRef{}
	watcher := NewWatcherActor(WatcherConfig{
		ChainSource:    chainRef,
		VTXOManagerRef: managerRef,
		Log:            fn.None[btclog.Logger](),
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
	require.Equal(t, 2, managerRef.requestCount())
}
