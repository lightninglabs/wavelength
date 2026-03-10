package vtxo

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/actormsg"
	"github.com/stretchr/testify/require"
)

// makeTestDescriptor creates a minimal VTXO descriptor for manager tests.
func makeTestDescriptor(idx uint32, amount btcutil.Amount) *Descriptor {
	return &Descriptor{
		Outpoint: wire.OutPoint{
			Hash:  chainhash.Hash{byte(idx)},
			Index: idx,
		},
		Amount:   amount,
		PkScript: []byte{0x51, 0x20, byte(idx)},
		Status:   VTXOStatusLive,
	}
}

// TestManagerListAvailable verifies that ListAvailableVTXOsRequest returns
// live VTXOs excluding locked ones.
func TestManagerListAvailable(t *testing.T) {
	t.Parallel()

	vtxo1 := makeTestDescriptor(1, 50_000)
	vtxo2 := makeTestDescriptor(2, 100_000)
	vtxo3 := makeTestDescriptor(3, 25_000)

	store := &MockVTXOStore{}
	store.On(
		"ListLiveVTXOs", t.Context(),
	).Return([]*Descriptor{vtxo1, vtxo2, vtxo3}, nil)

	mgr := NewManager(&ManagerConfig{Store: store})

	ctx := t.Context()

	// All three should be available initially.
	result := mgr.Receive(ctx, &actormsg.ListAvailableVTXOsRequest{})
	require.True(t, result.IsOk())

	resp, ok := result.Unpack()
	require.NoError(t, ok)

	//nolint:forcetypeassert
	listResp := resp.(*actormsg.ListAvailableVTXOsResponse)
	require.Len(t, listResp.Available, 3)

	// Lock vtxo2.
	lockResult := mgr.Receive(ctx, &actormsg.LockVTXOsRequest{
		Outpoints: []wire.OutPoint{vtxo2.Outpoint},
	})
	require.True(t, lockResult.IsOk())

	// Now only 2 should be available.
	result = mgr.Receive(ctx, &actormsg.ListAvailableVTXOsRequest{})
	require.True(t, result.IsOk())

	resp, ok = result.Unpack()
	require.NoError(t, ok)

	//nolint:forcetypeassert
	listResp = resp.(*actormsg.ListAvailableVTXOsResponse)
	require.Len(t, listResp.Available, 2)

	// Verify vtxo2 is not in the available list.
	for _, av := range listResp.Available {
		require.NotEqual(t, vtxo2.Outpoint, av.Outpoint)
	}
}

// TestManagerLockVTXOs verifies that locking outpoints adds them to the
// locked set and reports the correct count.
func TestManagerLockVTXOs(t *testing.T) {
	t.Parallel()

	mgr := NewManager(&ManagerConfig{})
	ctx := t.Context()

	op1 := wire.OutPoint{Hash: chainhash.Hash{1}, Index: 0}
	op2 := wire.OutPoint{Hash: chainhash.Hash{2}, Index: 0}

	// Lock two outpoints.
	result := mgr.Receive(ctx, &actormsg.LockVTXOsRequest{
		Outpoints: []wire.OutPoint{op1, op2},
	})
	require.True(t, result.IsOk())

	resp, err := result.Unpack()
	require.NoError(t, err)

	//nolint:forcetypeassert
	lockResp := resp.(*actormsg.LockVTXOsResponse)
	require.Equal(t, 2, lockResp.LockedCount)

	// Locking the same outpoints again should report 0 newly locked.
	result = mgr.Receive(ctx, &actormsg.LockVTXOsRequest{
		Outpoints: []wire.OutPoint{op1},
	})
	require.True(t, result.IsOk())

	resp, err = result.Unpack()
	require.NoError(t, err)

	//nolint:forcetypeassert
	lockResp = resp.(*actormsg.LockVTXOsResponse)
	require.Equal(t, 0, lockResp.LockedCount)
}

