package vtxo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/coinselect"
	"github.com/lightninglabs/darepo-client/lib/actormsg"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// mockVTXOActorRef is a minimal mock ActorRef for VTXO actors that supports
// both Tell and Ask. It runs events through a real FSM state to produce
// realistic accept/reject behavior.
type mockVTXOActorRef struct {
	id    string
	state VTXOState
	env   *VTXOEnvironment
}

// newMockVTXOActorRef creates a mock actor ref starting in the given state.
func newMockVTXOActorRef(id string, state VTXOState) *mockVTXOActorRef {
	return &mockVTXOActorRef{
		id:    id,
		state: state,
		env: &VTXOEnvironment{
			ExpiryConfig: DefaultExpiryConfig(),
		},
	}
}

// ID returns the mock actor ID.
func (m *mockVTXOActorRef) ID() string { return m.id }

// Tell sends a fire-and-forget message.
func (m *mockVTXOActorRef) Tell(_ context.Context,
	_ actormsg.VTXOActorMsg) error {

	return nil
}

// Ask processes the event through the real FSM state and returns a
// completed Future with the result.
func (m *mockVTXOActorRef) Ask(ctx context.Context,
	msg actormsg.VTXOActorMsg) actor.Future[actormsg.VTXOActorResp] {

	promise := actor.NewPromise[actormsg.VTXOActorResp]()

	vtxoEvent, ok := msg.(VTXOEvent)
	if !ok {
		promise.Complete(
			fn.Err[actormsg.VTXOActorResp](
				fmt.Errorf("not a VTXOEvent: %T", msg),
			),
		)

		return promise.Future()
	}

	transition, err := m.state.ProcessEvent(ctx, vtxoEvent, m.env)
	if err != nil {
		promise.Complete(fn.Err[actormsg.VTXOActorResp](err))

		return promise.Future()
	}

	// Apply the transition to update local state.
	if nextState, ok := transition.NextState.(VTXOState); ok {
		m.state = nextState
	}

	promise.Complete(
		fn.Ok[actormsg.VTXOActorResp](
			VTXOActorResponse{
				NewState: m.state,
			},
		),
	)

	return promise.Future()
}

// Compile-time check that mockVTXOActorRef implements VTXOActorRef.
var _ VTXOActorRef = (*mockVTXOActorRef)(nil)

// blockingVTXOActorRef returns asks that never complete on their own. Awaiters
// are unblocked only by their context, which lets admission tests verify that
// the manager bounds child actor asks.
type blockingVTXOActorRef struct {
	id string
}

// ID returns the mock actor ID.
func (b *blockingVTXOActorRef) ID() string { return b.id }

// Tell sends a fire-and-forget message.
func (b *blockingVTXOActorRef) Tell(_ context.Context,
	_ actormsg.VTXOActorMsg) error {

	return nil
}

// Ask returns a future that remains pending until the await context ends.
func (b *blockingVTXOActorRef) Ask(_ context.Context,
	_ actormsg.VTXOActorMsg) actor.Future[actormsg.VTXOActorResp] {

	promise := actor.NewPromise[actormsg.VTXOActorResp]()

	return promise.Future()
}

// Compile-time check that blockingVTXOActorRef implements VTXOActorRef.
var _ VTXOActorRef = (*blockingVTXOActorRef)(nil)

// newTestManager creates a Manager with mock actors for testing admission
// handlers. The store and actors map are populated from the given
// descriptors, each starting in LiveState.
func newTestManager(t *testing.T,
	descriptors []*Descriptor) (*Manager, *MockVTXOStore) {

	t.Helper()

	store := &MockVTXOStore{}
	mgr := &Manager{
		cfg: &ManagerConfig{
			Store:      store,
			RoundActor: newMockRoundActorRef(t),
		},
		actors: make(map[wire.OutPoint]VTXOActorRef),
	}

	for _, vtxo := range descriptors {
		ref := newMockVTXOActorRef(
			vtxo.Outpoint.String(),
			&LiveState{
				VTXO:              vtxo,
				LastCheckedHeight: vtxo.CreatedHeight,
			},
		)
		mgr.actors[vtxo.Outpoint] = ref
	}

	return mgr, store
}

// makeDescriptor creates a test Descriptor with the given amount.
func makeDescriptor(t *testing.T, amount btcutil.Amount,
	idx uint32) *Descriptor {

	t.Helper()

	h := newVTXOTestHarness(t)
	vtxo := h.newTestDescriptor()
	vtxo.Amount = amount
	vtxo.Outpoint.Index = idx

	return vtxo
}

// =============================================================================
// Spend selection tests
// =============================================================================

// TestSelectAndReserveSpendSuccess verifies that the manager selects and
// reserves VTXOs covering the target amount using largest-first selection.
func TestSelectAndReserveSpendSuccess(t *testing.T) {
	t.Parallel()

	vtxo1 := makeDescriptor(t, 30000, 0)
	vtxo2 := makeDescriptor(t, 50000, 1)
	vtxo3 := makeDescriptor(t, 20000, 2)

	mgr, store := newTestManager(t, []*Descriptor{
		vtxo1, vtxo2, vtxo3,
	})

	// ListVTXOsByStatus returns all live VTXOs.
	store.On(
		"ListVTXOsByStatus", t.Context(), VTXOStatusLive,
	).Return([]*Descriptor{vtxo1, vtxo2, vtxo3}, nil)

	result := mgr.Receive(t.Context(), &SelectAndReserveSpendRequest{
		TargetAmount: 40000,
	})
	resp, err := result.Unpack()
	require.NoError(t, err)

	spendResp, ok := resp.(*SelectAndReserveSpendResponse)
	require.True(t, ok)

	// Largest-first should pick vtxo2 (50000) which covers 40000.
	require.Len(t, spendResp.SelectedVTXOs, 1)
	require.Equal(t, vtxo2.Outpoint, spendResp.SelectedVTXOs[0].Outpoint)
	require.Equal(t, btcutil.Amount(50000), spendResp.TotalSelected)

	// Verify the actor is now in SpendingState.
	refAny := mgr.actors[vtxo2.Outpoint]
	require.NotNil(t, refAny)

	ref, ok := refAny.(*mockVTXOActorRef)
	require.True(t, ok, "expected *mockVTXOActorRef, got %T", refAny)

	_, ok = ref.state.(*SpendingState)
	require.True(t, ok, "expected SpendingState, got %T", ref.state)
}

