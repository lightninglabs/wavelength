package vtxo

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
)

// mockReservationStore is a test double for the durable spending-reservation
// index. It returns a fixed reserved set (or error) from ListReservedOutpoints,
// which is the only method the VTXO manager's startup sweep needs. Row deletion
// is no longer a manager concern: it happens atomically with the VTXO status
// change inside the actor's transition (the flag is asserted by
// TestSpendReleasedFromSpendingState and friends; the atomic delete by the
// db-level TestUpdateVTXOStatusReleasingReservation).
type mockReservationStore struct {
	mu sync.Mutex

	reserved []wire.OutPoint
	listErr  error
}

func (m *mockReservationStore) ListReservedOutpoints(_ context.Context) (
	[]wire.OutPoint, error) {

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listErr != nil {
		return nil, m.listErr
	}

	out := make([]wire.OutPoint, len(m.reserved))
	copy(out, m.reserved)

	return out, nil
}

// Compile-time check that mockReservationStore satisfies the interface.
var _ SpendingReservationStore = (*mockReservationStore)(nil)

// newSpendingManager builds a Manager whose actors are all in SpendingState for
// the given descriptors, wired to the supplied reservation store.
func newSpendingManager(t *testing.T, descriptors []*Descriptor,
	resStore SpendingReservationStore) (*Manager, *MockVTXOStore) {

	t.Helper()

	store := &MockVTXOStore{}
	mgr := &Manager{
		cfg: &ManagerConfig{
			Store:            store,
			RoundActor:       newMockRoundActorRef(t),
			ReservationStore: resStore,
		},
		actors: make(map[wire.OutPoint]VTXOActorRef),
	}

	for _, vtxo := range descriptors {
		ref := newMockVTXOActorRef(
			vtxo.Outpoint.String(),
			&SpendingState{
				VTXO:              vtxo,
				LastCheckedHeight: vtxo.CreatedHeight,
			},
		)
		mgr.actors[vtxo.Outpoint] = ref
	}

	return mgr, store
}

// actorState returns the current FSM state of a mock actor by outpoint.
func actorState(t *testing.T, mgr *Manager, op wire.OutPoint) VTXOState {
	t.Helper()

	refAny := mgr.actors[op]
	require.NotNil(t, refAny)

	ref, ok := refAny.(*mockVTXOActorRef)
	require.True(t, ok, "expected *mockVTXOActorRef, got %T", refAny)

	return ref.state
}

// TestSweepReleasesOnlyOrphanedReservations verifies that the startup sweep
// releases Spending VTXOs with no reservation row (orphans) while leaving the
// reserved ones untouched.
func TestSweepReleasesOnlyOrphanedReservations(t *testing.T) {
	t.Parallel()

	reserved := makeDescriptor(t, 50000, 0)
	orphanA := makeDescriptor(t, 40000, 1)
	orphanB := makeDescriptor(t, 30000, 2)

	resStore := &mockReservationStore{
		reserved: []wire.OutPoint{
			reserved.Outpoint,
		},
	}

	mgr, store := newSpendingManager(
		t, []*Descriptor{reserved, orphanA, orphanB}, resStore,
	)

	store.On(
		"ListVTXOsByStatus", t.Context(), VTXOStatusSpending,
	).Return([]*Descriptor{reserved, orphanA, orphanB}, nil)

	mgr.sweepOrphanedReservations(t.Context())

	// The reserved VTXO stays in SpendingState.
	_, ok := actorState(t, mgr, reserved.Outpoint).(*SpendingState)
	require.True(t, ok, "reserved VTXO must remain Spending")

	// The two orphans are released back to LiveState.
	_, ok = actorState(t, mgr, orphanA.Outpoint).(*LiveState)
	require.True(t, ok, "orphan A must be released to Live")

	_, ok = actorState(t, mgr, orphanB.Outpoint).(*LiveState)
	require.True(t, ok, "orphan B must be released to Live")

	store.AssertExpectations(t)
}

