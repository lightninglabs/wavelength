package vtxo

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightninglabs/wavelength/round"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// cohortActorRef models the manager-visible parts of a VTXO actor while
// allowing deterministic slow and blocked Ask behavior.
type cohortActorRef struct {
	mu sync.Mutex

	id      string
	desc    *Descriptor
	state   VTXOState
	delay   time.Duration
	blocked bool

	leader       wire.OutPoint
	generation   uint64
	hasToken     bool
	askCount     int
	releaseCount int
	operatorKey  *btcec.PublicKey
}

// newCohortActorRef creates a deterministic manager test child.
func newCohortActorRef(desc *Descriptor, state VTXOState) *cohortActorRef {
	return &cohortActorRef{
		id:    desc.Outpoint.String(),
		desc:  desc,
		state: state,
	}
}

// ID returns the mock actor identifier.
func (c *cohortActorRef) ID() string { return c.id }

// Tell applies token-owned cohort rollback without touching unrelated pending
// reservations.
func (c *cohortActorRef) Tell(_ context.Context,
	msg actormsg.VTXOActorMsg) error {

	c.mu.Lock()
	defer c.mu.Unlock()

	release, ok := msg.(*CohortRefreshReleaseEvent)
	if !ok {
		return nil
	}
	c.releaseCount++
	if !c.hasToken || c.leader != release.LeaderOutpoint ||
		c.generation != release.Generation {
		return nil
	}

	c.state = &LiveState{VTXO: c.desc}
	c.hasToken = false

	return nil
}

// TryTell delegates to Tell because this test double does not model mailbox
// saturation.
func (c *cohortActorRef) TryTell(ctx context.Context,
	msg actormsg.VTXOActorMsg) error {

	return c.Tell(ctx, msg)
}

// Ask returns a real cohort response, optionally after a deterministic delay.
func (c *cohortActorRef) Ask(ctx context.Context,
	msg actormsg.VTXOActorMsg) actor.Future[actormsg.VTXOActorResp] {

	promise := actor.NewPromise[actormsg.VTXOActorResp]()
	c.mu.Lock()
	c.askCount++
	blocked := c.blocked
	delay := c.delay
	c.mu.Unlock()
	if blocked {
		return promise.Future()
	}

	complete := func() {
		result := c.processCohortAsk(msg)
		promise.Complete(result)
	}
	if delay == 0 {
		complete()

		return promise.Future()
	}

	go func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
			complete()

		case <-ctx.Done():
			promise.Complete(
				fn.Err[actormsg.VTXOActorResp](
					ctx.Err(),
				),
			)
		}
	}()

	return promise.Future()
}