// TestSelectAndReserveSpendMultipleVTXOs verifies that coin selection picks
// multiple VTXOs when no single VTXO covers the target.
func TestSelectAndReserveSpendMultipleVTXOs(t *testing.T) {
	t.Parallel()

	vtxo1 := makeDescriptor(t, 30000, 0)
	vtxo2 := makeDescriptor(t, 25000, 1)
	vtxo3 := makeDescriptor(t, 20000, 2)

	mgr, store := newTestManager(t, []*Descriptor{
		vtxo1, vtxo2, vtxo3,
	})

	store.On(
		"ListVTXOsByStatus", t.Context(), VTXOStatusLive,
	).Return([]*Descriptor{vtxo1, vtxo2, vtxo3}, nil)

	result := mgr.Receive(t.Context(), &SelectAndReserveSpendRequest{
		TargetAmount: 50000,
	})
	resp, err := result.Unpack()
	require.NoError(t, err)

	spendResp, ok := resp.(*SelectAndReserveSpendResponse)
	require.True(t, ok, "expected *SelectAndReserveSpendResponse")

	// Largest-first: vtxo1 (30000) + vtxo2 (25000) = 55000 >= 50000.
	require.Len(t, spendResp.SelectedVTXOs, 2)
	require.Equal(t, btcutil.Amount(55000), spendResp.TotalSelected)
}

// TestSelectAndReserveSpendCoalescesDustChange verifies that OOR spend
// selection keeps adding inputs when the first covering input would require a
// non-zero change output below the operator dust limit. This avoids opening an
// OOR package that the daemon later rejects for dust change.
func TestSelectAndReserveSpendCoalescesDustChange(t *testing.T) {
	t.Parallel()

	vtxo1 := makeDescriptor(t, 1500, 0)
	vtxo2 := makeDescriptor(t, 1500, 1)
	vtxo3 := makeDescriptor(t, 1000, 2)
	vtxo4 := makeDescriptor(t, 999, 3)

	mgr, store := newTestManager(t, []*Descriptor{
		vtxo1, vtxo2, vtxo3, vtxo4,
	})

	store.On(
		"ListVTXOsByStatus", t.Context(), VTXOStatusLive,
	).Return([]*Descriptor{vtxo1, vtxo2, vtxo3, vtxo4}, nil)

	result := mgr.Receive(t.Context(), &SelectAndReserveSpendRequest{
		TargetAmount:    600,
		MinChangeAmount: 1000,
	})
	resp, err := result.Unpack()
	require.NoError(t, err)

	spendResp, ok := resp.(*SelectAndReserveSpendResponse)
	require.True(t, ok, "expected *SelectAndReserveSpendResponse")

	require.Len(t, spendResp.SelectedVTXOs, 2)
	require.Equal(t, btcutil.Amount(3000), spendResp.TotalSelected)
	require.Equal(t, vtxo1.Outpoint, spendResp.SelectedVTXOs[0].Outpoint)
	require.Equal(t, vtxo2.Outpoint, spendResp.SelectedVTXOs[1].Outpoint)
}

// TestSelectAndReserveSpendAllowsExactDustlessSpend asserts that the
// min-change guard does not force callers to select extra inputs when the
// selected VTXO set exactly matches the spend amount. Exact spends have no
// change output, so the operator dust limit is irrelevant.
func TestSelectAndReserveSpendAllowsExactDustlessSpend(t *testing.T) {
	t.Parallel()

	vtxo1 := makeDescriptor(t, 600, 0)
	vtxo2 := makeDescriptor(t, 1500, 1)

	mgr, store := newTestManager(t, []*Descriptor{
		vtxo1, vtxo2,
	})

	store.On(
		"ListVTXOsByStatus", t.Context(), VTXOStatusLive,
	).Return([]*Descriptor{vtxo1, vtxo2}, nil)

	result := mgr.Receive(t.Context(), &SelectAndReserveSpendRequest{
		TargetAmount:    1500,
		MinChangeAmount: 1000,
	})
	resp, err := result.Unpack()
	require.NoError(t, err)

	spendResp, ok := resp.(*SelectAndReserveSpendResponse)
	require.True(t, ok, "expected *SelectAndReserveSpendResponse")

	require.Len(t, spendResp.SelectedVTXOs, 1)
	require.Equal(t, btcutil.Amount(1500), spendResp.TotalSelected)
	require.Equal(t, vtxo2.Outpoint, spendResp.SelectedVTXOs[0].Outpoint)
}

// TestSelectAndReserveSpendRejectsUnavoidableDustChange verifies that a
// wallet with enough value but no exact-or-dust-safe combination fails before
// reserving any VTXO. This leaves the caller with a local admission error
// instead of an OOR package rejected after partial reservation.
func TestSelectAndReserveSpendRejectsUnavoidableDustChange(t *testing.T) {
	t.Parallel()

	vtxo1 := makeDescriptor(t, 2000, 0)
	vtxo2 := makeDescriptor(t, 1000, 1)

	mgr, store := newTestManager(t, []*Descriptor{vtxo1, vtxo2})

	store.On(
		"ListVTXOsByStatus", t.Context(), VTXOStatusLive,
	).Return([]*Descriptor{vtxo1, vtxo2}, nil)

	result := mgr.Receive(t.Context(), &SelectAndReserveSpendRequest{
		TargetAmount:    600,
		MinChangeAmount: 5000,
	})
	_, err := result.Unpack()
	require.Error(t, err)
	require.Contains(
		t, err.Error(),
		"change 1400 is below minimum change amount 5000",
	)

	for _, vtxo := range []*Descriptor{vtxo1, vtxo2} {
		refAny := mgr.actors[vtxo.Outpoint]
		ref, ok := refAny.(*mockVTXOActorRef)
		require.True(
			t, ok, "expected *mockVTXOActorRef, got %T", refAny,
		)

		_, ok = ref.state.(*LiveState)
		require.True(t, ok, "expected LiveState, got %T", ref.state)
	}
}

// TestSelectAndReserveSpendInsufficientFunds verifies that selection fails
// when candidates cannot cover the target.
func TestSelectAndReserveSpendInsufficientFunds(t *testing.T) {
	t.Parallel()

	vtxo1 := makeDescriptor(t, 10000, 0)

	mgr, store := newTestManager(t, []*Descriptor{vtxo1})

	store.On(
		"ListVTXOsByStatus", t.Context(), VTXOStatusLive,
	).Return([]*Descriptor{vtxo1}, nil)
	store.On("ListLiveVTXOs", t.Context()).Return(
		[]*Descriptor{vtxo1}, nil,
	)

	result := mgr.Receive(t.Context(), &SelectAndReserveSpendRequest{
		TargetAmount: 50000,
	})
	_, err := result.Unpack()
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInsufficientSpendableFunds)
	require.NotErrorIs(t, err, ErrVTXOLiquidityLocked)
}