// TestManagerSelectAndLockVTXOs verifies the manager atomically selects and
// locks VTXOs in a single request.
func TestManagerSelectAndLockVTXOs(t *testing.T) {
	t.Parallel()

	vtxo1 := makeTestDescriptor(1, 50_000)
	vtxo2 := makeTestDescriptor(2, 100_000)
	vtxo3 := makeTestDescriptor(3, 25_000)

	store := &MockVTXOStore{}
	store.On(
		"ListLiveVTXOs", t.Context(),
	).Return([]*Descriptor{vtxo1, vtxo2, vtxo3}, nil)

	mgr := NewManager(&ManagerConfig{Store: store})
	ctx := t.Context()

	result := mgr.Receive(ctx, &actormsg.SelectAndLockVTXOsRequest{
		TargetAmount: 60_000,
	})
	require.True(t, result.IsOk())

	resp, err := result.Unpack()
	require.NoError(t, err)

	//nolint:forcetypeassert
	selectResp := resp.(*actormsg.SelectAndLockVTXOsResponse)
	require.Len(t, selectResp.Selected, 1)
	require.Equal(t, vtxo2.Outpoint, selectResp.Selected[0].Outpoint)
	require.EqualValues(t, 100_000, selectResp.TotalSelected)
	require.True(t, mgr.lockedOutpoints.Contains(vtxo2.Outpoint))

	listResult := mgr.Receive(ctx, &actormsg.ListAvailableVTXOsRequest{})
	require.True(t, listResult.IsOk())

	listRespAny, err := listResult.Unpack()
	require.NoError(t, err)

	//nolint:forcetypeassert
	listResp := listRespAny.(*actormsg.ListAvailableVTXOsResponse)
	require.Len(t, listResp.Available, 2)
	for _, av := range listResp.Available {
		require.NotEqual(t, vtxo2.Outpoint, av.Outpoint)
	}
}

// TestManagerSelectAndLockRejectsZeroTarget verifies that a zero or
// negative target amount returns an error without locking anything.
func TestManagerSelectAndLockRejectsZeroTarget(t *testing.T) {
	t.Parallel()

	mgr := NewManager(&ManagerConfig{})
	ctx := t.Context()

	result := mgr.Receive(ctx, &actormsg.SelectAndLockVTXOsRequest{
		TargetAmount: 0,
	})
	require.True(t, result.IsErr())
	require.Contains(t, result.Err().Error(),
		"target amount must be positive")

	result = mgr.Receive(ctx, &actormsg.SelectAndLockVTXOsRequest{
		TargetAmount: -100,
	})
	require.True(t, result.IsErr())
	require.Contains(t, result.Err().Error(),
		"target amount must be positive")
}

// TestManagerUnlockVTXOs verifies that unlocking removes outpoints from
// the locked set.
func TestManagerUnlockVTXOs(t *testing.T) {
	t.Parallel()

	mgr := NewManager(&ManagerConfig{})
	ctx := t.Context()

	op1 := wire.OutPoint{Hash: chainhash.Hash{1}, Index: 0}
	op2 := wire.OutPoint{Hash: chainhash.Hash{2}, Index: 0}

	// Lock two, then unlock one.
	mgr.Receive(ctx, &actormsg.LockVTXOsRequest{
		Outpoints: []wire.OutPoint{op1, op2},
	})

	result := mgr.Receive(ctx, &actormsg.UnlockVTXOsRequest{
		Outpoints: []wire.OutPoint{op1},
	})
	require.True(t, result.IsOk())

	resp, err := result.Unpack()
	require.NoError(t, err)

	//nolint:forcetypeassert
	unlockResp := resp.(*actormsg.UnlockVTXOsResponse)
	require.Equal(t, 1, unlockResp.UnlockedCount)

	// op1 should be gone from locked set, op2 still there.
	require.False(t, mgr.lockedOutpoints.Contains(op1))
	require.True(t, mgr.lockedOutpoints.Contains(op2))
}

// TestManagerUnlockNonexistent verifies that unlocking an outpoint that
// was not locked reports 0 unlocked.
func TestManagerUnlockNonexistent(t *testing.T) {
	t.Parallel()

	mgr := NewManager(&ManagerConfig{})
	ctx := t.Context()

	op := wire.OutPoint{Hash: chainhash.Hash{99}, Index: 0}

	result := mgr.Receive(ctx, &actormsg.UnlockVTXOsRequest{
		Outpoints: []wire.OutPoint{op},
	})
	require.True(t, result.IsOk())

	resp, err := result.Unpack()
	require.NoError(t, err)

	//nolint:forcetypeassert
	unlockResp := resp.(*actormsg.UnlockVTXOsResponse)
	require.Equal(t, 0, unlockResp.UnlockedCount)
}

// TestManagerTerminatedUnlocks verifies that a terminated VTXO is
// automatically removed from the locked set.
func TestManagerTerminatedUnlocks(t *testing.T) {
	t.Parallel()

	mgr := NewManager(&ManagerConfig{})
	ctx := t.Context()

	op := wire.OutPoint{Hash: chainhash.Hash{1}, Index: 0}

	// Lock the outpoint.
	mgr.Receive(ctx, &actormsg.LockVTXOsRequest{
		Outpoints: []wire.OutPoint{op},
	})
	require.True(t, mgr.lockedOutpoints.Contains(op))

	// Simulate VTXO termination. We need to add a tracking entry
	// first so delete(m.actors, ...) doesn't panic.
	mgr.actors[op] = nil

	mgr.Receive(ctx, &VTXOTerminatedMsg{
		Outpoint:   op,
		FinalState: "forfeited",
		Reason:     "test",
	})

	// Should be automatically unlocked.
	require.False(t, mgr.lockedOutpoints.Contains(op))
}

