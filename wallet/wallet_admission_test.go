package wallet

import (
	"context"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/baselib/actor"
	"github.com/lightninglabs/darepo-client/lib/actormsg"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// mockVTXOManagerBehavior implements actor.ActorBehavior for the VTXO manager
// service key type. It responds to admission messages with configurable
// results, enabling wallet handler tests without a real VTXO manager.
type mockVTXOManagerBehavior struct {
	// selectResp is returned for SelectAndReserveSpendRequest.
	selectResp *actormsg.SelectAndReserveSpendResponse

	// selectErr when set causes SelectAndReserveSpendRequest to fail.
	selectErr error

	// releaseResp is returned for ReleaseSpendRequest.
	releaseResp *actormsg.ReleaseSpendResponse

	// releaseErr when set causes ReleaseSpendRequest to fail.
	releaseErr error

	// completeResp is returned for CompleteSpendRequest.
	completeResp *actormsg.CompleteSpendResponse

	// completeErr when set causes CompleteSpendRequest to fail.
	completeErr error

	// forfeitReserveResp is returned for ReserveForfeitRequest.
	forfeitReserveResp *actormsg.ReserveForfeitResponse

	// forfeitReserveErr when set causes ReserveForfeitRequest to fail.
	forfeitReserveErr error

	// forfeitReleaseResp is returned for ReleaseForfeitRequest.
	forfeitReleaseResp *actormsg.ReleaseForfeitResponse

	// forfeitReleaseErr when set causes ReleaseForfeitRequest to fail.
	forfeitReleaseErr error

	// forfeitReleaseCalls tracks how many ReleaseForfeitRequest were
	// received.
	forfeitReleaseCalls int
}

// Receive processes VTXO manager messages from the wallet.
func (m *mockVTXOManagerBehavior) Receive(_ context.Context,
	msg actormsg.VTXOManagerMsg) fn.Result[actormsg.VTXOManagerResp] {

	switch msg.(type) {
	case *actormsg.SelectAndReserveSpendRequest:
		if m.selectErr != nil {
			return fn.Err[actormsg.VTXOManagerResp](
				m.selectErr,
			)
		}

		return fn.Ok[actormsg.VTXOManagerResp](m.selectResp)

	case *actormsg.ReleaseSpendRequest:
		if m.releaseErr != nil {
			return fn.Err[actormsg.VTXOManagerResp](
				m.releaseErr,
			)
		}

		return fn.Ok[actormsg.VTXOManagerResp](m.releaseResp)

	case *actormsg.CompleteSpendRequest:
		if m.completeErr != nil {
			return fn.Err[actormsg.VTXOManagerResp](
				m.completeErr,
			)
		}

		return fn.Ok[actormsg.VTXOManagerResp](m.completeResp)

	case *actormsg.ReserveForfeitRequest:
		if m.forfeitReserveErr != nil {
			return fn.Err[actormsg.VTXOManagerResp](
				m.forfeitReserveErr,
			)
		}

		return fn.Ok[actormsg.VTXOManagerResp](
			m.forfeitReserveResp,
		)

	case *actormsg.ReleaseForfeitRequest:
		m.forfeitReleaseCalls++

		if m.forfeitReleaseErr != nil {
			return fn.Err[actormsg.VTXOManagerResp](
				m.forfeitReleaseErr,
			)
		}

		return fn.Ok[actormsg.VTXOManagerResp](
			m.forfeitReleaseResp,
		)

	default:
		return fn.Err[actormsg.VTXOManagerResp](
			fmt.Errorf("unexpected message: %T", msg),
		)
	}
}

// newTestWalletWithManager creates an Ark wallet backed by a mock VTXO
// manager registered in a real actor system. Returns the wallet, the mock
// manager behavior (for configuring responses), and a cleanup function.
func newTestWalletWithManager(t *testing.T,
	mgr *mockVTXOManagerBehavior) *Ark {

	t.Helper()

	system := actor.NewActorSystem()
	t.Cleanup(func() {
		// Use Background context because t.Context() is already
		// cancelled by the time cleanup runs.
		//nolint:usetesting
		err := system.Shutdown(context.Background())
		require.NoError(t, err)
	})

	// Register the mock VTXO manager with the well-known service key.
	mgrKey := actormsg.VTXOManagerServiceKey()
	actor.RegisterWithSystem(
		system, actormsg.VTXOManagerServiceKeyName, mgrKey, mgr,
	)

	// Chain source is not needed for admission tests so we pass nil.
	return NewArk(
		nil, nil, nil, nil, system, btclog.Disabled,
	)
}

// testOutpoint returns a deterministic outpoint for testing.
func testOutpoint(idx uint32) wire.OutPoint {
	return wire.OutPoint{
		Hash:  chainhash.Hash{byte(idx), 0xaa},
		Index: idx,
	}
}

// TestSelectAndLockVTXOs verifies that the wallet forwards a spend selection
// request to the VTXO manager and translates the response correctly.
func TestSelectAndLockVTXOs(t *testing.T) {
	t.Parallel()

	mgr := &mockVTXOManagerBehavior{
		selectResp: &actormsg.SelectAndReserveSpendResponse{
			SelectedVTXOs: []actormsg.SelectedVTXO{
				{
					Outpoint: testOutpoint(0),
					Amount:   50000,
					PkScript: []byte{0x51, 0x20, 0x01},
				},
				{
					Outpoint: testOutpoint(1),
					Amount:   30000,
					PkScript: []byte{0x51, 0x20, 0x02},
				},
			},
			TotalSelected: 80000,
		},
	}
	w := newTestWalletWithManager(t, mgr)

	req := &SelectAndLockVTXOsRequest{
		TargetAmount: 70000,
	}
	result := w.Receive(t.Context(), req)
	require.True(t, result.IsOk(), "expected ok, got: %v",
		result.Err())

	respVal, _ := result.Unpack()

	//nolint:forcetypeassert
	resp := respVal.(*SelectAndLockVTXOsResponse)
	require.Len(t, resp.SelectedVTXOs, 2)
	require.Equal(t, btcutil.Amount(80000), resp.TotalSelected)
	require.Equal(t, testOutpoint(0), resp.SelectedVTXOs[0].Outpoint)
	require.Equal(t, btcutil.Amount(50000),
		resp.SelectedVTXOs[0].Amount)
}

// TestSelectAndLockVTXOsInsufficientFunds verifies that the wallet surfaces
// manager errors when selection fails.
func TestSelectAndLockVTXOsInsufficientFunds(t *testing.T) {
	t.Parallel()

	mgr := &mockVTXOManagerBehavior{
		selectErr: fmt.Errorf("insufficient funds: need 100000"),
	}
	w := newTestWalletWithManager(t, mgr)

	req := &SelectAndLockVTXOsRequest{
		TargetAmount: 100000,
	}
	result := w.Receive(t.Context(), req)
	require.True(t, result.IsErr())
	require.ErrorContains(t, result.Err(), "insufficient funds")
}

// TestUnlockVTXOs verifies that the wallet forwards a spend release request
// to the VTXO manager and translates the response.
func TestUnlockVTXOs(t *testing.T) {
	t.Parallel()

	mgr := &mockVTXOManagerBehavior{
		releaseResp: &actormsg.ReleaseSpendResponse{
			ReleasedCount: 2,
		},
	}
	w := newTestWalletWithManager(t, mgr)

	req := &UnlockVTXOsRequest{
		Outpoints: []wire.OutPoint{
			testOutpoint(0), testOutpoint(1),
		},
	}
	result := w.Receive(t.Context(), req)
	require.True(t, result.IsOk(), "expected ok, got: %v",
		result.Err())

	respVal, _ := result.Unpack()

	//nolint:forcetypeassert
	resp := respVal.(*UnlockVTXOsResponse)
	require.Equal(t, 2, resp.UnlockedCount)
}

// TestUnlockVTXOsError verifies that unlock surfaces manager errors.
func TestUnlockVTXOsError(t *testing.T) {
	t.Parallel()

	mgr := &mockVTXOManagerBehavior{
		releaseErr: fmt.Errorf("no actor for outpoint"),
	}
	w := newTestWalletWithManager(t, mgr)

	req := &UnlockVTXOsRequest{
		Outpoints: []wire.OutPoint{testOutpoint(99)},
	}
	result := w.Receive(t.Context(), req)
	require.True(t, result.IsErr())
	require.ErrorContains(t, result.Err(), "no actor for outpoint")
}

// TestCompleteSpendVTXOs verifies that the wallet forwards a spend completion
// request to the VTXO manager.
func TestCompleteSpendVTXOs(t *testing.T) {
	t.Parallel()

	mgr := &mockVTXOManagerBehavior{
		completeResp: &actormsg.CompleteSpendResponse{
			CompletedCount: 3,
		},
	}
	w := newTestWalletWithManager(t, mgr)

	req := &CompleteSpendVTXOsRequest{
		Outpoints: []wire.OutPoint{
			testOutpoint(0), testOutpoint(1), testOutpoint(2),
		},
	}
	result := w.Receive(t.Context(), req)
	require.True(t, result.IsOk(), "expected ok, got: %v",
		result.Err())

	respVal, _ := result.Unpack()

	//nolint:forcetypeassert
	resp := respVal.(*CompleteSpendVTXOsResponse)
	require.Equal(t, 3, resp.CompletedCount)
}

// TestCompleteSpendVTXOsError verifies that complete surfaces manager errors.
func TestCompleteSpendVTXOsError(t *testing.T) {
	t.Parallel()

	mgr := &mockVTXOManagerBehavior{
		completeErr: fmt.Errorf("no actor for outpoint"),
	}
	w := newTestWalletWithManager(t, mgr)

	req := &CompleteSpendVTXOsRequest{
		Outpoints: []wire.OutPoint{testOutpoint(7)},
	}
	result := w.Receive(t.Context(), req)
	require.True(t, result.IsErr())
	require.ErrorContains(t, result.Err(), "no actor for outpoint")
}

// TestSelectAndLockNoActorSystem verifies that admission handlers return an
// error when the actor system is not configured.
func TestSelectAndLockNoActorSystem(t *testing.T) {
	t.Parallel()

	w := NewArk(nil, nil, nil, nil, nil, btclog.Disabled)

	result := w.Receive(t.Context(), &SelectAndLockVTXOsRequest{
		TargetAmount: 10000,
	})
	require.True(t, result.IsErr())
	require.ErrorContains(t, result.Err(),
		"actor system not configured")
}

// =============================================================================
// Cooperative admission tests (refresh/leave gating)
// =============================================================================

// mockRoundActorBehavior implements actor.ActorBehavior for the round actor
// service key type. It responds to RegisterIntentMsg with a configurable
// error, enabling wallet handler tests without a real round actor.
type mockRoundActorBehavior struct {
	// registerErr when set causes RegisterIntentMsg to fail.
	registerErr error

	// registerCalls tracks how many times RegisterIntentMsg was received.
	registerCalls int
}

// Receive processes round actor messages from the wallet.
func (m *mockRoundActorBehavior) Receive(_ context.Context,
	msg actormsg.RoundReceivable) fn.Result[actormsg.RoundActorResp] {

	switch msg.(type) {
	case *actormsg.RegisterIntentMsg:
		m.registerCalls++

		if m.registerErr != nil {
			return fn.Err[actormsg.RoundActorResp](
				m.registerErr,
			)
		}

		return fn.Ok[actormsg.RoundActorResp](nil)

	default:
		return fn.Err[actormsg.RoundActorResp](
			fmt.Errorf("unexpected message: %T", msg),
		)
	}
}

// newTestWalletWithManagerAndRound creates a wallet with both mock VTXO
// manager and mock round actor registered. The vtxoReader provides VTXO
// descriptors for intent composition.
func newTestWalletWithManagerAndRound(t *testing.T,
	mgr *mockVTXOManagerBehavior,
	roundActor *mockRoundActorBehavior,
	vtxoReader VTXOReader) *Ark {

	t.Helper()

	system := actor.NewActorSystem()
	t.Cleanup(func() {
		// Use Background context because t.Context() is already
		// cancelled by the time cleanup runs.
		//nolint:usetesting
		err := system.Shutdown(context.Background())
		require.NoError(t, err)
	})

	// Register the mock VTXO manager.
	mgrKey := actormsg.VTXOManagerServiceKey()
	actor.RegisterWithSystem(
		system, actormsg.VTXOManagerServiceKeyName,
		mgrKey, mgr,
	)

	// Register the mock round actor.
	roundKey := actormsg.RoundActorServiceKey()
	actor.RegisterWithSystem(
		system, actormsg.RoundActorServiceKeyName,
		roundKey, roundActor,
	)

	return NewArk(
		nil, nil, vtxoReader, nil, system, btclog.Disabled,
	)
}

// testVTXOReader returns a VTXOReader backed by a static map.
func testVTXOReader(
	descs map[wire.OutPoint]*VTXODescriptor) VTXOReader {

	return VTXOReaderFunc(func(_ context.Context,
		outpoint wire.OutPoint) (*VTXODescriptor, error) {

		desc, ok := descs[outpoint]
		if !ok {
			return nil, fmt.Errorf(
				"vtxo not found: %s", outpoint,
			)
		}

		return desc, nil
	})
}

// TestRefreshReservesBeforeRoundRegistration verifies that the wallet
// reserves forfeit inputs through the VTXO manager before sending
// RegisterIntentMsg to the round actor.
func TestRefreshReservesBeforeRoundRegistration(t *testing.T) {
	t.Parallel()

	op := testOutpoint(0)
	vtxoDescs := map[wire.OutPoint]*VTXODescriptor{
		op: {
			Outpoint: op,
			Amount:   50000,
			PkScript: []byte{0x51, 0x20, 0x01},
			Expiry:   100,
			OwnerKey: keychain.KeyDescriptor{},
		},
	}

	mgr := &mockVTXOManagerBehavior{
		forfeitReserveResp: &actormsg.ReserveForfeitResponse{},
	}
	roundActor := &mockRoundActorBehavior{}

	w := newTestWalletWithManagerAndRound(
		t, mgr, roundActor, testVTXOReader(vtxoDescs),
	)

	req := &RefreshVTXOsRequest{
		TargetOutpoints: []wire.OutPoint{op},
	}
	result := w.Receive(t.Context(), req)
	require.True(t, result.IsOk(), "expected ok, got: %v",
		result.Err())

	// Round should have received the intent.
	require.Equal(t, 1, roundActor.registerCalls)
}

// TestRefreshReleasesOnRoundRejection verifies that the wallet releases
// forfeit reservations when the round actor rejects the intent.
func TestRefreshReleasesOnRoundRejection(t *testing.T) {
	t.Parallel()

	op := testOutpoint(0)
	vtxoDescs := map[wire.OutPoint]*VTXODescriptor{
		op: {
			Outpoint: op,
			Amount:   50000,
			PkScript: []byte{0x51, 0x20, 0x01},
			Expiry:   100,
			OwnerKey: keychain.KeyDescriptor{},
		},
	}

	mgr := &mockVTXOManagerBehavior{
		forfeitReserveResp: &actormsg.ReserveForfeitResponse{},
		forfeitReleaseResp: &actormsg.ReleaseForfeitResponse{
			ReleasedCount: 1,
		},
	}
	roundActor := &mockRoundActorBehavior{
		registerErr: fmt.Errorf("round full"),
	}

	w := newTestWalletWithManagerAndRound(
		t, mgr, roundActor, testVTXOReader(vtxoDescs),
	)

	req := &RefreshVTXOsRequest{
		TargetOutpoints: []wire.OutPoint{op},
	}
	result := w.Receive(t.Context(), req)
	require.True(t, result.IsErr())
	require.ErrorContains(t, result.Err(), "round full")

	// Manager should have received the release call.
	require.Equal(t, 1, mgr.forfeitReleaseCalls)
}

// TestRefreshFailsOnManagerRejection verifies that the wallet surfaces
// manager reservation errors without sending to the round actor.
func TestRefreshFailsOnManagerRejection(t *testing.T) {
	t.Parallel()

	op := testOutpoint(0)
	vtxoDescs := map[wire.OutPoint]*VTXODescriptor{
		op: {
			Outpoint: op,
			Amount:   50000,
			PkScript: []byte{0x51, 0x20, 0x01},
			Expiry:   100,
		},
	}

	mgr := &mockVTXOManagerBehavior{
		forfeitReserveErr: fmt.Errorf(
			"VTXO already spending",
		),
	}
	roundActor := &mockRoundActorBehavior{}

	w := newTestWalletWithManagerAndRound(
		t, mgr, roundActor, testVTXOReader(vtxoDescs),
	)

	req := &RefreshVTXOsRequest{
		TargetOutpoints: []wire.OutPoint{op},
	}
	result := w.Receive(t.Context(), req)
	require.True(t, result.IsErr())
	require.ErrorContains(t, result.Err(), "VTXO already spending")

	// Round should NOT have been called.
	require.Equal(t, 0, roundActor.registerCalls)
}

// TestLeaveReservesBeforeRoundRegistration verifies that the wallet
// reserves forfeit inputs before sending a leave intent to the round.
func TestLeaveReservesBeforeRoundRegistration(t *testing.T) {
	t.Parallel()

	op := testOutpoint(0)
	vtxoDescs := map[wire.OutPoint]*VTXODescriptor{
		op: {
			Outpoint: op,
			Amount:   50000,
			PkScript: []byte{0x51, 0x20, 0x01},
			Expiry:   100,
		},
	}

	mgr := &mockVTXOManagerBehavior{
		forfeitReserveResp: &actormsg.ReserveForfeitResponse{},
	}
	roundActor := &mockRoundActorBehavior{}

	w := newTestWalletWithManagerAndRound(
		t, mgr, roundActor, testVTXOReader(vtxoDescs),
	)

	req := &LeaveVTXOsRequest{
		TargetOutpoints: []wire.OutPoint{op},
		DestOutput:      &wire.TxOut{Value: 49000},
	}
	result := w.Receive(t.Context(), req)
	require.True(t, result.IsOk(), "expected ok, got: %v",
		result.Err())

	require.Equal(t, 1, roundActor.registerCalls)
}

// TestLeaveReleasesOnRoundRejection verifies that the wallet releases
// forfeit reservations when the round actor rejects the leave intent.
func TestLeaveReleasesOnRoundRejection(t *testing.T) {
	t.Parallel()

	op := testOutpoint(0)
	vtxoDescs := map[wire.OutPoint]*VTXODescriptor{
		op: {
			Outpoint: op,
			Amount:   50000,
			PkScript: []byte{0x51, 0x20, 0x01},
			Expiry:   100,
		},
	}

	mgr := &mockVTXOManagerBehavior{
		forfeitReserveResp: &actormsg.ReserveForfeitResponse{},
		forfeitReleaseResp: &actormsg.ReleaseForfeitResponse{
			ReleasedCount: 1,
		},
	}
	roundActor := &mockRoundActorBehavior{
		registerErr: fmt.Errorf("round expired"),
	}

	w := newTestWalletWithManagerAndRound(
		t, mgr, roundActor, testVTXOReader(vtxoDescs),
	)

	req := &LeaveVTXOsRequest{
		TargetOutpoints: []wire.OutPoint{op},
		DestOutput:      &wire.TxOut{Value: 49000},
	}
	result := w.Receive(t.Context(), req)
	require.True(t, result.IsErr())
	require.ErrorContains(t, result.Err(), "round expired")

	// Manager should have received the release call.
	require.Equal(t, 1, mgr.forfeitReleaseCalls)
}