// TestSelectAndReserveSpendDoubleExclusion verifies that a second selection
// cannot get VTXOs already reserved by a prior selection. The first
// selection moves VTXOs to SpendingState, so the store's ListVTXOsByStatus
// (which returns only Live) excludes them.
func TestSelectAndReserveSpendDoubleExclusion(t *testing.T) {
	t.Parallel()

	vtxo1 := makeDescriptor(t, 50000, 0)
	vtxo2 := makeDescriptor(t, 30000, 1)

	mgr, store := newTestManager(t, []*Descriptor{vtxo1, vtxo2})

	// First call: both VTXOs are live.
	store.On(
		"ListVTXOsByStatus", t.Context(), VTXOStatusLive,
	).Return([]*Descriptor{vtxo1, vtxo2}, nil).Once()

	result := mgr.Receive(t.Context(), &SelectAndReserveSpendRequest{
		TargetAmount: 40000,
	})
	_, err := result.Unpack()
	require.NoError(t, err)

	// Second call: vtxo1 is now Spending, only vtxo2 remains live.
	store.On(
		"ListVTXOsByStatus", t.Context(), VTXOStatusLive,
	).Return([]*Descriptor{vtxo2}, nil).Once()
	spendingVTXO := *vtxo1
	spendingVTXO.Status = VTXOStatusSpending
	store.On("ListLiveVTXOs", t.Context()).Return(
		[]*Descriptor{nil, &spendingVTXO, vtxo2}, nil,
	).Once()

	result = mgr.Receive(t.Context(), &SelectAndReserveSpendRequest{
		TargetAmount: 40000,
	})
	_, err = result.Unpack()
	require.Error(t, err)
	require.ErrorIs(t, err, ErrVTXOLiquidityLocked)
	require.NotErrorIs(t, err, ErrInsufficientSpendableFunds)
}

// =============================================================================
// Spend release and completion tests
// =============================================================================

// TestReleaseSpend verifies that releasing a spend reservation returns the
// VTXO to LiveState.
func TestReleaseSpend(t *testing.T) {
	t.Parallel()

	vtxo1 := makeDescriptor(t, 50000, 0)
	mgr, store := newTestManager(t, []*Descriptor{vtxo1})

	// First reserve.
	store.On(
		"ListVTXOsByStatus", t.Context(), VTXOStatusLive,
	).Return([]*Descriptor{vtxo1}, nil)

	result := mgr.Receive(t.Context(), &SelectAndReserveSpendRequest{
		TargetAmount: 40000,
	})
	_, err := result.Unpack()
	require.NoError(t, err)

	// Now release.
	result = mgr.Receive(t.Context(), &ReleaseSpendRequest{
		Outpoints: []wire.OutPoint{vtxo1.Outpoint},
	})
	resp, err := result.Unpack()
	require.NoError(t, err)

	releaseResp, ok := resp.(*ReleaseSpendResponse)
	require.True(t, ok, "expected *ReleaseSpendResponse")
	require.Equal(t, 1, releaseResp.ReleasedCount)

	// Actor should be back in LiveState.
	refAny := mgr.actors[vtxo1.Outpoint]
	require.NotNil(t, refAny)

	ref, ok := refAny.(*mockVTXOActorRef)
	require.True(t, ok, "expected *mockVTXOActorRef, got %T", refAny)

	_, ok = ref.state.(*LiveState)
	require.True(t, ok, "expected LiveState, got %T", ref.state)
}

// TestCompleteSpend verifies that completing a spend transitions the VTXO
// to terminal SpentState.
func TestCompleteSpend(t *testing.T) {
	t.Parallel()

	vtxo1 := makeDescriptor(t, 50000, 0)
	mgr, store := newTestManager(t, []*Descriptor{vtxo1})

	// First reserve.
	store.On(
		"ListVTXOsByStatus", t.Context(), VTXOStatusLive,
	).Return([]*Descriptor{vtxo1}, nil)

	result := mgr.Receive(t.Context(), &SelectAndReserveSpendRequest{
		TargetAmount: 40000,
	})
	_, err := result.Unpack()
	require.NoError(t, err)

	// Now complete.
	result = mgr.Receive(t.Context(), &CompleteSpendRequest{
		Outpoints: []wire.OutPoint{vtxo1.Outpoint},
	})
	resp, err := result.Unpack()
	require.NoError(t, err)

	completeResp, ok := resp.(*CompleteSpendResponse)
	require.True(t, ok, "expected *CompleteSpendResponse")
	require.Equal(t, 1, completeResp.CompletedCount)

	// Actor should be in SpentState.
	refAny := mgr.actors[vtxo1.Outpoint]
	require.NotNil(t, refAny)

	ref, ok := refAny.(*mockVTXOActorRef)
	require.True(t, ok, "expected *mockVTXOActorRef, got %T", refAny)

	_, ok = ref.state.(*SpentState)
	require.True(t, ok, "expected SpentState, got %T", ref.state)
}

// TestCompleteSpendUsesCallerDeadline verifies that spend completion is not
// failed by the shorter forfeit admission timeout.
func TestCompleteSpendUsesCallerDeadline(t *testing.T) {
	t.Parallel()

	vtxo := makeDescriptor(t, 50000, 0)
	mgr, _ := newTestManager(t, nil)
	mgr.cfg.ForfeitVTXOActorAskTimeout = 10 * time.Millisecond
	mgr.actors[vtxo.Outpoint] = &blockingVTXOActorRef{
		id: vtxo.Outpoint.String(),
	}

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	result := mgr.Receive(ctx, &CompleteSpendRequest{
		Outpoints: []wire.OutPoint{vtxo.Outpoint},
	})
	_, err := result.Unpack()
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.GreaterOrEqual(t, time.Since(start), 75*time.Millisecond)
}

// TestCompleteSpendAlreadyPersistedSpentIsIdempotent verifies the resume case
// where a previous CompleteSpend call durably wrote VTXOStatusSpent, the VTXO
// actor was cleaned up, and the OOR actor retries MarkInputsSpent before it has
// checkpointed Completed.
func TestCompleteSpendAlreadyPersistedSpentIsIdempotent(t *testing.T) {
	t.Parallel()

	vtxo := makeDescriptor(t, 50000, 0)
	vtxo.Status = VTXOStatusSpent

	mgr, store := newTestManager(t, nil)
	store.On(
		"GetVTXO", t.Context(), vtxo.Outpoint,
	).Return(vtxo, nil).Once()

	result := mgr.Receive(t.Context(), &CompleteSpendRequest{
		Outpoints: []wire.OutPoint{vtxo.Outpoint},
	})
	resp, err := result.Unpack()
	require.NoError(t, err)

	completeResp, ok := resp.(*CompleteSpendResponse)
	require.True(t, ok, "expected *CompleteSpendResponse")
	require.Equal(t, 1, completeResp.CompletedCount)

	store.AssertExpectations(t)
}

// TestCompleteSpendMissingPersistedVTXOReturnsNoActor verifies that a missing
// persisted VTXO remains a normal unknown-outpoint error.
func TestCompleteSpendMissingPersistedVTXOReturnsNoActor(t *testing.T) {
	t.Parallel()

	unknownOP := wire.OutPoint{Index: 99}

	mgr, store := newTestManager(t, nil)
	store.On(
		"GetVTXO", t.Context(), unknownOP,
	).Return(nil, sql.ErrNoRows).Once()

	result := mgr.Receive(t.Context(), &CompleteSpendRequest{
		Outpoints: []wire.OutPoint{unknownOP},
	})
	_, err := result.Unpack()
	require.Error(t, err)
	require.Contains(t, err.Error(), "no actor for outpoint")

	store.AssertExpectations(t)
}

