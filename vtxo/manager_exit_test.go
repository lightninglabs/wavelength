package vtxo

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// newExitTestManager builds a Manager holding a single mock VTXO actor in the
// given state, keyed by the descriptor's outpoint. It mirrors newTestManager
// but lets the caller pick the actor's starting state (newTestManager always
// starts actors in LiveState).
func newExitTestManager(t *testing.T, vtxo *Descriptor, state VTXOState) (
	*Manager, *MockVTXOStore, *mockVTXOActorRef) {

	t.Helper()

	store := &MockVTXOStore{}
	ref := newMockVTXOActorRef(vtxo.Outpoint.String(), state)
	mgr := &Manager{
		cfg: &ManagerConfig{
			Store:      store,
			RoundActor: newMockRoundActorRef(t),
		},
		actors: map[wire.OutPoint]VTXOActorRef{
			vtxo.Outpoint: ref,
		},
	}

	return mgr, store, ref
}

// TestHandleExitOutcomeRecoverableDrivesActorToLive verifies that a
// recoverable exit outcome drives the live actor's FSM back to LiveState.
func TestHandleExitOutcomeRecoverableDrivesActorToLive(t *testing.T) {
	t.Parallel()

	vtxo := makeDescriptor(t, 50_000, 0)
	mgr, _, ref := newExitTestManager(t, vtxo, &UnilateralExitState{
		VTXO:              vtxo,
		Reason:            "manual unroll",
		LastCheckedHeight: 100,
	})

	resp := mgr.Receive(t.Context(), &ExitOutcomeNotification{
		Outpoint: vtxo.Outpoint,
		Outcome:  ExitOutcomeRecoverable,
		Reason:   "min relay fee not met",
	})
	_, err := resp.Unpack()
	require.NoError(t, err)

	require.IsType(
		t, &LiveState{}, ref.state,
		"recoverable outcome should roll the actor back to live",
	)
}

// TestHandleExitOutcomeConfirmedDrivesActorToSpent verifies that a confirmed
// exit outcome drives the live actor's FSM to the terminal SpentState.
func TestHandleExitOutcomeConfirmedDrivesActorToSpent(t *testing.T) {
	t.Parallel()

	vtxo := makeDescriptor(t, 50_000, 1)
	mgr, _, ref := newExitTestManager(t, vtxo, &UnilateralExitState{
		VTXO:   vtxo,
		Reason: "manual unroll",
	})

	resp := mgr.Receive(t.Context(), &ExitOutcomeNotification{
		Outpoint: vtxo.Outpoint,
		Outcome:  ExitOutcomeConfirmed,
	})
	_, err := resp.Unpack()
	require.NoError(t, err)

	require.IsType(
		t, &SpentState{}, ref.state,
		"confirmed outcome should retire the actor to spent",
	)
}

// TestHandleExitOutcomeRecoverableHoldsRecoveryOnlyTarget verifies that a
// recoverable exit failure does NOT relive a recovery-only target (a
// non-standard exit policy, e.g. a vHTLC refund) into the live coin set: the
// live actor stays in UnilateralExitState. Reliving it would turn a
// swap-contract output into spendable wallet balance and re-poison sweep-all.
func TestHandleExitOutcomeRecoverableHoldsRecoveryOnlyTarget(t *testing.T) {
	t.Parallel()

	vtxo := makeDescriptor(t, 1_799, 6)
	mgr, _, ref := newExitTestManager(t, vtxo, &UnilateralExitState{
		VTXO:   vtxo,
		Reason: "vhtlc recovery",
	})

	resp := mgr.Receive(t.Context(), &ExitOutcomeNotification{
		Outpoint:       vtxo.Outpoint,
		Outcome:        ExitOutcomeRecoverable,
		Reason:         "min relay fee not met",
		ExitPolicyKind: actormsg.ExitPolicyVHTLCRefundWithoutReceiver,
	})
	_, err := resp.Unpack()
	require.NoError(t, err)

	require.IsType(
		t, &UnilateralExitState{}, ref.state,
		"recovery-only target must not be relived to live",
	)
}

// TestHandleExitOutcomeRecoverableNoActorHoldsRecoveryOnlyTarget verifies the
// store-fallback path also holds a recovery-only target in exit: with no live
// actor, the recoverable outcome must not load the descriptor or write a Live
// status. The guard short-circuits before any store access.
func TestHandleExitOutcomeRecoverableNoActorHoldsRecoveryOnlyTarget(
	t *testing.T) {

	t.Parallel()

	vtxo := makeDescriptor(t, 1_799, 7)
	vtxo.Status = VTXOStatusUnilateralExit
	store := &MockVTXOStore{}
	mgr := &Manager{
		cfg: &ManagerConfig{
			Store: store,
		},
		actors: make(map[wire.OutPoint]VTXOActorRef),
	}

	// No GetVTXO / UpdateVTXOStatus expectations: the recovery-only guard
	// returns before touching the store.
	resp := mgr.Receive(t.Context(), &ExitOutcomeNotification{
		Outpoint:       vtxo.Outpoint,
		Outcome:        ExitOutcomeRecoverable,
		ExitPolicyKind: actormsg.ExitPolicyVHTLCRefundWithoutReceiver,
	})
	_, err := resp.Unpack()
	require.NoError(t, err)

	store.AssertExpectations(t)
	store.AssertNotCalled(t, "GetVTXO")
	store.AssertNotCalled(t, "UpdateVTXOStatus")
}

