package coinselect

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/lib/actormsg"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/lightninglabs/darepo-client/wallet"
	"github.com/stretchr/testify/require"
)

// stubVTXOStore implements vtxo.VTXOStore with only ListLiveVTXOs
// returning real data. All other methods are no-op stubs that satisfy
// the interface but are never called during coin selection tests.
//
// The ready flag controls when VTXOs become visible. During
// Manager.Start the store returns an empty list (so no VTXO actors
// are spawned), then after calling setReady the full list is returned
// for coin selection queries.
type stubVTXOStore struct {
	vtxos []*vtxo.Descriptor
	ready bool
}

// setReady makes the test VTXOs visible to ListLiveVTXOs.
func (s *stubVTXOStore) setReady() { s.ready = true }

// ListLiveVTXOs returns the pre-configured test VTXOs only after
// setReady has been called.
func (s *stubVTXOStore) ListLiveVTXOs(context.Context) (
	[]*vtxo.Descriptor, error) {

	if !s.ready {
		return nil, nil
	}

	return s.vtxos, nil
}

// SaveVTXO is unused in coin selection tests.
func (s *stubVTXOStore) SaveVTXO(context.Context,
	*vtxo.Descriptor) error {

	return nil
}

// GetVTXO is unused in coin selection tests.
func (s *stubVTXOStore) GetVTXO(context.Context,
	wire.OutPoint) (*vtxo.Descriptor, error) {

	return nil, nil
}

// ListVTXOsByStatus is unused in coin selection tests.
func (s *stubVTXOStore) ListVTXOsByStatus(context.Context,
	vtxo.VTXOStatus) ([]*vtxo.Descriptor, error) {

	return nil, nil
}

// UpdateVTXOStatus is unused in coin selection tests.
func (s *stubVTXOStore) UpdateVTXOStatus(context.Context,
	wire.OutPoint, vtxo.VTXOStatus) error {

	return nil
}

// MarkForfeiting is unused in coin selection tests.
func (s *stubVTXOStore) MarkForfeiting(context.Context,
	wire.OutPoint, string, *wire.MsgTx) error {

	return nil
}

// GetForfeitTx is unused in coin selection tests.
func (s *stubVTXOStore) GetForfeitTx(context.Context,
	wire.OutPoint) (*wire.MsgTx, error) {

	return nil, nil
}

// MarkForfeited is unused in coin selection tests.
func (s *stubVTXOStore) MarkForfeited(context.Context,
	wire.OutPoint, chainhash.Hash) error {

	return nil
}

// DeleteVTXO is unused in coin selection tests.
func (s *stubVTXOStore) DeleteVTXO(context.Context,
	wire.OutPoint) error {

	return nil
}

// Compile-time check that stubVTXOStore satisfies the interface.
var _ vtxo.VTXOStore = (*stubVTXOStore)(nil)

// makeDescriptor creates a minimal VTXO descriptor for tests.
func makeDescriptor(idx uint32,
	amount btcutil.Amount) *vtxo.Descriptor {

	return &vtxo.Descriptor{
		Outpoint: wire.OutPoint{
			Hash:  chainhash.Hash{byte(idx)},
			Index: idx,
		},
		Amount:   amount,
		PkScript: []byte{0x51, 0x20, byte(idx)},
		Status:   vtxo.VTXOStatusLive,
	}
}

// setupCoinSelectionTest creates a minimal actor system with a real VTXO
// manager and wallet actor wired together via the well-known service key.
// The VTXO manager uses a stub store pre-loaded with the given
// descriptors. Returns the wallet actor for sending requests.
func setupCoinSelectionTest(t *testing.T,
	vtxos []*vtxo.Descriptor) *wallet.Ark {

	t.Helper()

	system := actor.NewActorSystem()
	t.Cleanup(func() {
		//nolint:usetesting
		_ = system.Shutdown(context.Background())
	})

	store := &stubVTXOStore{vtxos: vtxos}
	mgrCfg := &vtxo.ManagerConfig{
		Store: store,
	}
	mgrActor := vtxo.NewManager(mgrCfg)

	// Register under the well-known service key so the wallet
	// actor can discover it.
	mgrKey := actormsg.VTXOManagerServiceKey()
	mgrRef := mgrKey.Spawn(
		system, actormsg.VTXOManagerServiceKeyName,
		mgrActor,
	)

	// Create a TellOnlyRef for the manager's self-reference
	// (needed by Start for VTXO actor termination notifications).
	mgrTellRef := actor.NewMapInputRef[
		vtxo.ManagerMsg, vtxo.ManagerMsg,
	](mgrRef, func(m vtxo.ManagerMsg) vtxo.ManagerMsg {
		return m
	})

	err := mgrActor.Start(t.Context(), mgrTellRef)
	require.NoError(t, err)

	// Make VTXOs visible now that Start has completed without
	// trying to spawn actors for them.
	store.setReady()

	// Create a minimal wallet actor with only the actor system
	// wired in. The boarding backend, store, and chain source are
	// not used during coin selection.
	walletActor := wallet.NewArk(
		nil, nil, nil, system, btclog.Disabled,
	)

	return walletActor
}

