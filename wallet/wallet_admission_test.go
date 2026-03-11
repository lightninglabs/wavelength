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