// TestSweepAbortsOnListReservationsError verifies that an error reading the
// reservation index aborts the sweep without releasing anything, since acting
// on incomplete info could free an in-flight spend.
func TestSweepAbortsOnListReservationsError(t *testing.T) {
	t.Parallel()

	orphan := makeDescriptor(t, 40000, 0)

	resStore := &mockReservationStore{
		listErr: errors.New("reservation index unreadable"),
	}

	mgr, store := newSpendingManager(
		t, []*Descriptor{orphan}, resStore,
	)

	store.On(
		"ListVTXOsByStatus", t.Context(), VTXOStatusSpending,
	).Return([]*Descriptor{orphan}, nil)

	mgr.sweepOrphanedReservations(t.Context())

	// Nothing released: the VTXO stays Spending despite having no row.
	_, ok := actorState(t, mgr, orphan.Outpoint).(*SpendingState)
	require.True(t, ok, "VTXO must remain Spending on list error")
}

// TestSweepSkippedWhenStoreNil verifies that a nil reservation store disables
// the sweep entirely (no store query, no release).
func TestSweepSkippedWhenStoreNil(t *testing.T) {
	t.Parallel()

	orphan := makeDescriptor(t, 40000, 0)

	mgr, store := newSpendingManager(t, []*Descriptor{orphan}, nil)
	mgr.cfg.ReservationStore = nil

	mgr.sweepOrphanedReservations(t.Context())

	// ListVTXOsByStatus must not have been called; VTXO stays Spending.
	store.AssertNotCalled(t, "ListVTXOsByStatus")

	_, ok := actorState(t, mgr, orphan.Outpoint).(*SpendingState)
	require.True(t, ok, "VTXO must remain Spending when store is nil")
}

// TestReleaseSpendTransitionsToLive verifies that a successful spend release
// drives the VTXO out of SpendingState back to LiveState. The reservation row
// is dropped atomically inside the actor's status transition, so the manager
// no longer issues a separate delete (see the db-level
// TestUpdateVTXOStatusReleasingReservation).
func TestReleaseSpendTransitionsToLive(t *testing.T) {
	t.Parallel()

	vtxo1 := makeDescriptor(t, 50000, 0)

	resStore := &mockReservationStore{}

	// The actor starts in SpendingState so release has a real transition to
	// perform.
	mgr, _ := newSpendingManager(t, []*Descriptor{vtxo1}, resStore)

	result := mgr.Receive(t.Context(), &ReleaseSpendRequest{
		Outpoints: []wire.OutPoint{vtxo1.Outpoint},
	})
	resp, err := result.Unpack()
	require.NoError(t, err)

	releaseResp, ok := resp.(*ReleaseSpendResponse)
	require.True(t, ok, "expected *ReleaseSpendResponse")
	require.Equal(t, 1, releaseResp.ReleasedCount)

	// Actor is back in LiveState.
	_, ok = actorState(t, mgr, vtxo1.Outpoint).(*LiveState)
	require.True(t, ok, "expected LiveState after release")
}

// TestCompleteSpendTransitionsToSpent verifies that completing a spend drives
// the VTXO out of SpendingState to terminal SpentState. As with release, the
// reservation row deletion is atomic with the status change in the actor, not
// a separate manager call.
func TestCompleteSpendTransitionsToSpent(t *testing.T) {
	t.Parallel()

	vtxo1 := makeDescriptor(t, 50000, 0)

	resStore := &mockReservationStore{}
	mgr, _ := newSpendingManager(
		t, []*Descriptor{vtxo1}, resStore,
	)

	result := mgr.Receive(t.Context(), &CompleteSpendRequest{
		Outpoints: []wire.OutPoint{vtxo1.Outpoint},
	})
	resp, err := result.Unpack()
	require.NoError(t, err)

	completeResp, ok := resp.(*CompleteSpendResponse)
	require.True(t, ok, "expected *CompleteSpendResponse")
	require.Equal(t, 1, completeResp.CompletedCount)

	_, ok = actorState(t, mgr, vtxo1.Outpoint).(*SpentState)
	require.True(t, ok, "expected SpentState after complete")
}
