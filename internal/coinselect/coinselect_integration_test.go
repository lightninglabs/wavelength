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
	fn "github.com/lightningnetwork/lnd/fn/v2"
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

// stubRoundActor captures TriggerVTXOSendMsg messages forwarded by the wallet
// during in-round send flows. It satisfies the round actor's service key
// interface (RoundReceivable → RoundActorResp). A channel is used for
// synchronization since the wallet uses Tell (fire-and-forget) which is
// processed asynchronously by the actor system.
type stubRoundActor struct {
	ch chan *actormsg.TriggerVTXOSendMsg
}

// newStubRoundActor creates a stub with a buffered channel.
func newStubRoundActor() *stubRoundActor {
	return &stubRoundActor{
		ch: make(chan *actormsg.TriggerVTXOSendMsg, 10),
	}
}

// Receive captures TriggerVTXOSendMsg and returns Ok for all messages.
func (s *stubRoundActor) Receive(_ context.Context,
	msg actormsg.RoundReceivable) fn.Result[actormsg.RoundActorResp] {

	if sendMsg, ok := msg.(*actormsg.TriggerVTXOSendMsg); ok {
		s.ch <- sendMsg
	}

	return fn.Ok[actormsg.RoundActorResp](nil)
}

// waitForSend blocks until a TriggerVTXOSendMsg is received or the
// context is cancelled.
func (s *stubRoundActor) waitForSend(
	t *testing.T) *actormsg.TriggerVTXOSendMsg {

	t.Helper()

	select {
	case msg := <-s.ch:
		return msg

	case <-t.Context().Done():
		t.Fatal("timed out waiting for TriggerVTXOSendMsg")
		return nil
	}
}

// setupSendVTXOTest creates a minimal actor system with a real VTXO manager,
// a stub round actor, and a wallet actor wired together via service keys.
// Returns the wallet actor and the stub round actor for assertions.
func setupSendVTXOTest(t *testing.T,
	vtxos []*vtxo.Descriptor) (
	*wallet.Ark, *stubRoundActor) {

	t.Helper()

	system := actor.NewActorSystem()
	t.Cleanup(func() {
		//nolint:usetesting
		_ = system.Shutdown(context.Background())
	})

	// Register VTXO manager under the well-known service key.
	store := &stubVTXOStore{vtxos: vtxos}
	mgrCfg := &vtxo.ManagerConfig{Store: store}
	mgrActor := vtxo.NewManager(mgrCfg)

	mgrKey := actormsg.VTXOManagerServiceKey()
	mgrRef := mgrKey.Spawn(
		system, actormsg.VTXOManagerServiceKeyName,
		mgrActor,
	)

	mgrTellRef := actor.NewMapInputRef[
		vtxo.ManagerMsg, vtxo.ManagerMsg,
	](mgrRef, func(m vtxo.ManagerMsg) vtxo.ManagerMsg {
		return m
	})

	err := mgrActor.Start(t.Context(), mgrTellRef)
	require.NoError(t, err)

	store.setReady()

	// Register a stub round actor under the well-known service key
	// so the wallet can forward TriggerVTXOSendMsg to it.
	roundStub := newStubRoundActor()
	roundKey := actormsg.RoundActorServiceKey()
	roundKey.Spawn(
		system, actormsg.RoundActorServiceKeyName,
		roundStub,
	)

	walletActor := wallet.NewArk(
		nil, nil, nil, system, btclog.Disabled,
	)

	return walletActor, roundStub
}