// TestCompleteSpendPersistedSpentCheckError verifies that transient lookup
// errors are surfaced instead of being reported as a missing actor.
func TestCompleteSpendPersistedSpentCheckError(t *testing.T) {
	t.Parallel()

	unknownOP := wire.OutPoint{Index: 99}
	storeErr := errors.New("temporary db outage")

	mgr, store := newTestManager(t, nil)
	store.On(
		"GetVTXO", t.Context(), unknownOP,
	).Return(nil, storeErr).Once()

	result := mgr.Receive(t.Context(), &CompleteSpendRequest{
		Outpoints: []wire.OutPoint{unknownOP},
	})
	_, err := result.Unpack()
	require.ErrorIs(t, err, storeErr)
	require.Contains(t, err.Error(), "load vtxo for spent check")

	store.AssertExpectations(t)
}

// =============================================================================
// Forfeit admission tests
// =============================================================================

// TestReserveForfeitSuccess verifies that the manager reserves specific
// VTXOs for cooperative consumption.
func TestReserveForfeitSuccess(t *testing.T) {
	t.Parallel()

	vtxo1 := makeDescriptor(t, 50000, 0)
	vtxo2 := makeDescriptor(t, 30000, 1)

	mgr, _ := newTestManager(t, []*Descriptor{vtxo1, vtxo2})

	result := mgr.Receive(t.Context(), &ReserveForfeitRequest{
		Outpoints: []wire.OutPoint{
			vtxo1.Outpoint, vtxo2.Outpoint,
		},
	})
	_, err := result.Unpack()
	require.NoError(t, err)

	// Both actors should be in PendingForfeitState.
	for _, vtxo := range []*Descriptor{vtxo1, vtxo2} {
		refAny := mgr.actors[vtxo.Outpoint]
		require.NotNil(t, refAny)

		ref, ok := refAny.(*mockVTXOActorRef)
		require.True(
			t, ok, "expected *mockVTXOActorRef, got %T", refAny,
		)

		_, ok = ref.state.(*PendingForfeitState)
		require.True(
			t, ok, "expected PendingForfeitState for %s, got %T",
			vtxo.Outpoint, ref.state,
		)
	}
}

// TestReserveForfeitRejectedWhenSpending verifies that forfeit reservation
// fails for a VTXO already reserved for OOR spend, and rolls back any
// VTXOs that were successfully reserved before the failure.
func TestReserveForfeitRejectedWhenSpending(t *testing.T) {
	t.Parallel()

	vtxo1 := makeDescriptor(t, 50000, 0)
	vtxo2 := makeDescriptor(t, 30000, 1)

	mgr, store := newTestManager(t, []*Descriptor{vtxo1, vtxo2})

	// Reserve vtxo2 for spend first.
	store.On(
		"ListVTXOsByStatus", t.Context(), VTXOStatusLive,
	).Return([]*Descriptor{vtxo2}, nil)

	result := mgr.Receive(t.Context(), &SelectAndReserveSpendRequest{
		TargetAmount: 20000,
	})
	_, err := result.Unpack()
	require.NoError(t, err)

	// Now try to reserve both for forfeit — vtxo1 succeeds, vtxo2
	// fails because it's Spending. vtxo1 should be rolled back.
	result = mgr.Receive(t.Context(), &ReserveForfeitRequest{
		Outpoints: []wire.OutPoint{
			vtxo1.Outpoint, vtxo2.Outpoint,
		},
	})
	_, err = result.Unpack()
	require.Error(t, err)
	require.Contains(t, err.Error(), "cannot accept pending forfeit")

	// vtxo1 should be rolled back to LiveState.
	refAny, ok := mgr.actors[vtxo1.Outpoint]
	require.True(t, ok, "actor not found for vtxo1")

	ref1, ok := refAny.(*mockVTXOActorRef)
	require.True(t, ok, "expected *mockVTXOActorRef, got %T", refAny)

	_, ok = ref1.state.(*LiveState)
	require.True(
		t, ok, "expected LiveState after rollback, got %T", ref1.state,
	)
}

// TestReserveForfeitTimeoutRollsBackReservedVTXOs verifies that a blocked
// child actor times out quickly and previously reserved VTXOs are released.
func TestReserveForfeitTimeoutRollsBackReservedVTXOs(t *testing.T) {
	t.Parallel()

	vtxo1 := makeDescriptor(t, 50000, 0)
	vtxo2 := makeDescriptor(t, 30000, 1)

	mgr, _ := newTestManager(t, []*Descriptor{vtxo1, vtxo2})
	mgr.cfg.ForfeitVTXOActorAskTimeout = 10 * time.Millisecond
	mgr.actors[vtxo2.Outpoint] = &blockingVTXOActorRef{
		id: vtxo2.Outpoint.String(),
	}

	start := time.Now()
	result := mgr.Receive(t.Context(), &ReserveForfeitRequest{
		Outpoints: []wire.OutPoint{
			vtxo1.Outpoint, vtxo2.Outpoint,
		},
	})
	_, err := result.Unpack()
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(start), time.Second)

	refAny, ok := mgr.actors[vtxo1.Outpoint]
	require.True(t, ok, "actor not found for vtxo1")

	ref1, ok := refAny.(*mockVTXOActorRef)
	require.True(t, ok, "expected *mockVTXOActorRef, got %T", refAny)

	_, ok = ref1.state.(*LiveState)
	require.True(
		t, ok, "expected LiveState after rollback, got %T", ref1.state,
	)
}

