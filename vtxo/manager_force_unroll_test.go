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

// TestHandleForceUnrollForfeitSignatureIssued asserts the manager reports the
// ForfeitingState suppression as a refusal rather than as an accepted exit.
//
// Once the forfeit signature has left, ForfeitingState declines a
// critical-expiry admission and self-loops. That self-loop is neither a
// terminal state nor ExpiredState, so it fell through the two no-op checks
// and the caller was told Accepted:true for a job that was never scheduled —
// the exact "uniform Accepted:true that masks work that was never scheduled"
// the Ask-not-Tell design exists to prevent.
func TestHandleForceUnrollForfeitSignatureIssued(t *testing.T) {
	t.Parallel()

	vtxo := makeDescriptor(t, 50_000, 14)

	forfeitTx := wire.NewMsgTx(2)
	mgr, _, ref := newExitTestManager(t, vtxo, &ForfeitingState{
		VTXO:        vtxo,
		NewRoundID:  "round-stalled",
		ForfeitTxID: forfeitTx.TxHash(),
		ForfeitTx:   forfeitTx,
	})

	// UnrollTriggerCriticalExpiry is the empty-string zero value, so this
	// is also what any caller that forgets to set Trigger lands on.
	resp := mgr.Receive(t.Context(), &actormsg.ForceUnrollRequest{
		Outpoint: vtxo.Outpoint,
		Trigger:  actormsg.UnrollTriggerCriticalExpiry,
	})
	unpacked, err := resp.Unpack()
	require.NoError(t, err)

	forceResp, ok := unpacked.(*ForceUnrollResponse)
	require.True(t, ok)
	require.False(t, forceResp.Accepted)
	require.Equal(
		t, "forfeit signature already issued; recover via the round "+
			"holding this VTXO", forceResp.Reason,
	)

	// Nothing was scheduled, which is the whole point: the actor stays
	// where it was.
	require.IsType(t, &ForfeitingState{}, ref.state)
}

// TestHandleForceUnrollManualFromForfeitingAccepted is the control: a manual
// trigger still escalates from ForfeitingState even with the signature
// issued, and must still report Accepted. It is the only lever left when the
// operator is unreachable and the commitment never confirms, so the refusal
// above must be scoped to the automatic trigger alone.
func TestHandleForceUnrollManualFromForfeitingAccepted(t *testing.T) {
	t.Parallel()

	vtxo := makeDescriptor(t, 50_000, 15)

	forfeitTx := wire.NewMsgTx(2)
	mgr, store, ref := newExitTestManager(t, vtxo, &ForfeitingState{
		VTXO:        vtxo,
		NewRoundID:  "round-stalled",
		ForfeitTxID: forfeitTx.TxHash(),
		ForfeitTx:   forfeitTx,
	})

	store.On(
		"UpdateVTXOStatus", t.Context(), vtxo.Outpoint,
		VTXOStatusUnilateralExit,
	).Return(nil).Maybe()

	resp := mgr.Receive(t.Context(), &actormsg.ForceUnrollRequest{
		Outpoint: vtxo.Outpoint,
		Trigger:  actormsg.UnrollTriggerManual,
	})
	unpacked, err := resp.Unpack()
	require.NoError(t, err)

	forceResp, ok := unpacked.(*ForceUnrollResponse)
	require.True(t, ok)
	require.True(t, forceResp.Accepted)
	require.IsType(t, &UnilateralExitState{}, ref.state)
}
