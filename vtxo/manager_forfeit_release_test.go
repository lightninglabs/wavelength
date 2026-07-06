package vtxo

import (
	"errors"
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
)

// newPendingForfeitManager builds a Manager whose actors are all in
// PendingForfeitState for the given descriptors. This mirrors the state a VTXO
// is left in when an in-flight cooperative round (refresh, leave, or directed
// send) reserved it as a forfeit input and the daemon then restarted before the
// round reached the point of no return.
func newPendingForfeitManager(t *testing.T,
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
		// Mirror real recovery: the descriptor is cached in the
		// liveDescriptors snapshot with its on-disk PendingForfeit
		// status, and an actor is spawned in PendingForfeitState.
		vtxo.Status = VTXOStatusPendingForfeit
		mgr.liveDescriptors = append(mgr.liveDescriptors, vtxo)

		ref := newMockVTXOActorRef(
			vtxo.Outpoint.String(),
			&PendingForfeitState{
				VTXO:              vtxo,
				RequestedAtHeight: vtxo.CreatedHeight,
			},
		)
		mgr.actors[vtxo.Outpoint] = ref
	}

	return mgr, store
}

// snapshotStatus returns the status of the given outpoint in the manager's
// cached liveDescriptors snapshot, or false if it is absent.
func snapshotStatus(mgr *Manager, op wire.OutPoint) (VTXOStatus, bool) {
	for _, desc := range mgr.liveDescriptors {
		if desc.Outpoint == op {
			return desc.Status, true
		}
	}

	return 0, false
}

// TestReleaseOrphanedForfeitsReturnsToLive verifies that the startup sweep
// returns every VTXO stranded in PendingForfeitState back to LiveState. The
// owning round FSM is in-memory only, so any PendingForfeit VTXO found at
// startup is provably orphaned and safe to release: no forfeit signature has
// left the process yet, so the operator cannot broadcast a forfeit.
func TestReleaseOrphanedForfeitsReturnsToLive(t *testing.T) {
	t.Parallel()

	orphanA := makeDescriptor(t, 50000, 0)
	orphanB := makeDescriptor(t, 40000, 1)

	mgr, store := newPendingForfeitManager(
		t, []*Descriptor{orphanA, orphanB},
	)

	store.On(
		"ListVTXOsByStatus", t.Context(), VTXOStatusPendingForfeit,
	).Return([]*Descriptor{orphanA, orphanB}, nil)

	mgr.releaseOrphanedForfeits(t.Context())

	_, ok := actorState(t, mgr, orphanA.Outpoint).(*LiveState)
	require.True(t, ok, "orphan A must be released to Live")

	_, ok = actorState(t, mgr, orphanB.Outpoint).(*LiveState)
	require.True(t, ok, "orphan B must be released to Live")

	// The cached liveDescriptors snapshot must also be refreshed to Live,
	// so downstream consumers (e.g. the fraud-watch restore) do not skip
	// the now-live VTXOs on a stale PendingForfeit status.
	statusA, foundA := snapshotStatus(mgr, orphanA.Outpoint)
	require.True(t, foundA)
	require.Equal(t, VTXOStatusLive, statusA, "snapshot A must read Live")

	statusB, foundB := snapshotStatus(mgr, orphanB.Outpoint)
	require.True(t, foundB)
	require.Equal(t, VTXOStatusLive, statusB, "snapshot B must read Live")

	store.AssertExpectations(t)
}

// TestReleaseOrphanedForfeitsListErrorIsNoop verifies that a failure listing
// PendingForfeit VTXOs aborts the sweep without touching any actor, so a
// transient store error cannot mis-handle a reservation.
func TestReleaseOrphanedForfeitsListErrorIsNoop(t *testing.T) {
	t.Parallel()

	orphan := makeDescriptor(t, 50000, 0)

	mgr, store := newPendingForfeitManager(t, []*Descriptor{orphan})

	store.On(
		"ListVTXOsByStatus", t.Context(), VTXOStatusPendingForfeit,
	).Return([]*Descriptor(nil), errors.New("store unreadable"))

	mgr.releaseOrphanedForfeits(t.Context())

	_, ok := actorState(t, mgr, orphan.Outpoint).(*PendingForfeitState)
	require.True(t, ok, "VTXO must remain PendingForfeit on list error")

	store.AssertExpectations(t)
}

// TestReleaseOrphanedForfeitsSkipsMissingActor verifies that a persisted
// PendingForfeit descriptor with no recovered actor is skipped without panic
// and without disturbing the actors that are present.
func TestReleaseOrphanedForfeitsSkipsMissingActor(t *testing.T) {
	t.Parallel()

	tracked := makeDescriptor(t, 50000, 0)
	missing := makeDescriptor(t, 40000, 1)

	// Only `tracked` gets an actor; `missing` is returned by the store but
	// is absent from the actor map.
	mgr, store := newPendingForfeitManager(t, []*Descriptor{tracked})

	store.On(
		"ListVTXOsByStatus", t.Context(), VTXOStatusPendingForfeit,
	).Return([]*Descriptor{tracked, missing}, nil)

	mgr.releaseOrphanedForfeits(t.Context())

	// The tracked VTXO is still released to Live; the missing one is simply
	// skipped.
	_, ok := actorState(t, mgr, tracked.Outpoint).(*LiveState)
	require.True(t, ok, "tracked VTXO must be released to Live")

	_, present := mgr.actors[missing.Outpoint]
	require.False(t, present, "missing VTXO must not gain an actor")

	store.AssertExpectations(t)
}