// TestReserveForfeitCallerTimeoutStillRollsBack verifies that rollback does
// not inherit a canceled caller context after a partial reservation.
func TestReserveForfeitCallerTimeoutStillRollsBack(t *testing.T) {
	t.Parallel()

	vtxo1 := makeDescriptor(t, 50000, 0)
	vtxo2 := makeDescriptor(t, 30000, 1)

	mgr, _ := newTestManager(t, []*Descriptor{vtxo1, vtxo2})
	mgr.cfg.ForfeitVTXOActorAskTimeout = time.Second
	mgr.actors[vtxo2.Outpoint] = &blockingVTXOActorRef{
		id: vtxo2.Outpoint.String(),
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()

	result := mgr.Receive(ctx, &ReserveForfeitRequest{
		Outpoints: []wire.OutPoint{
			vtxo1.Outpoint, vtxo2.Outpoint,
		},
	})
	_, err := result.Unpack()
	require.ErrorIs(t, err, context.DeadlineExceeded)

	refAny, ok := mgr.actors[vtxo1.Outpoint]
	require.True(t, ok, "actor not found for vtxo1")

	ref1, ok := refAny.(*mockVTXOActorRef)
	require.True(t, ok, "expected *mockVTXOActorRef, got %T", refAny)

	_, ok = ref1.state.(*LiveState)
	require.True(
		t, ok, "expected LiveState after rollback, got %T", ref1.state,
	)
}

// TestSpendReserveRejectedWhenPendingForfeit verifies that spend
// reservation fails for a VTXO already committed to forfeit.
func TestSpendReserveRejectedWhenPendingForfeit(t *testing.T) {
	t.Parallel()

	vtxo1 := makeDescriptor(t, 50000, 0)
	mgr, store := newTestManager(t, []*Descriptor{vtxo1})

	// Reserve for forfeit first.
	result := mgr.Receive(t.Context(), &ReserveForfeitRequest{
		Outpoints: []wire.OutPoint{vtxo1.Outpoint},
	})
	_, err := result.Unpack()
	require.NoError(t, err)

	// Now try to select for spend — store still lists it as Live
	// (store is a mock), but the actor will reject.
	store.On(
		"ListVTXOsByStatus", t.Context(), VTXOStatusLive,
	).Return([]*Descriptor{vtxo1}, nil)

	result = mgr.Receive(t.Context(), &SelectAndReserveSpendRequest{
		TargetAmount: 40000,
	})
	_, err = result.Unpack()
	require.Error(t, err)
	require.ErrorIs(t, err, ErrVTXOLiquidityLocked)
	require.Contains(t, err.Error(), "cannot reserve for spend")
}

// TestReleaseForfeit verifies that releasing a forfeit reservation returns
// VTXOs to LiveState.
func TestReleaseForfeit(t *testing.T) {
	t.Parallel()

	vtxo1 := makeDescriptor(t, 50000, 0)
	mgr, _ := newTestManager(t, []*Descriptor{vtxo1})

	// Reserve for forfeit.
	result := mgr.Receive(t.Context(), &ReserveForfeitRequest{
		Outpoints: []wire.OutPoint{vtxo1.Outpoint},
	})
	_, err := result.Unpack()
	require.NoError(t, err)

	// Release.
	result = mgr.Receive(t.Context(), &ReleaseForfeitRequest{
		Outpoints: []wire.OutPoint{vtxo1.Outpoint},
	})
	resp, err := result.Unpack()
	require.NoError(t, err)

	releaseResp, ok := resp.(*ReleaseForfeitResponse)
	require.True(t, ok, "expected *ReleaseForfeitResponse")
	require.Equal(t, 1, releaseResp.ReleasedCount)

	// Actor should be back in LiveState.
	refAny, ok := mgr.actors[vtxo1.Outpoint]
	require.True(t, ok, "actor not found")
	ref, ok := refAny.(*mockVTXOActorRef)
	require.True(t, ok, "expected *mockVTXOActorRef, got %T", refAny)
	_, ok = ref.state.(*LiveState)
	require.True(t, ok, "expected LiveState, got %T", ref.state)
}

// TestUnknownOutpointRejected verifies that the manager rejects operations
// on unknown outpoints.
func TestUnknownOutpointRejected(t *testing.T) {
	t.Parallel()

	mgr, _ := newTestManager(t, nil)

	unknownOP := wire.OutPoint{Index: 99}

	// ReleaseSpend with unknown outpoint.
	result := mgr.Receive(t.Context(), &ReleaseSpendRequest{
		Outpoints: []wire.OutPoint{unknownOP},
	})
	_, err := result.Unpack()
	require.Error(t, err)
	require.Contains(t, err.Error(), "no actor for outpoint")

	// ReserveForfeit with unknown outpoint.
	result = mgr.Receive(t.Context(), &ReserveForfeitRequest{
		Outpoints: []wire.OutPoint{unknownOP},
	})
	_, err = result.Unpack()
	require.Error(t, err)
	require.Contains(t, err.Error(), "no actor for outpoint")
}

// =============================================================================
// Input validation tests
// =============================================================================

// TestSelectAndReserveSpendZeroTarget verifies that a zero target amount is
// rejected before coin selection starts.
func TestSelectAndReserveSpendZeroTarget(t *testing.T) {
	t.Parallel()

	mgr, _ := newTestManager(t, nil)

	result := mgr.Receive(t.Context(), &SelectAndReserveSpendRequest{
		TargetAmount: 0,
	})
	_, err := result.Unpack()
	require.Error(t, err)
	require.Contains(t, err.Error(), "target amount must be positive")
}

// TestSelectAndReserveSpendNegativeTarget verifies that a negative target
// amount is rejected.
func TestSelectAndReserveSpendNegativeTarget(t *testing.T) {
	t.Parallel()

	mgr, _ := newTestManager(t, nil)

	result := mgr.Receive(t.Context(), &SelectAndReserveSpendRequest{
		TargetAmount: -1,
	})
	_, err := result.Unpack()
	require.Error(t, err)
	require.Contains(t, err.Error(), "target amount must be positive")
}

// TestReleaseSpendDuplicateOutpoints verifies that duplicate outpoints in a
// release request are normalized so the actor only receives one event.
func TestReleaseSpendDuplicateOutpoints(t *testing.T) {
	t.Parallel()

	vtxo1 := makeDescriptor(t, 50000, 0)
	mgr, store := newTestManager(t, []*Descriptor{vtxo1})

	// Reserve first.
	store.On(
		"ListVTXOsByStatus", t.Context(), VTXOStatusLive,
	).Return([]*Descriptor{vtxo1}, nil)

	result := mgr.Receive(t.Context(), &SelectAndReserveSpendRequest{
		TargetAmount: 40000,
	})
	_, err := result.Unpack()
	require.NoError(t, err)

	// Release with the same outpoint twice — should succeed without
	// the second pass hitting an invalid transition.
	result = mgr.Receive(t.Context(), &ReleaseSpendRequest{
		Outpoints: []wire.OutPoint{
			vtxo1.Outpoint, vtxo1.Outpoint,
		},
	})
	resp, err := result.Unpack()
	require.NoError(t, err)

	releaseResp, ok := resp.(*ReleaseSpendResponse)
	require.True(t, ok, "expected *ReleaseSpendResponse")
	require.Equal(t, 1, releaseResp.ReleasedCount)
}

// TestReserveForfeitDuplicateOutpoints verifies that duplicate outpoints in a
// forfeit reservation are normalized.
func TestReserveForfeitDuplicateOutpoints(t *testing.T) {
	t.Parallel()

	vtxo1 := makeDescriptor(t, 50000, 0)
	mgr, _ := newTestManager(t, []*Descriptor{vtxo1})

	// Reserve with the same outpoint twice.
	result := mgr.Receive(t.Context(), &ReserveForfeitRequest{
		Outpoints: []wire.OutPoint{
			vtxo1.Outpoint, vtxo1.Outpoint,
		},
	})
	_, err := result.Unpack()
	require.NoError(t, err)

	refAny, ok := mgr.actors[vtxo1.Outpoint]
	require.True(t, ok, "actor not found")
	ref, ok := refAny.(*mockVTXOActorRef)
	require.True(t, ok, "expected *mockVTXOActorRef, got %T", refAny)
	_, ok = ref.state.(*PendingForfeitState)
	require.True(t, ok, "expected PendingForfeitState, got %T",
		ref.state)
}

// TestReleaseForfeitDuplicateOutpoints verifies that duplicate outpoints in a
// forfeit release are normalized.
func TestReleaseForfeitDuplicateOutpoints(t *testing.T) {
	t.Parallel()

	vtxo1 := makeDescriptor(t, 50000, 0)
	mgr, _ := newTestManager(t, []*Descriptor{vtxo1})

	// Reserve for forfeit first.
	result := mgr.Receive(t.Context(), &ReserveForfeitRequest{
		Outpoints: []wire.OutPoint{vtxo1.Outpoint},
	})
	_, err := result.Unpack()
	require.NoError(t, err)

	// Release with duplicate outpoints.
	result = mgr.Receive(t.Context(), &ReleaseForfeitRequest{
		Outpoints: []wire.OutPoint{
			vtxo1.Outpoint, vtxo1.Outpoint,
		},
	})
	resp, err := result.Unpack()
	require.NoError(t, err)

	releaseResp, ok := resp.(*ReleaseForfeitResponse)
	require.True(t, ok, "expected *ReleaseForfeitResponse")
	require.Equal(t, 1, releaseResp.ReleasedCount)
}

// =============================================================================
// Coin selection unit tests
// =============================================================================

// TestSelectLargestFirst verifies the largest-first coin selection logic.
func TestSelectLargestFirst(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		amounts   []btcutil.Amount
		target    btcutil.Amount
		wantCount int
		wantNil   bool
	}{
		{
			name: "single VTXO covers target",
			amounts: []btcutil.Amount{
				50000,
				30000,
				10000,
			},
			target:    40000,
			wantCount: 1,
		},
		{
			name: "two VTXOs needed",
			amounts: []btcutil.Amount{
				30000,
				25000,
				10000,
			},
			target:    50000,
			wantCount: 2,
		},
		{
			name: "all VTXOs needed",
			amounts: []btcutil.Amount{
				20000,
				15000,
				10000,
			},
			target:    45000,
			wantCount: 3,
		},
		{
			name: "insufficient funds",
			amounts: []btcutil.Amount{
				20000,
				10000,
			},
			target:  50000,
			wantNil: true,
		},
		{
			name:    "empty candidates",
			amounts: nil,
			target:  1000,
			wantNil: true,
		},
		{
			name: "exact match",
			amounts: []btcutil.Amount{
				50000,
			},
			target:    50000,
			wantCount: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var candidates []*Descriptor
			for i, amount := range tc.amounts {
				candidates = append(candidates, &Descriptor{
					Outpoint: wire.OutPoint{
						Index: uint32(i),
					},
					Amount: amount,
				})
			}

			res, err := coinselect.LargestFirst(
				candidates, func(d *Descriptor) btcutil.Amount {
					return d.Amount
				}, coinselect.Request{
					Target: tc.target,
				},
			)

			if tc.wantNil {
				require.Error(t, err)
				require.Nil(t, res.Selected)

				return
			}

			require.NoError(t, err)
			require.Len(t, res.Selected, tc.wantCount)

			// Verify total covers target.
			var total btcutil.Amount
			for _, v := range res.Selected {
				total += v.Amount
			}
			require.GreaterOrEqual(
				t, int64(total), int64(tc.target),
			)
		})
	}
}