// TestSelectAndLockIntegration exercises the full wallet→manager flow:
// service key lookup, listing available VTXOs, coin selection, and
// locking via the VTXO manager.
func TestSelectAndLockIntegration(t *testing.T) {
	t.Parallel()

	vtxo1 := makeDescriptor(1, 50_000)
	vtxo2 := makeDescriptor(2, 100_000)
	vtxo3 := makeDescriptor(3, 25_000)

	w := setupCoinSelectionTest(
		t, []*vtxo.Descriptor{vtxo1, vtxo2, vtxo3},
	)
	ctx := t.Context()

	// Select VTXOs covering 60k. Largest-first should pick vtxo2
	// (100k) alone.
	result := w.Receive(ctx, &wallet.SelectAndLockVTXOsRequest{
		TargetAmount: 60_000,
	})
	require.True(t, result.IsOk(), "select: %v", result.Err())

	resp, err := result.Unpack()
	require.NoError(t, err)

	//nolint:forcetypeassert
	sr := resp.(*wallet.SelectAndLockVTXOsResponse)
	require.Len(t, sr.SelectedVTXOs, 1)
	require.Equal(t, vtxo2.Outpoint,
		sr.SelectedVTXOs[0].Outpoint)
	require.Equal(t, btcutil.Amount(100_000), sr.TotalSelected)
}

// TestSelectExcludesLockedVTXOs verifies that a second selection does
// not return VTXOs locked by the first.
func TestSelectExcludesLockedVTXOs(t *testing.T) {
	t.Parallel()

	vtxo1 := makeDescriptor(1, 50_000)
	vtxo2 := makeDescriptor(2, 100_000)
	vtxo3 := makeDescriptor(3, 25_000)

	w := setupCoinSelectionTest(
		t, []*vtxo.Descriptor{vtxo1, vtxo2, vtxo3},
	)
	ctx := t.Context()

	// First selection locks vtxo2 (100k covers 60k).
	result := w.Receive(ctx, &wallet.SelectAndLockVTXOsRequest{
		TargetAmount: 60_000,
	})
	require.True(t, result.IsOk())

	// Second selection for 40k must skip vtxo2 (locked) and pick
	// vtxo1 (50k).
	result = w.Receive(ctx, &wallet.SelectAndLockVTXOsRequest{
		TargetAmount: 40_000,
	})
	require.True(t, result.IsOk())

	resp, err := result.Unpack()
	require.NoError(t, err)

	//nolint:forcetypeassert
	sr := resp.(*wallet.SelectAndLockVTXOsResponse)
	require.Len(t, sr.SelectedVTXOs, 1)
	require.Equal(t, vtxo1.Outpoint,
		sr.SelectedVTXOs[0].Outpoint)
}

// TestSelectInsufficientAfterLock verifies that selecting more than the
// available (unlocked) balance returns an error.
func TestSelectInsufficientAfterLock(t *testing.T) {
	t.Parallel()

	vtxo1 := makeDescriptor(1, 50_000)

	w := setupCoinSelectionTest(
		t, []*vtxo.Descriptor{vtxo1},
	)
	ctx := t.Context()

	// Lock the only VTXO.
	result := w.Receive(ctx, &wallet.SelectAndLockVTXOsRequest{
		TargetAmount: 50_000,
	})
	require.True(t, result.IsOk())

	// Nothing left — should fail.
	result = w.Receive(ctx, &wallet.SelectAndLockVTXOsRequest{
		TargetAmount: 1_000,
	})
	require.True(t, result.IsErr())
	require.Contains(t, result.Err().Error(),
		"insufficient VTXO balance")
}

// TestUnlockRestoresAvailability verifies that unlocking VTXOs makes
// them available for subsequent coin selection.
func TestUnlockRestoresAvailability(t *testing.T) {
	t.Parallel()

	vtxo1 := makeDescriptor(1, 80_000)

	w := setupCoinSelectionTest(
		t, []*vtxo.Descriptor{vtxo1},
	)
	ctx := t.Context()

	// Select and lock vtxo1.
	result := w.Receive(ctx, &wallet.SelectAndLockVTXOsRequest{
		TargetAmount: 50_000,
	})
	require.True(t, result.IsOk())

	// Unlock vtxo1.
	unlockResult := w.Receive(ctx, &wallet.UnlockVTXOsRequest{
		Outpoints: []wire.OutPoint{vtxo1.Outpoint},
	})
	require.True(t, unlockResult.IsOk())

	resp, err := unlockResult.Unpack()
	require.NoError(t, err)

	//nolint:forcetypeassert
	unlockResp := resp.(*wallet.UnlockVTXOsResponse)
	require.Equal(t, 1, unlockResp.UnlockedCount)

	// vtxo1 should be available again.
	result = w.Receive(ctx, &wallet.SelectAndLockVTXOsRequest{
		TargetAmount: 50_000,
	})
	require.True(t, result.IsOk(), "re-select: %v",
		result.Err())

	selectResp, err := result.Unpack()
	require.NoError(t, err)

	//nolint:forcetypeassert
	sr := selectResp.(*wallet.SelectAndLockVTXOsResponse)
	require.Len(t, sr.SelectedVTXOs, 1)
	require.Equal(t, vtxo1.Outpoint,
		sr.SelectedVTXOs[0].Outpoint)
}

// TestSelectWithNoActorSystem verifies the wallet returns a clear error
// when no actor system is configured.
func TestSelectWithNoActorSystem(t *testing.T) {
	t.Parallel()

	w := wallet.NewArk(nil, nil, nil, nil, btclog.Disabled)

	result := w.Receive(t.Context(),
		&wallet.SelectAndLockVTXOsRequest{TargetAmount: 1_000})

	require.True(t, result.IsErr())
	require.Contains(t, result.Err().Error(),
		"no actor system")
}