// processCohortAsk applies live reservation or same-height pending adoption.
func (c *cohortActorRef) processCohortAsk(
	msg actormsg.VTXOActorMsg) fn.Result[actormsg.VTXOActorResp] {

	if _, release := msg.(*ForfeitReleasedEvent); release {
		c.mu.Lock()
		defer c.mu.Unlock()

		prior := c.state
		c.state = &LiveState{VTXO: c.desc}
		c.hasToken = false
		c.releaseCount++

		return fn.Ok[actormsg.VTXOActorResp](VTXOActorResponse{
			PriorState: prior,
			NewState:   c.state,
		})
	}

	event, cohortOK := msg.(*CohortRefreshEvent)
	if !cohortOK {
		return fn.Err[actormsg.VTXOActorResp](
			fmt.Errorf("unexpected cohort test event %T", msg),
		)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.operatorKey = event.OperatorKey

	prior := c.state
	triggerHeight := event.Height
	switch state := c.state.(type) {
	case *LiveState:
		c.state = &PendingForfeitState{
			VTXO:              c.desc,
			RequestedAtHeight: event.Height,
		}
		c.leader = event.LeaderOutpoint
		c.generation = event.Generation
		c.hasToken = true

	case *PendingForfeitState:
		if state.RequestedAtHeight <= 0 ||
			state.RequestedAtHeight != event.Height {
			return fn.Err[actormsg.VTXOActorResp](
				fmt.Errorf("pending reservation is not " +
					"same-height automatic"),
			)
		}
		triggerHeight = state.RequestedAtHeight

	default:
		return fn.Err[actormsg.VTXOActorResp](
			fmt.Errorf("cohort test actor is %T", c.state),
		)
	}

	return fn.Ok[actormsg.VTXOActorResp](VTXOActorResponse{
		PriorState: prior,
		NewState:   c.state,
		RoundRequest: &round.RefreshVTXORequest{
			VTXOOutpoint:  c.desc.Outpoint,
			Amount:        int64(c.desc.Amount),
			Automatic:     true,
			BatchExpiry:   c.desc.BatchExpiry,
			TriggerHeight: triggerHeight,
			OperatorKey:   event.OperatorKey,
		},
	})
}

// keySnapshot returns the operator key observed by the latest cohort Ask.
func (c *cohortActorRef) keySnapshot() *btcec.PublicKey {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.operatorKey
}

// snapshot returns child state and counters under the mock lock.
func (c *cohortActorRef) snapshot() (VTXOState, int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.state, c.askCount, c.releaseCount
}

// deterministicCohortDescriptor creates same-hash outpoints so index order is
// the canonical ordering asserted by the tests.
func deterministicCohortDescriptor(t *testing.T, index uint32,
	batchExpiry int32) *Descriptor {

	t.Helper()
	desc := makeDescriptor(t, 10_000, index)
	desc.Outpoint.Hash = [32]byte{}
	desc.Outpoint.Index = index
	desc.BatchExpiry = batchExpiry
	desc.CreatedHeight = 1
	desc.Status = VTXOStatusLive

	return desc
}

// autoRefreshLeaderRequest builds the manager relay under test.
func autoRefreshLeaderRequest(desc *Descriptor,
	height int32) *round.RefreshVTXORequest {

	return &round.RefreshVTXORequest{
		VTXOOutpoint:  desc.Outpoint,
		Amount:        int64(desc.Amount),
		Automatic:     true,
		BatchExpiry:   desc.BatchExpiry,
		TriggerHeight: height,
		ExpandCohort:  true,
	}
}

// expectCohortListings configures the two bounded discovery projections.
func expectCohortListings(store *MockVTXOStore, live, pending []*Descriptor) {
	store.On(
		"ListVTXOsByStatus", mock.Anything, VTXOStatusLive,
	).Return(live, nil).Once()
	store.On(
		"ListVTXOsByStatus", mock.Anything, VTXOStatusPendingForfeit,
	).Return(pending, nil).Once()
}

// TestManagerBuildsAtomicAutoRefreshCohort verifies same-expiry selection,
// reservation/free-window exclusions, canonical ordering, and one round Tell.
func TestManagerBuildsAtomicAutoRefreshCohort(t *testing.T) {
	t.Parallel()

	const (
		batchExpiry = int32(1_000)
		height      = int32(800)
	)
	leader := deterministicCohortDescriptor(t, 10, batchExpiry)
	eligible := deterministicCohortDescriptor(t, 9, batchExpiry)
	reserved := deterministicCohortDescriptor(t, 8, batchExpiry)
	waitsForFree := deterministicCohortDescriptor(t, 7, batchExpiry)
	different := deterministicCohortDescriptor(t, 6, batchExpiry+1)
	leader.RelativeExpiry = 100
	eligible.RelativeExpiry = 100
	reserved.RelativeExpiry = 100
	waitsForFree.RelativeExpiry = 10

	store := &MockVTXOStore{}
	expectCohortListings(store, []*Descriptor{
		waitsForFree, different, reserved, eligible, leader,
	}, nil)
	roundActor := newMockRoundActorRef(t)
	expiryCfg := &ExpiryConfig{
		RefreshThresholdBlocks:  200,
		CriticalThresholdBlocks: 30,
		MinRefreshBuffer:        50,
		FreeRefreshWindow: func() uint32 {
			return 120
		},
	}
	mgr := NewManager(&ManagerConfig{
		Store:        store,
		RoundActor:   roundActor,
		ExpiryConfig: expiryCfg,
	})
	mgr.reserved[reserved.Outpoint] = 1
	children := map[*Descriptor]*cohortActorRef{}
	for _, desc := range []*Descriptor{
		leader, eligible, reserved, waitsForFree, different,
	} {
		ref := newCohortActorRef(desc, &LiveState{VTXO: desc})
		children[desc] = ref
		mgr.actors[desc.Outpoint] = ref
	}

	_, operatorKey := generateTestKeyPair(t)
	leaderRequest := autoRefreshLeaderRequest(leader, height)
	leaderRequest.OperatorKey = operatorKey
	_, err := mgr.Receive(t.Context(), &RelayToRoundMsg{
		Payload: leaderRequest,
	}).Unpack()
	require.NoError(t, err)

	messages := roundActor.getMessages()
	require.Len(t, messages, 1)
	cohort, ok := messages[0].(*round.RefreshVTXOCohortRequest)
	require.True(t, ok)
	require.Len(t, cohort.Requests, 2)
	require.Equal(t, uint32(9), cohort.Requests[0].VTXOOutpoint.Index)
	require.Equal(t, uint32(10), cohort.Requests[1].VTXOOutpoint.Index)

	state, _, _ := children[eligible].snapshot()
	require.IsType(t, &PendingForfeitState{}, state)
	require.True(
		t,
		xOnlyEqual(
			operatorKey, children[eligible].keySnapshot(),
		),
	)
	for _, excluded := range []*Descriptor{
		reserved, waitsForFree, different,
	} {
		state, _, _ = children[excluded].snapshot()
		require.IsType(t, &LiveState{}, state)
	}
	store.AssertExpectations(t)
}

// TestManagerAutoRefreshRelayFailureReleasesWholeCohort verifies a failed
// round-actor handoff releases both manager-reserved siblings and the leader.
func TestManagerAutoRefreshRelayFailureReleasesWholeCohort(t *testing.T) {
	t.Parallel()

	const (
		batchExpiry = int32(1_000)
		height      = int32(800)
	)
	leader := deterministicCohortDescriptor(t, 0, batchExpiry)
	sibling := deterministicCohortDescriptor(t, 1, batchExpiry)
	leaderRef := newCohortActorRef(leader, &PendingForfeitState{
		VTXO:              leader,
		RequestedAtHeight: height,
	})
	siblingRef := newCohortActorRef(
		sibling, &LiveState{VTXO: sibling},
	)

	store := &MockVTXOStore{}
	expectCohortListings(store, []*Descriptor{leader, sibling}, nil)
	roundActor := newMockRoundActorRef(t)
	roundActor.tellErr = fmt.Errorf("round actor unavailable")
	mgr := NewManager(&ManagerConfig{
		Store:      store,
		RoundActor: roundActor,
	})
	mgr.actors[leader.Outpoint] = leaderRef
	mgr.actors[sibling.Outpoint] = siblingRef

	_, err := mgr.Receive(t.Context(), &RelayToRoundMsg{
		Payload: autoRefreshLeaderRequest(leader, height),
	}).Unpack()
	require.ErrorContains(t, err, "round actor unavailable")
	require.Empty(t, roundActor.getMessages())

	for _, ref := range []*cohortActorRef{leaderRef, siblingRef} {
		state, _, releases := ref.snapshot()
		require.IsType(t, &LiveState{}, state)
		require.Equal(t, 1, releases)
	}
	store.AssertExpectations(t)
}

// TestManagerAutoRefreshCohortCapIsDeterministic verifies overflow stays live
// and DB iteration order cannot decide which 31 siblings join the leader.
func TestManagerAutoRefreshCohortCapIsDeterministic(t *testing.T) {
	t.Parallel()

	const batchExpiry = int32(1_000)
	leader := deterministicCohortDescriptor(t, 0, batchExpiry)
	descriptors := []*Descriptor{leader}
	mgrActors := make(map[uint32]*cohortActorRef)
	for index := uint32(1); index <= 40; index++ {
		desc := deterministicCohortDescriptor(t, index, batchExpiry)
		descriptors = append(descriptors, desc)
		mgrActors[index] = newCohortActorRef(
			desc, &LiveState{
				VTXO: desc,
			},
		)
	}

	// Reverse the store order to prove canonical selection is independent
	// of iteration order.
	live := make([]*Descriptor, 0, len(descriptors))
	for i := len(descriptors) - 1; i >= 0; i-- {
		live = append(live, descriptors[i])
	}
	store := &MockVTXOStore{}
	expectCohortListings(store, live, nil)
	roundActor := newMockRoundActorRef(t)
	mgr := NewManager(&ManagerConfig{
		Store:      store,
		RoundActor: roundActor,
	})
	mgr.actors[leader.Outpoint] = newCohortActorRef(
		leader, &PendingForfeitState{
			VTXO: leader, RequestedAtHeight: 800,
		},
	)
	for index, ref := range mgrActors {
		mgr.actors[wire.OutPoint{Index: index}] = ref
		mgr.actors[ref.desc.Outpoint] = ref
	}

	_, err := mgr.Receive(t.Context(), &RelayToRoundMsg{
		Payload: autoRefreshLeaderRequest(leader, 800),
	}).Unpack()
	require.NoError(t, err)

	messages := roundActor.getMessages()
	require.Len(t, messages, 1)
	cohort, ok := messages[0].(*round.RefreshVTXOCohortRequest)
	require.True(t, ok)
	require.Len(t, cohort.Requests, maxAutoRefreshCohortSize)
	for index, request := range cohort.Requests {
		require.Equal(t, uint32(index), request.VTXOOutpoint.Index)
	}
	for index := uint32(1); index <= 40; index++ {
		state, _, _ := mgrActors[index].snapshot()
		if index < maxAutoRefreshCohortSize {
			require.IsType(t, &PendingForfeitState{}, state)
		} else {
			require.IsType(t, &LiveState{}, state)
		}
	}
	store.AssertExpectations(t)
}

// TestManagerAutoRefreshCohortHasOneTotalDeadline verifies one blocked child
// cannot multiply the per-child timeout or prevent prompt leader relay.
func TestManagerAutoRefreshCohortHasOneTotalDeadline(t *testing.T) {
	t.Parallel()

	const batchExpiry = int32(1_000)
	leader := deterministicCohortDescriptor(t, 0, batchExpiry)
	blockedDesc := deterministicCohortDescriptor(t, 1, batchExpiry)
	laterDesc := deterministicCohortDescriptor(t, 2, batchExpiry)
	blocked := newCohortActorRef(blockedDesc, &LiveState{VTXO: blockedDesc})
	blocked.blocked = true
	later := newCohortActorRef(laterDesc, &LiveState{VTXO: laterDesc})

	store := &MockVTXOStore{}
	expectCohortListings(store, []*Descriptor{
		leader, laterDesc, blockedDesc,
	}, nil)
	roundActor := newMockRoundActorRef(t)
	mgr := NewManager(&ManagerConfig{
		Store:                      store,
		RoundActor:                 roundActor,
		ForfeitVTXOActorAskTimeout: 40 * time.Millisecond,
	})
	mgr.actors[blockedDesc.Outpoint] = blocked
	mgr.actors[laterDesc.Outpoint] = later

	started := time.Now()
	_, err := mgr.Receive(t.Context(), &RelayToRoundMsg{
		Payload: autoRefreshLeaderRequest(leader, 800),
	}).Unpack()
	require.NoError(t, err)
	require.Less(t, time.Since(started), 500*time.Millisecond)

	messages := roundActor.getMessages()
	require.Len(t, messages, 1)
	cohort, ok := messages[0].(*round.RefreshVTXOCohortRequest)
	require.True(t, ok)
	require.Len(t, cohort.Requests, 1)
	_, _, releases := blocked.snapshot()
	require.Equal(t, 1, releases)
	_, asks, _ := later.snapshot()
	require.Zero(t, asks)
	store.AssertExpectations(t)
}

// TestManagerWaitsForDiscoveryBeforeAtomicRoundHandoff verifies a sibling Ask
// slower than the old 500ms quiet timer cannot expose a partial registration.
func TestManagerWaitsForDiscoveryBeforeAtomicRoundHandoff(t *testing.T) {
	t.Parallel()

	const batchExpiry = int32(1_000)
	leader := deterministicCohortDescriptor(t, 0, batchExpiry)
	fastDesc := deterministicCohortDescriptor(t, 1, batchExpiry)
	slowDesc := deterministicCohortDescriptor(t, 2, batchExpiry)
	fast := newCohortActorRef(fastDesc, &LiveState{VTXO: fastDesc})
	slow := newCohortActorRef(slowDesc, &LiveState{VTXO: slowDesc})
	slow.delay = 650 * time.Millisecond

	store := &MockVTXOStore{}
	expectCohortListings(store, []*Descriptor{
		leader, slowDesc, fastDesc,
	}, nil)
	roundActor := newMockRoundActorRef(t)
	mgr := NewManager(&ManagerConfig{
		Store:                      store,
		RoundActor:                 roundActor,
		ForfeitVTXOActorAskTimeout: 2 * time.Second,
	})
	mgr.actors[fastDesc.Outpoint] = fast
	mgr.actors[slowDesc.Outpoint] = slow

	done := make(chan error, 1)
	go func() {
		_, err := mgr.Receive(t.Context(), &RelayToRoundMsg{
			Payload: autoRefreshLeaderRequest(leader, 800),
		}).Unpack()
		done <- err
	}()

	select {
	case err := <-done:
		require.Failf(
			t, "coordinator returned before slow sibling", "erro"+
				"r: %v", err,
		)

	case <-time.After(550 * time.Millisecond):
		require.Empty(t, roundActor.getMessages())
	}

	select {
	case err := <-done:
		require.NoError(t, err)

	case <-time.After(2 * time.Second):
		t.Fatal("coordinator did not finish within aggregate deadline")
	}

	messages := roundActor.getMessages()
	require.Len(t, messages, 1)
	cohort, ok := messages[0].(*round.RefreshVTXOCohortRequest)
	require.True(t, ok)
	require.Len(t, cohort.Requests, 3)
	store.AssertExpectations(t)
}

// TestManagerAdoptsSameHeightPendingLeader verifies multiple same-block
// threshold relays collapse into one atomic handoff and the later queued relay
// is consumed without a second round message.
func TestManagerAdoptsSameHeightPendingLeader(t *testing.T) {
	t.Parallel()

	const (
		batchExpiry = int32(1_000)
		height      = int32(800)
	)
	leader := deterministicCohortDescriptor(t, 0, batchExpiry)
	sibling := deterministicCohortDescriptor(t, 1, batchExpiry)
	sibling.Status = VTXOStatusPendingForfeit
	siblingRef := newCohortActorRef(sibling, &PendingForfeitState{
		VTXO:              sibling,
		RequestedAtHeight: height,
	})

	store := &MockVTXOStore{}
	expectCohortListings(store, nil, []*Descriptor{leader, sibling})
	roundActor := newMockRoundActorRef(t)
	mgr := NewManager(&ManagerConfig{
		Store:      store,
		RoundActor: roundActor,
	})
	mgr.actors[sibling.Outpoint] = siblingRef

	_, err := mgr.Receive(t.Context(), &RelayToRoundMsg{
		Payload: autoRefreshLeaderRequest(leader, height),
	}).Unpack()
	require.NoError(t, err)
	messages := roundActor.getMessages()
	require.Len(t, messages, 1)
	cohort, ok := messages[0].(*round.RefreshVTXOCohortRequest)
	require.True(t, ok)
	require.Len(t, cohort.Requests, 2)

	_, err = mgr.Receive(t.Context(), &RelayToRoundMsg{
		Payload: autoRefreshLeaderRequest(sibling, height),
	}).Unpack()
	require.NoError(t, err)
	require.Len(t, roundActor.getMessages(), 1)
	store.AssertExpectations(t)
}

// TestManagerDoesNotAdoptOlderOrManualPending verifies pending reservations
// outside the exact same-height automatic race remain owned by their round.
func TestManagerDoesNotAdoptOlderOrManualPending(t *testing.T) {
	t.Parallel()

	const (
		batchExpiry = int32(1_000)
		height      = int32(800)
	)
	leader := deterministicCohortDescriptor(t, 0, batchExpiry)
	manual := deterministicCohortDescriptor(t, 1, batchExpiry)
	older := deterministicCohortDescriptor(t, 2, batchExpiry)
	manual.Status = VTXOStatusPendingForfeit
	older.Status = VTXOStatusPendingForfeit
	manualRef := newCohortActorRef(manual, &PendingForfeitState{
		VTXO: manual,
	})
	olderRef := newCohortActorRef(older, &PendingForfeitState{
		VTXO:              older,
		RequestedAtHeight: height - 1,
	})

	store := &MockVTXOStore{}
	expectCohortListings(store, nil, []*Descriptor{manual, older})
	roundActor := newMockRoundActorRef(t)
	mgr := NewManager(&ManagerConfig{
		Store:      store,
		RoundActor: roundActor,
	})
	mgr.actors[manual.Outpoint] = manualRef
	mgr.actors[older.Outpoint] = olderRef

	_, err := mgr.Receive(t.Context(), &RelayToRoundMsg{
		Payload: autoRefreshLeaderRequest(leader, height),
	}).Unpack()
	require.NoError(t, err)
	messages := roundActor.getMessages()
	require.Len(t, messages, 1)
	cohort, ok := messages[0].(*round.RefreshVTXOCohortRequest)
	require.True(t, ok)
	require.Len(t, cohort.Requests, 1)
	for _, ref := range []*cohortActorRef{manualRef, olderRef} {
		state, _, releases := ref.snapshot()
		require.IsType(t, &PendingForfeitState{}, state)
		require.Equal(t, 1, releases)
	}
	store.AssertExpectations(t)
}

var _ VTXOActorRef = (*cohortActorRef)(nil)