// TestRollbackOnPartialSpendFailure verifies that if the Nth VTXO actor
// rejects SpendReserveEvent, the first N-1 are rolled back to LiveState.
func TestRollbackOnPartialSpendFailure(t *testing.T) {
	t.Parallel()

	vtxo1 := makeDescriptor(t, 30000, 0)
	vtxo2 := makeDescriptor(t, 25000, 1)

	mgr, store := newTestManager(t, []*Descriptor{vtxo1, vtxo2})

	// Put vtxo2's actor in PendingForfeitState so SpendReserve fails.
	refAny, ok := mgr.actors[vtxo2.Outpoint]
	require.True(t, ok, "actor not found for vtxo2")

	ref2, ok := refAny.(*mockVTXOActorRef)
	require.True(t, ok, "expected *mockVTXOActorRef, got %T", refAny)

	ref2.state = &PendingForfeitState{
		VTXO:              vtxo2,
		RequestedAtHeight: 0,
	}

	// Selection returns both (store doesn't know about FSM state).
	store.On(
		"ListVTXOsByStatus", t.Context(), VTXOStatusLive,
	).Return([]*Descriptor{vtxo1, vtxo2}, nil)

	result := mgr.Receive(t.Context(), &SelectAndReserveSpendRequest{
		TargetAmount: 50000,
	})
	_, err := result.Unpack()
	require.Error(t, err)
	require.ErrorIs(t, err, ErrVTXOLiquidityLocked)

	// vtxo1 should be rolled back to LiveState.
	refAny1, ok := mgr.actors[vtxo1.Outpoint]
	require.True(t, ok, "actor not found for vtxo1")

	ref1, ok := refAny1.(*mockVTXOActorRef)
	require.True(t, ok, "expected *mockVTXOActorRef, got %T", refAny1)

	_, ok = ref1.state.(*LiveState)
	require.True(
		t, ok, "expected LiveState after rollback, got %T", ref1.state,
	)
}

// =============================================================================
// Recovery tests
// =============================================================================
//
// These tests verify that VTXOs recovered in SpendingState or
// PendingForfeitState correctly enforce admission rules, as they would
// after a daemon restart. The manager is constructed with mock actors
// pre-initialized in the recovered state, simulating what spawnVTXOActor
// produces when it calls statusToState on a persisted descriptor.

// TestRecoveredSpendingRejectsForfeit verifies that a VTXO recovered in
// SpendingState rejects cooperative forfeit admission. After a restart,
// a VTXO that was claimed for an OOR spend must still block cooperative
// consumption.
func TestRecoveredSpendingRejectsForfeit(t *testing.T) {
	t.Parallel()

	vtxo := makeDescriptor(t, 50000, 0)

	// Simulate recovery: actor starts in SpendingState as if
	// restored from VTXOStatusSpending by statusToState.
	store := &MockVTXOStore{}
	mgr := &Manager{
		cfg: &ManagerConfig{
			Store:      store,
			RoundActor: newMockRoundActorRef(t),
		},
		actors: make(map[wire.OutPoint]VTXOActorRef),
	}

	mgr.actors[vtxo.Outpoint] = newMockVTXOActorRef(
		vtxo.Outpoint.String(),
		&SpendingState{
			VTXO:              vtxo,
			LastCheckedHeight: vtxo.CreatedHeight,
		},
	)

	// Forfeit reservation must fail because the VTXO is spending.
	result := mgr.Receive(
		t.Context(), &ReserveForfeitRequest{
			Outpoints: []wire.OutPoint{vtxo.Outpoint},
		},
	)
	_, err := result.Unpack()
	require.Error(t, err)
	require.Contains(t, err.Error(), "reserve forfeit")
}

