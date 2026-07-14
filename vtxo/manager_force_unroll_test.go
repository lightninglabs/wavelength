package vtxo

import (
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestHandleForceUnrollLiveActorTransitions verifies the manager drives a live
// VTXO actor into UnilateralExitState on a ForceUnrollRequest, so fraud and
// vHTLC recovery converge on the same admission gate as manual and
// critical-expiry exits.
func TestHandleForceUnrollLiveActorTransitions(t *testing.T) {
	t.Parallel()

	vtxo := makeDescriptor(t, 50_000, 10)
	mgr, _, ref := newExitTestManager(t, vtxo, &LiveState{
		VTXO:              vtxo,
		LastCheckedHeight: 100,
	})

	resp := mgr.Receive(t.Context(), &actormsg.ForceUnrollRequest{
		Outpoint: vtxo.Outpoint,
		Reason:   "recipient fraud spend",
		Trigger:  actormsg.UnrollTriggerFraudSpend,
	})
	unpacked, err := resp.Unpack()
	require.NoError(t, err)

	forceResp, ok := unpacked.(*ForceUnrollResponse)
	require.True(t, ok)
	require.True(t, forceResp.Accepted)
	require.IsType(t, &UnilateralExitState{}, ref.state)
}

// TestHandleForceUnrollAbsentActorNoDescriptor verifies that a force-unroll for
// an outpoint the wallet no longer tracks reports "no such vtxo" rather than
// spawning a phantom actor.
func TestHandleForceUnrollAbsentActorNoDescriptor(t *testing.T) {
	t.Parallel()

	vtxo := makeDescriptor(t, 50_000, 11)
	store := &MockVTXOStore{}
	mgr := &Manager{
		cfg: &ManagerConfig{
			Store: store,
		},
		actors: make(map[wire.OutPoint]VTXOActorRef),
	}

	store.On("GetVTXO", t.Context(), vtxo.Outpoint).Return(nil, nil)

	resp := mgr.Receive(t.Context(), &actormsg.ForceUnrollRequest{
		Outpoint: vtxo.Outpoint,
		Trigger:  actormsg.UnrollTriggerManual,
	})
	unpacked, err := resp.Unpack()
	require.NoError(t, err)

	forceResp, ok := unpacked.(*ForceUnrollResponse)
	require.True(t, ok)
	require.False(t, forceResp.Accepted)
	require.Equal(t, "no such vtxo", forceResp.Reason)
	store.AssertExpectations(t)
}

// TestHandleForceUnrollAbsentActorNotFoundError verifies that a force-unroll
// for an outpoint the store reports missing via ErrVTXONotFound (the production
// contract, versus a nil-descriptor mock) is a declined force-unroll rather
// than a hard internal error. A miss on the store is not our VTXO to unroll.
func TestHandleForceUnrollAbsentActorNotFoundError(t *testing.T) {
	t.Parallel()

	vtxo := makeDescriptor(t, 50_000, 13)
	store := &MockVTXOStore{}
	mgr := &Manager{
		cfg: &ManagerConfig{
			Store: store,
		},
		actors: make(map[wire.OutPoint]VTXOActorRef),
	}

	// The real store wraps sql.ErrNoRows in ErrVTXONotFound; mirror that
	// wrapping so the test exercises the same errors.Is match the manager
	// relies on, not just the bare sentinel.
	store.On("GetVTXO", t.Context(), vtxo.Outpoint).Return(
		nil, fmt.Errorf("get VTXO: %w", ErrVTXONotFound),
	)

	resp := mgr.Receive(t.Context(), &actormsg.ForceUnrollRequest{
		Outpoint: vtxo.Outpoint,
		Trigger:  actormsg.UnrollTriggerManual,
	})
	unpacked, err := resp.Unpack()
	require.NoError(t, err)

	forceResp, ok := unpacked.(*ForceUnrollResponse)
	require.True(t, ok)
	require.False(t, forceResp.Accepted)
	require.Equal(t, "no such vtxo", forceResp.Reason)
	store.AssertExpectations(t)
}

// TestHandleForceUnrollAbsentActorTerminalDescriptor verifies that a
// force-unroll for a VTXO whose persisted descriptor is already terminal
// (spent) is a reported no-op rather than respawning an actor that would
// immediately reap itself.
func TestHandleForceUnrollAbsentActorTerminalDescriptor(t *testing.T) {
	t.Parallel()

	vtxo := makeDescriptor(t, 50_000, 12)
	vtxo.Status = VTXOStatusSpent
	store := &MockVTXOStore{}
	mgr := &Manager{
		cfg: &ManagerConfig{
			Store: store,
		},
		actors: make(map[wire.OutPoint]VTXOActorRef),
	}

	store.On("GetVTXO", t.Context(), vtxo.Outpoint).Return(vtxo, nil)

	resp := mgr.Receive(t.Context(), &actormsg.ForceUnrollRequest{
		Outpoint: vtxo.Outpoint,
		ExitPolicy: fn.Some(actormsg.ExitPolicy{
			Kind: actormsg.ExitPolicyVHTLCRefundWithoutReceiver,
			Ref:  actormsg.ExitPolicyRef("recovery-12"),
		}),
	})
	unpacked, err := resp.Unpack()
	require.NoError(t, err)

	forceResp, ok := unpacked.(*ForceUnrollResponse)
	require.True(t, ok)
	require.False(t, forceResp.Accepted)
	require.Equal(t, "already terminal", forceResp.Reason)
	store.AssertExpectations(t)
}