// TestManagerListAvailableFields verifies that the AvailableVTXO fields
// are correctly populated from the descriptor.
func TestManagerListAvailableFields(t *testing.T) {
	t.Parallel()

	vtxo := makeTestDescriptor(7, 42_000)

	store := &MockVTXOStore{}
	store.On(
		"ListLiveVTXOs", t.Context(),
	).Return([]*Descriptor{vtxo}, nil)

	mgr := NewManager(&ManagerConfig{Store: store})
	ctx := t.Context()

	result := mgr.Receive(ctx, &actormsg.ListAvailableVTXOsRequest{})
	resp, err := result.Unpack()
	require.NoError(t, err)

	//nolint:forcetypeassert
	listResp := resp.(*actormsg.ListAvailableVTXOsResponse)
	require.Len(t, listResp.Available, 1)

	av := listResp.Available[0]
	require.Equal(t, vtxo.Outpoint, av.Outpoint)
	require.Equal(t, int64(42_000), av.Amount)
	require.Equal(t, vtxo.PkScript, av.PkScript)
}

// TestManagerGetActiveCountStillWorks verifies the pre-existing message
// still works after the ManagerResp interface change.
func TestManagerGetActiveCountStillWorks(t *testing.T) {
	t.Parallel()

	mgr := NewManager(&ManagerConfig{})
	ctx := t.Context()

	result := mgr.Receive(ctx, &GetActiveVTXOCountRequest{})
	require.True(t, result.IsOk())

	resp, err := result.Unpack()
	require.NoError(t, err)

	//nolint:forcetypeassert
	countResp := resp.(*GetActiveVTXOCountResponse)
	require.Equal(t, 0, countResp.Count)
}

// TestManagerLockedVTXOsExcludedFromNextSelection simulates two sequential
// coin selection calls. The first call locks some VTXOs; the second call
// should only see the remaining unlocked VTXOs, preventing double-spends.
func TestManagerLockedVTXOsExcludedFromNextSelection(t *testing.T) {
	t.Parallel()

	vtxo1 := makeTestDescriptor(1, 50_000)
	vtxo2 := makeTestDescriptor(2, 30_000)
	vtxo3 := makeTestDescriptor(3, 20_000)

	store := &MockVTXOStore{}
	store.On(
		"ListLiveVTXOs", t.Context(),
	).Return([]*Descriptor{vtxo1, vtxo2, vtxo3}, nil)

	mgr := NewManager(&ManagerConfig{Store: store})
	ctx := t.Context()

	// First selection: lock vtxo1 and vtxo2 (simulating a 70k send).
	lockResult := mgr.Receive(ctx, &actormsg.LockVTXOsRequest{
		Outpoints: []wire.OutPoint{
			vtxo1.Outpoint, vtxo2.Outpoint,
		},
	})
	require.True(t, lockResult.IsOk())

	// Second selection: list available should only return vtxo3.
	listResult := mgr.Receive(
		ctx, &actormsg.ListAvailableVTXOsRequest{},
	)
	resp, err := listResult.Unpack()
	require.NoError(t, err)

	//nolint:forcetypeassert
	listResp := resp.(*actormsg.ListAvailableVTXOsResponse)
	require.Len(t, listResp.Available, 1)
	require.Equal(t, vtxo3.Outpoint, listResp.Available[0].Outpoint)

	// Unlock the first selection (simulating transfer failure).
	unlockResult := mgr.Receive(ctx, &actormsg.UnlockVTXOsRequest{
		Outpoints: []wire.OutPoint{
			vtxo1.Outpoint, vtxo2.Outpoint,
		},
	})
	require.True(t, unlockResult.IsOk())

	// All three should be available again.
	listResult = mgr.Receive(
		ctx, &actormsg.ListAvailableVTXOsRequest{},
	)
	resp, err = listResult.Unpack()
	require.NoError(t, err)

	//nolint:forcetypeassert
	listResp = resp.(*actormsg.ListAvailableVTXOsResponse)
	require.Len(t, listResp.Available, 3)
}

// Compile-time check that response types satisfy ManagerResp.
var _ ManagerResp = (*actormsg.ListAvailableVTXOsResponse)(nil)
var _ ManagerResp = (*actormsg.SelectAndLockVTXOsResponse)(nil)
var _ ManagerResp = (*actormsg.LockVTXOsResponse)(nil)
var _ ManagerResp = (*actormsg.UnlockVTXOsResponse)(nil)
var _ ManagerResp = (*VTXOCreatedResp)(nil)
var _ ManagerResp = (*VTXOTerminatedResp)(nil)
var _ ManagerResp = (*GetActiveVTXOCountResponse)(nil)