// TestHandleExitOutcomeConfirmedNoActorPersistsSpent verifies that, with no
// live actor for the outpoint (e.g. after a restart where the exiting VTXO was
// not part of the live-recovery set), a confirmed outcome still persists the
// terminal spent status directly.
func TestHandleExitOutcomeConfirmedNoActorPersistsSpent(t *testing.T) {
	t.Parallel()

	vtxo := makeDescriptor(t, 50_000, 2)
	vtxo.Status = VTXOStatusUnilateralExit
	store := &MockVTXOStore{}
	mgr := &Manager{
		cfg: &ManagerConfig{
			Store: store,
		},
		actors: make(map[wire.OutPoint]VTXOActorRef),
	}

	// The confirm path loads the descriptor to guard against retiring a
	// VTXO that has since moved off the exit state.
	store.On("GetVTXO", t.Context(), vtxo.Outpoint).Return(vtxo, nil)
	store.On(
		"UpdateVTXOStatus", t.Context(), vtxo.Outpoint, VTXOStatusSpent,
	).Return(nil)

	resp := mgr.Receive(t.Context(), &ExitOutcomeNotification{
		Outpoint: vtxo.Outpoint,
		Outcome:  ExitOutcomeConfirmed,
	})
	_, err := resp.Unpack()
	require.NoError(t, err)

	store.AssertExpectations(t)
}

// TestHandleExitOutcomeConfirmedNoActorAlreadySpentIsNoop verifies that a
// re-delivered confirmation for a VTXO that is no longer in the exit state
// (e.g. already spent) does not rewrite its status.
func TestHandleExitOutcomeConfirmedNoActorAlreadySpentIsNoop(t *testing.T) {
	t.Parallel()

	vtxo := makeDescriptor(t, 50_000, 4)
	vtxo.Status = VTXOStatusSpent
	store := &MockVTXOStore{}
	mgr := &Manager{
		cfg: &ManagerConfig{
			Store: store,
		},
		actors: make(map[wire.OutPoint]VTXOActorRef),
	}

	// Only GetVTXO is expected; no UpdateVTXOStatus because the guard
	// short-circuits on a non-exiting VTXO.
	store.On("GetVTXO", t.Context(), vtxo.Outpoint).Return(vtxo, nil)

	resp := mgr.Receive(t.Context(), &ExitOutcomeNotification{
		Outpoint: vtxo.Outpoint,
		Outcome:  ExitOutcomeConfirmed,
	})
	_, err := resp.Unpack()
	require.NoError(t, err)

	store.AssertExpectations(t)
}

// TestHandleExitOutcomeRecoverableNoDescriptorIsNoop verifies that a
// recoverable outcome for an unknown outpoint (no live actor and no persisted
// descriptor) is a clean no-op rather than an error.
func TestHandleExitOutcomeRecoverableNoDescriptorIsNoop(t *testing.T) {
	t.Parallel()

	vtxo := makeDescriptor(t, 50_000, 3)
	store := &MockVTXOStore{}
	mgr := &Manager{
		cfg: &ManagerConfig{
			Store: store,
		},
		actors: make(map[wire.OutPoint]VTXOActorRef),
	}

	store.On("GetVTXO", t.Context(), vtxo.Outpoint).Return(nil, nil)

	resp := mgr.Receive(t.Context(), &ExitOutcomeNotification{
		Outpoint: vtxo.Outpoint,
		Outcome:  ExitOutcomeRecoverable,
	})
	_, err := resp.Unpack()
	require.NoError(t, err)

	store.AssertExpectations(t)
}

// TestManagerStartReconcilesConfirmedExit verifies that startup reconciliation
// is owned by the VTXO manager: VTXOs still persisted as unilateral-exit are
// resolved through the configured exit outcome resolver and retired to spent
// when their exit job completed.
func TestManagerStartReconcilesConfirmedExit(t *testing.T) {
	t.Parallel()

	vtxo := makeDescriptor(t, 50_000, 5)
	vtxo.Status = VTXOStatusUnilateralExit
	store := &MockVTXOStore{}
	mgr := NewManager(&ManagerConfig{
		Store: store,
		ExitOutcomeResolver: func(_ context.Context,
			outpoint wire.OutPoint) (
			fn.Option[ExitOutcomeResolution], error) {

			require.Equal(t, vtxo.Outpoint, outpoint)

			return fn.Some(ExitOutcomeResolution{
				Outcome: ExitOutcomeConfirmed,
			}), nil
		},
	})

	store.On("ListLiveVTXOs", t.Context()).Return([]*Descriptor{}, nil)
	store.On(
		"ListVTXOsByStatus", t.Context(), VTXOStatusUnilateralExit,
	).Return([]*Descriptor{vtxo}, nil)
	store.On(
		"ListVTXOsByStatus", t.Context(), VTXOStatusPendingForfeit,
	).Return([]*Descriptor{}, nil)
	store.On("GetVTXO", t.Context(), vtxo.Outpoint).Return(vtxo, nil)
	store.On(
		"UpdateVTXOStatus", t.Context(), vtxo.Outpoint, VTXOStatusSpent,
	).Return(nil)

	err := mgr.Start(t.Context(), nil)
	require.NoError(t, err)

	store.AssertExpectations(t)
}