// TestRecoveredSpendingAllowsRelease verifies that a VTXO recovered in
// SpendingState can be released back to LiveState. This covers the case
// where a daemon restarts mid-OOR and the caller decides to cancel.
func TestRecoveredSpendingAllowsRelease(t *testing.T) {
	t.Parallel()

	vtxo := makeDescriptor(t, 50000, 0)

	store := &MockVTXOStore{}
	mgr := &Manager{
		cfg: &ManagerConfig{
			Store:      store,
			RoundActor: newMockRoundActorRef(t),
		},
		actors: make(map[wire.OutPoint]VTXOActorRef),
	}

	mgr.actors[vtxo.Outpoint] = newMockVTXOActorRef(
		vtxo.Outpoint.String(),
		&SpendingState{
			VTXO:              vtxo,
			LastCheckedHeight: vtxo.CreatedHeight,
		},
	)

	// Release should succeed and transition back to LiveState.
	result := mgr.Receive(
		t.Context(), &ReleaseSpendRequest{
			Outpoints: []wire.OutPoint{vtxo.Outpoint},
		},
	)
	resp, err := result.Unpack()
	require.NoError(t, err)

	releaseResp, ok := resp.(*ReleaseSpendResponse)
	require.True(t, ok)
	require.Equal(t, 1, releaseResp.ReleasedCount)

	// Verify actor is back in LiveState.
	refAny, ok := mgr.actors[vtxo.Outpoint]
	require.True(t, ok, "actor not found")
	ref, ok := refAny.(*mockVTXOActorRef)
	require.True(t, ok, "expected *mockVTXOActorRef, got %T", refAny)
	_, ok = ref.state.(*LiveState)
	require.True(t, ok, "expected LiveState, got %T", ref.state)
}

// TestRecoveredSpendingAllowsCompletion verifies that a VTXO recovered
// in SpendingState can be completed to SpentState. This covers the case
// where an OOR session resumes after restart and reaches the commit
// phase.
func TestRecoveredSpendingAllowsCompletion(t *testing.T) {
	t.Parallel()

	vtxo := makeDescriptor(t, 50000, 0)

	store := &MockVTXOStore{}
	mgr := &Manager{
		cfg: &ManagerConfig{
			Store:      store,
			RoundActor: newMockRoundActorRef(t),
		},
		actors: make(map[wire.OutPoint]VTXOActorRef),
	}

	mgr.actors[vtxo.Outpoint] = newMockVTXOActorRef(
		vtxo.Outpoint.String(),
		&SpendingState{
			VTXO:              vtxo,
			LastCheckedHeight: vtxo.CreatedHeight,
		},
	)

	// Completion should succeed.
	result := mgr.Receive(
		t.Context(), &CompleteSpendRequest{
			Outpoints: []wire.OutPoint{vtxo.Outpoint},
		},
	)
	resp, err := result.Unpack()
	require.NoError(t, err)

	completeResp, ok := resp.(*CompleteSpendResponse)
	require.True(t, ok)
	require.Equal(t, 1, completeResp.CompletedCount)

	// Verify actor reached terminal SpentState.
	refAny, ok := mgr.actors[vtxo.Outpoint]
	require.True(t, ok, "actor not found")
	ref, ok := refAny.(*mockVTXOActorRef)
	require.True(t, ok, "expected *mockVTXOActorRef, got %T", refAny)
	_, ok = ref.state.(*SpentState)
	require.True(t, ok, "expected SpentState, got %T", ref.state)
}

// TestRecoveredPendingForfeitRejectsSpend verifies that a VTXO
// recovered in PendingForfeitState rejects OOR spend admission. After
// a restart, a VTXO that was claimed for cooperative consumption must
// still block OOR spend selection.
func TestRecoveredPendingForfeitRejectsSpend(t *testing.T) {
	t.Parallel()

	vtxo := makeDescriptor(t, 50000, 0)

	store := &MockVTXOStore{}
	mgr := &Manager{
		cfg: &ManagerConfig{
			Store:      store,
			RoundActor: newMockRoundActorRef(t),
		},
		actors: make(map[wire.OutPoint]VTXOActorRef),
	}

	mgr.actors[vtxo.Outpoint] = newMockVTXOActorRef(
		vtxo.Outpoint.String(),
		&PendingForfeitState{
			VTXO:              vtxo,
			RequestedAtHeight: 0,
		},
	)

	// Store returns the VTXO as a live candidate (the store
	// doesn't filter by FSM state, only by persisted status).
	store.On(
		"ListVTXOsByStatus", t.Context(), VTXOStatusLive,
	).Return([]*Descriptor{vtxo}, nil)

	// Spend selection should fail because the actor rejects it.
	result := mgr.Receive(
		t.Context(), &SelectAndReserveSpendRequest{
			TargetAmount: 40000,
		},
	)
	_, err := result.Unpack()
	require.Error(t, err)
}

// TestRecoveredPendingForfeitAllowsRelease verifies that a VTXO
// recovered in PendingForfeitState can be released back to LiveState.
// This covers the case where a round was in progress when the daemon
// crashed, and after restart the wallet decides to release.
func TestRecoveredPendingForfeitAllowsRelease(t *testing.T) {
	t.Parallel()

	vtxo := makeDescriptor(t, 50000, 0)

	store := &MockVTXOStore{}
	mgr := &Manager{
		cfg: &ManagerConfig{
			Store:      store,
			RoundActor: newMockRoundActorRef(t),
		},
		actors: make(map[wire.OutPoint]VTXOActorRef),
	}

	mgr.actors[vtxo.Outpoint] = newMockVTXOActorRef(
		vtxo.Outpoint.String(),
		&PendingForfeitState{
			VTXO:              vtxo,
			RequestedAtHeight: 0,
		},
	)

	// Release should succeed.
	result := mgr.Receive(
		t.Context(), &ReleaseForfeitRequest{
			Outpoints: []wire.OutPoint{vtxo.Outpoint},
		},
	)
	resp, err := result.Unpack()
	require.NoError(t, err)

	releaseResp, ok := resp.(*ReleaseForfeitResponse)
	require.True(t, ok)
	require.Equal(t, 1, releaseResp.ReleasedCount)

	// Verify actor is back in LiveState.
	refAny, ok := mgr.actors[vtxo.Outpoint]
	require.True(t, ok, "actor not found")
	ref, ok := refAny.(*mockVTXOActorRef)
	require.True(t, ok, "expected *mockVTXOActorRef, got %T", refAny)
	_, ok = ref.state.(*LiveState)
	require.True(t, ok, "expected LiveState, got %T", ref.state)
}

// =============================================================================
// Atomic cooperative select-and-reserve tests
// =============================================================================