// TestSendVTXOsIntegration exercises the full wallet→manager→round flow:
// the wallet selects and locks VTXOs, computes change, and forwards a
// TriggerVTXOSendMsg to the round actor with the correct forfeit
// outpoints, recipients, and change amount.
func TestSendVTXOsIntegration(t *testing.T) {
	t.Parallel()

	t.Run("sends_with_change", func(t *testing.T) {
		t.Parallel()

		vtxo1 := makeDescriptor(1, 100_000)
		vtxo2 := makeDescriptor(2, 50_000)

		w, roundStub := setupSendVTXOTest(
			t, []*vtxo.Descriptor{vtxo1, vtxo2},
		)
		ctx := t.Context()

		// Send 80k to one recipient. Largest-first selects
		// vtxo1 (100k), leaving 20k change.
		recipients := []actormsg.SendRecipient{{
			PkScript: []byte{0x51, 0x20, 0xAA},
			Amount:   80_000,
		}}

		result := w.Receive(ctx, &wallet.SendVTXOsRequest{
			Recipients:  recipients,
			TotalAmount: 80_000,
		})
		require.True(t, result.IsOk(),
			"send: %v", result.Err())

		resp, err := result.Unpack()
		require.NoError(t, err)

		//nolint:forcetypeassert
		sr := resp.(*wallet.SendVTXOsResponse)
		require.Equal(t, 1, sr.SelectedCount)
		require.Equal(t,
			btcutil.Amount(100_000), sr.TotalSelected)
		require.Equal(t,
			btcutil.Amount(20_000), sr.ChangeAmount)

		// Verify the round actor received the send message.
		sendMsg := roundStub.waitForSend(t)
		require.Len(t, sendMsg.ForfeitOutpoints, 1)
		require.Equal(t, vtxo1.Outpoint,
			sendMsg.ForfeitOutpoints[0])
		require.Len(t, sendMsg.Recipients, 1)
		require.Equal(t, int64(80_000),
			sendMsg.Recipients[0].Amount)
		require.Equal(t, int64(20_000),
			sendMsg.ChangeAmount)
	})

	t.Run("sends_exact_amount_no_change", func(t *testing.T) {
		t.Parallel()

		vtxo1 := makeDescriptor(10, 50_000)

		w, roundStub := setupSendVTXOTest(
			t, []*vtxo.Descriptor{vtxo1},
		)
		ctx := t.Context()

		recipients := []actormsg.SendRecipient{{
			PkScript: []byte{0x51, 0x20, 0xBB},
			Amount:   50_000,
		}}

		result := w.Receive(ctx, &wallet.SendVTXOsRequest{
			Recipients:  recipients,
			TotalAmount: 50_000,
		})
		require.True(t, result.IsOk(),
			"send: %v", result.Err())

		resp, err := result.Unpack()
		require.NoError(t, err)

		//nolint:forcetypeassert
		sr := resp.(*wallet.SendVTXOsResponse)
		require.Equal(t, btcutil.Amount(0), sr.ChangeAmount)

		sendMsg := roundStub.waitForSend(t)
		require.Equal(t, int64(0),
			sendMsg.ChangeAmount)
	})

	t.Run("sends_to_multiple_recipients", func(t *testing.T) {
		t.Parallel()

		vtxo1 := makeDescriptor(20, 200_000)

		w, roundStub := setupSendVTXOTest(
			t, []*vtxo.Descriptor{vtxo1},
		)
		ctx := t.Context()

		recipients := []actormsg.SendRecipient{
			{
				PkScript: []byte{0x51, 0x20, 0x01},
				Amount:   60_000,
			},
			{
				PkScript: []byte{0x51, 0x20, 0x02},
				Amount:   40_000,
			},
		}

		result := w.Receive(ctx, &wallet.SendVTXOsRequest{
			Recipients:  recipients,
			TotalAmount: 100_000,
		})
		require.True(t, result.IsOk(),
			"send: %v", result.Err())

		resp, err := result.Unpack()
		require.NoError(t, err)

		//nolint:forcetypeassert
		sr := resp.(*wallet.SendVTXOsResponse)
		require.Equal(t,
			btcutil.Amount(100_000), sr.ChangeAmount)

		sendMsg := roundStub.waitForSend(t)
		require.Len(t, sendMsg.Recipients, 2)
		require.Equal(t, int64(60_000),
			sendMsg.Recipients[0].Amount)
		require.Equal(t, int64(40_000),
			sendMsg.Recipients[1].Amount)
	})

	t.Run("insufficient_balance", func(t *testing.T) {
		t.Parallel()

		vtxo1 := makeDescriptor(30, 10_000)

		w, _ := setupSendVTXOTest(
			t, []*vtxo.Descriptor{vtxo1},
		)
		ctx := t.Context()

		recipients := []actormsg.SendRecipient{{
			PkScript: []byte{0x51, 0x20, 0xCC},
			Amount:   50_000,
		}}

		result := w.Receive(ctx, &wallet.SendVTXOsRequest{
			Recipients:  recipients,
			TotalAmount: 50_000,
		})
		require.True(t, result.IsErr())
		require.Contains(t, result.Err().Error(),
			"insufficient")
	})

	t.Run("locks_vtxos_during_send", func(t *testing.T) {
		t.Parallel()

		vtxo1 := makeDescriptor(40, 100_000)

		w, _ := setupSendVTXOTest(
			t, []*vtxo.Descriptor{vtxo1},
		)
		ctx := t.Context()

		// First send locks vtxo1.
		recipients := []actormsg.SendRecipient{{
			PkScript: []byte{0x51, 0x20, 0xDD},
			Amount:   50_000,
		}}

		result := w.Receive(ctx, &wallet.SendVTXOsRequest{
			Recipients:  recipients,
			TotalAmount: 50_000,
		})
		require.True(t, result.IsOk())

		// Second send should fail since vtxo1 is locked.
		result = w.Receive(ctx, &wallet.SendVTXOsRequest{
			Recipients:  recipients,
			TotalAmount: 50_000,
		})
		require.True(t, result.IsErr())
		require.Contains(t, result.Err().Error(),
			"insufficient")
	})
}
