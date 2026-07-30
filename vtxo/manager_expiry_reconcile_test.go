package vtxo

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/chainsource"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// bestHeightRef returns a fixed chain tip without publishing a subscription
// epoch. It models backends where startup must explicitly query the already
// synchronized height instead of waiting for the next block.
type bestHeightRef struct {
	height int32
}

// ID returns the test actor identifier.
func (b *bestHeightRef) ID() string {
	return "best-height"
}

// Tell implements the chain source actor reference.
func (b *bestHeightRef) Tell(_ context.Context,
	_ chainsource.ChainSourceMsg) error {

	return nil
}

// Ask returns the configured best height.
func (b *bestHeightRef) Ask(_ context.Context,
	msg chainsource.ChainSourceMsg,
) actor.Future[chainsource.ChainSourceResp] {

	promise := actor.NewPromise[chainsource.ChainSourceResp]()
	if _, ok := msg.(*chainsource.BestHeightRequest); !ok {
		promise.Complete(
			fn.Err[chainsource.ChainSourceResp](
				errors.New("unexpected chain source message"),
			),
		)

		return promise.Future()
	}

	promise.Complete(
		fn.Ok[chainsource.ChainSourceResp](
			&chainsource.BestHeightResponse{
				Height: b.height,
			},
		),
	)

	return promise.Future()
}

// TestReconcileExpiryUsesCurrentTip verifies startup does not depend on a new
// block notification. A locally Live but already-expired VTXO is classified
// and then driven into the ordinary refresh reservation, while a safe VTXO is
// left untouched.
func TestReconcileExpiryUsesCurrentTip(t *testing.T) {
	t.Parallel()

	expired := makeDescriptor(t, 50_000, 0)
	safe := makeDescriptor(t, 40_000, 1)
	tip := expired.BatchExpiry
	safe.BatchExpiry = tip + 1_000

	store := &MockVTXOStore{}
	store.On(
		"ListVTXOsByStatus", t.Context(), VTXOStatusPendingForfeit,
	).Return([]*Descriptor{}, nil)

	mgr := NewManager(&ManagerConfig{
		Store:       store,
		ChainSource: &bestHeightRef{height: tip},
	})
	mgr.actors = map[wire.OutPoint]VTXOActorRef{
		expired.Outpoint: newMockVTXOActorRef(
			expired.Outpoint.String(), &LiveState{
				VTXO: expired,
			},
		),
		safe.Outpoint: newMockVTXOActorRef(
			safe.Outpoint.String(), &LiveState{
				VTXO: safe,
			},
		),
	}

	response, err := mgr.handleReconcileExpiry(t.Context()).Unpack()
	require.NoError(t, err)
	reconcileResponse, ok := response.(*ReconcileExpiryResponse)
	require.True(t, ok)
	require.Equal(t, 2, reconcileResponse.Checked)
	require.IsType(
		t, &PendingForfeitState{}, actorState(t, mgr, expired.Outpoint),
	)
	require.IsType(t, &LiveState{}, actorState(t, mgr, safe.Outpoint))
	store.AssertExpectations(t)
}

// TestReconcileExpiryContinuesRecoveredExpired verifies a VTXO already
// persisted as Expired needs only one application of the startup tip to enter
// the ordinary refresh flow.
func TestReconcileExpiryContinuesRecoveredExpired(t *testing.T) {
	t.Parallel()

	expired := makeDescriptor(t, 50_000, 0)
	tip := expired.BatchExpiry

	store := &MockVTXOStore{}
	store.On(
		"ListVTXOsByStatus", t.Context(), VTXOStatusPendingForfeit,
	).Return([]*Descriptor{}, nil)

	mgr := NewManager(&ManagerConfig{
		Store:       store,
		ChainSource: &bestHeightRef{height: tip},
	})
	mgr.actors = map[wire.OutPoint]VTXOActorRef{
		expired.Outpoint: newMockVTXOActorRef(
			expired.Outpoint.String(), &ExpiredState{
				VTXO: expired,
			},
		),
	}

	_, err := mgr.handleReconcileExpiry(t.Context()).Unpack()
	require.NoError(t, err)
	require.IsType(
		t, &PendingForfeitState{}, actorState(t, mgr, expired.Outpoint),
	)
	store.AssertExpectations(t)
}