// TestSelectAndReserveForfeitSuccess verifies that the manager selects
// and reserves VTXOs for cooperative consumption using largest-first
// selection, driving each into PendingForfeitState.
func TestSelectAndReserveForfeitSuccess(t *testing.T) {
	t.Parallel()

	vtxo1 := makeDescriptor(t, 30000, 0)
	vtxo2 := makeDescriptor(t, 50000, 1)
	vtxo3 := makeDescriptor(t, 20000, 2)

	mgr, store := newTestManager(t, []*Descriptor{
		vtxo1, vtxo2, vtxo3,
	})

	store.On(
		"ListVTXOsByStatus", t.Context(), VTXOStatusLive,
	).Return([]*Descriptor{vtxo1, vtxo2, vtxo3}, nil)

	result := mgr.Receive(
		t.Context(), &SelectAndReserveForfeitRequest{
			TargetAmount: 40000,
		},
	)
	resp, err := result.Unpack()
	require.NoError(t, err)

	forfeitResp, ok := resp.(*SelectAndReserveForfeitResponse)
	require.True(t, ok)

	// Largest-first picks vtxo2 (50000) covering 40000.
	require.Len(t, forfeitResp.SelectedVTXOs, 1)
	require.Equal(
		t, vtxo2.Outpoint, forfeitResp.SelectedVTXOs[0].Outpoint,
	)
	require.Equal(t,
		btcutil.Amount(50000), forfeitResp.TotalSelected,
	)

	// Verify the actor is now in PendingForfeitState.
	actorAny, ok := mgr.actors[vtxo2.Outpoint]
	require.True(t, ok, "actor not found for vtxo2")

	actorRef, ok := actorAny.(*mockVTXOActorRef)
	require.True(t, ok, "expected *mockVTXOActorRef")

	_, ok = actorRef.state.(*PendingForfeitState)
	require.True(
		t, ok, "expected PendingForfeitState, got %T", actorRef.state,
	)
}

// TestSelectAndReserveForfeitMultipleVTXOs verifies that coin selection
// picks multiple VTXOs when no single VTXO covers the target.
func TestSelectAndReserveForfeitMultipleVTXOs(t *testing.T) {
	t.Parallel()

	vtxo1 := makeDescriptor(t, 30000, 0)
	vtxo2 := makeDescriptor(t, 25000, 1)
	vtxo3 := makeDescriptor(t, 20000, 2)

	mgr, store := newTestManager(t, []*Descriptor{
		vtxo1, vtxo2, vtxo3,
	})

	store.On(
		"ListVTXOsByStatus", t.Context(), VTXOStatusLive,
	).Return([]*Descriptor{vtxo1, vtxo2, vtxo3}, nil)

	result := mgr.Receive(
		t.Context(), &SelectAndReserveForfeitRequest{
			TargetAmount: 50000,
		},
	)
	resp, err := result.Unpack()
	require.NoError(t, err)

	forfeitResp, ok := resp.(*SelectAndReserveForfeitResponse)
	require.True(t, ok, "expected *SelectAndReserveForfeitResponse")

	// Largest-first: vtxo1 (30000) + vtxo2 (25000) = 55000.
	require.Len(t, forfeitResp.SelectedVTXOs, 2)
	require.Equal(t,
		btcutil.Amount(55000), forfeitResp.TotalSelected,
	)
}

// TestSelectAndReserveForfeitInsufficientFunds verifies that selection
// fails when live candidates cannot cover the target.
func TestSelectAndReserveForfeitInsufficientFunds(t *testing.T) {
	t.Parallel()

	vtxo1 := makeDescriptor(t, 10000, 0)

	mgr, store := newTestManager(t, []*Descriptor{vtxo1})

	store.On(
		"ListVTXOsByStatus", t.Context(), VTXOStatusLive,
	).Return([]*Descriptor{vtxo1}, nil)
	store.On("ListLiveVTXOs", t.Context()).Return(
		[]*Descriptor{vtxo1}, nil,
	)

	result := mgr.Receive(
		t.Context(), &SelectAndReserveForfeitRequest{
			TargetAmount: 50000,
		},
	)
	_, err := result.Unpack()
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInsufficientSpendableFunds)
}

// TestSelectAndReserveForfeitSkipsNonLive verifies that VTXOs already
// in SpendingState or PendingForfeitState are excluded from candidates
// because only Live VTXOs are returned by ListVTXOsByStatus.
func TestSelectAndReserveForfeitSkipsNonLive(t *testing.T) {
	t.Parallel()

	vtxo1 := makeDescriptor(t, 50000, 0)
	vtxo2 := makeDescriptor(t, 30000, 1)

	mgr, store := newTestManager(t, []*Descriptor{
		vtxo1, vtxo2,
	})

	// Put vtxo1 into SpendingState so it won't be listed as Live.
	mgr.actors[vtxo1.Outpoint] = newMockVTXOActorRef(
		vtxo1.Outpoint.String(), &SpendingState{
			VTXO: vtxo1,
		},
	)

	// Store only returns vtxo2 as Live.
	store.On(
		"ListVTXOsByStatus", t.Context(), VTXOStatusLive,
	).Return([]*Descriptor{vtxo2}, nil)

	result := mgr.Receive(
		t.Context(), &SelectAndReserveForfeitRequest{
			TargetAmount: 25000,
		},
	)
	resp, err := result.Unpack()
	require.NoError(t, err)

	forfeitResp, ok := resp.(*SelectAndReserveForfeitResponse)
	require.True(t, ok, "expected *SelectAndReserveForfeitResponse")

	// Only vtxo2 was available and selected.
	require.Len(t, forfeitResp.SelectedVTXOs, 1)
	require.Equal(
		t, vtxo2.Outpoint, forfeitResp.SelectedVTXOs[0].Outpoint,
	)
}

// TestSelectAndReserveForfeitPartialRollback verifies that if one VTXO
// rejects PendingForfeitEvent, previously reserved VTXOs are rolled
// back via ForfeitReleasedEvent.
func TestSelectAndReserveForfeitPartialRollback(t *testing.T) {
	t.Parallel()

	vtxo1 := makeDescriptor(t, 30000, 0)
	vtxo2 := makeDescriptor(t, 25000, 1)

	mgr, store := newTestManager(t, []*Descriptor{
		vtxo1, vtxo2,
	})

	// Put vtxo2 (second in sort order) into SpendingState so it
	// will reject PendingForfeitEvent during reservation.
	mgr.actors[vtxo2.Outpoint] = newMockVTXOActorRef(
		vtxo2.Outpoint.String(), &SpendingState{
			VTXO: vtxo2,
		},
	)

	// Store returns both as Live (stale view).
	store.On(
		"ListVTXOsByStatus", t.Context(), VTXOStatusLive,
	).Return([]*Descriptor{vtxo1, vtxo2}, nil)

	// Target requires both VTXOs.
	result := mgr.Receive(
		t.Context(), &SelectAndReserveForfeitRequest{
			TargetAmount: 50000,
		},
	)
	_, err := result.Unpack()
	require.Error(t, err)
	require.Contains(t, err.Error(), "reserve forfeit")

	// Verify vtxo1 was rolled back to LiveState.
	actorAny, ok := mgr.actors[vtxo1.Outpoint]
	require.True(t, ok, "actor not found for vtxo1")

	actorRef, ok := actorAny.(*mockVTXOActorRef)
	require.True(t, ok, "expected *mockVTXOActorRef")

	_, ok = actorRef.state.(*LiveState)
	require.True(
		t, ok, "expected LiveState after rollback, got %T",
		actorRef.state,
	)
}
