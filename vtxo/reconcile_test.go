package vtxo

import (
	"testing"

	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// newSpendingTestManager builds a Manager whose actors all start in
// SpendingState, mirroring the post-restart recovery of VTXOs that were
// reserved for an OOR spend. The store reports the same set as Spending.
func newSpendingTestManager(t *testing.T,
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

	for _, d := range descriptors {
		d.Status = VTXOStatusSpending
		mgr.actors[d.Outpoint] = newMockVTXOActorRef(
			d.Outpoint.String(),
			&SpendingState{
				VTXO:              d,
				LastCheckedHeight: d.CreatedHeight,
			},
		)
	}

	return mgr, store
}

// requireActorState asserts the mock actor for outpoint is in state T.
func requireActorState[T VTXOState](t *testing.T, mgr *Manager,
	op wire.OutPoint) {

	t.Helper()

	ref, ok := mgr.actors[op].(*mockVTXOActorRef)
	require.True(t, ok, "expected *mockVTXOActorRef, got %T", mgr.actors[op])

	_, ok = ref.state.(T)
	require.Truef(t, ok, "expected state %T, got %T", *new(T), ref.state)
}

// TestReconcileSpendingReservationsReleasesOrphans verifies the core #587 fix:
// a Spending VTXO that no live OOR session claims is released back to Live,
// while a Spending VTXO a live session still holds is left untouched.
func TestReconcileSpendingReservationsReleasesOrphans(t *testing.T) {
	t.Parallel()

	claimed := makeDescriptor(t, 10000, 0)
	orphanA := makeDescriptor(t, 20000, 1)
	orphanB := makeDescriptor(t, 30000, 2)

	mgr, store := newSpendingTestManager(t, []*Descriptor{
		claimed, orphanA, orphanB,
	})

	store.On(
		"ListVTXOsByStatus", mock.Anything, VTXOStatusSpending,
	).Return([]*Descriptor{claimed, orphanA, orphanB}, nil)

	// Only `claimed` is still held by a live outgoing OOR session.
	result := mgr.Receive(t.Context(),
		&ReconcileSpendingReservationsRequest{
			ClaimedInputs: []wire.OutPoint{claimed.Outpoint},
		},
	)
	resp, err := result.Unpack()
	require.NoError(t, err)

	reconcileResp, ok := resp.(*ReconcileSpendingReservationsResponse)
	require.True(t, ok)
	require.Equal(t, 3, reconcileResp.ScannedCount)
	require.Equal(t, 1, reconcileResp.ClaimedCount)
	require.Equal(t, 2, reconcileResp.ReleasedCount)

	// Orphans are back in LiveState; the claimed reservation is untouched.
	requireActorState[*LiveState](t, mgr, orphanA.Outpoint)
	requireActorState[*LiveState](t, mgr, orphanB.Outpoint)
	requireActorState[*SpendingState](t, mgr, claimed.Outpoint)
}

// TestReconcileSpendingReservationsNoOrphans verifies that when every Spending
// VTXO is still claimed by a live session, nothing is released.
func TestReconcileSpendingReservationsNoOrphans(t *testing.T) {
	t.Parallel()

	v1 := makeDescriptor(t, 10000, 0)
	v2 := makeDescriptor(t, 20000, 1)

	mgr, store := newSpendingTestManager(t, []*Descriptor{v1, v2})

	store.On(
		"ListVTXOsByStatus", mock.Anything, VTXOStatusSpending,
	).Return([]*Descriptor{v1, v2}, nil)

	result := mgr.Receive(t.Context(),
		&ReconcileSpendingReservationsRequest{
			ClaimedInputs: []wire.OutPoint{v1.Outpoint, v2.Outpoint},
		},
	)
	resp, err := result.Unpack()
	require.NoError(t, err)

	reconcileResp := resp.(*ReconcileSpendingReservationsResponse)
	require.Equal(t, 0, reconcileResp.ReleasedCount)
	requireActorState[*SpendingState](t, mgr, v1.Outpoint)
	requireActorState[*SpendingState](t, mgr, v2.Outpoint)
}

// TestReconcileSpendingReservationsEmptyClaimedReleasesAll verifies that with
// no live sessions at all (empty claimed set), every Spending VTXO is treated
// as an orphan and released. This is the full-starvation recovery case.
func TestReconcileSpendingReservationsEmptyClaimedReleasesAll(t *testing.T) {
	t.Parallel()

	v1 := makeDescriptor(t, 10000, 0)
	v2 := makeDescriptor(t, 20000, 1)

	mgr, store := newSpendingTestManager(t, []*Descriptor{v1, v2})

	store.On(
		"ListVTXOsByStatus", mock.Anything, VTXOStatusSpending,
	).Return([]*Descriptor{v1, v2}, nil)

	result := mgr.Receive(t.Context(),
		&ReconcileSpendingReservationsRequest{ClaimedInputs: nil},
	)
	resp, err := result.Unpack()
	require.NoError(t, err)

	reconcileResp := resp.(*ReconcileSpendingReservationsResponse)
	require.Equal(t, 2, reconcileResp.ReleasedCount)
	requireActorState[*LiveState](t, mgr, v1.Outpoint)
	requireActorState[*LiveState](t, mgr, v2.Outpoint)
}

// TestOrphanedReservations covers the pure set-difference helper.
func TestOrphanedReservations(t *testing.T) {
	t.Parallel()

	a := wire.OutPoint{Index: 1}
	b := wire.OutPoint{Index: 2}
	c := wire.OutPoint{Index: 3}
	spending := []*Descriptor{{Outpoint: a}, {Outpoint: b}, {Outpoint: c}}

	require.ElementsMatch(t, []wire.OutPoint{a, c},
		orphanedReservations(spending, []wire.OutPoint{b}))

	require.ElementsMatch(t, []wire.OutPoint{a, b, c},
		orphanedReservations(spending, nil))

	require.Empty(t, orphanedReservations(
		spending, []wire.OutPoint{a, b, c},
	))

	require.Empty(t, orphanedReservations(nil, []wire.OutPoint{a}))
}
