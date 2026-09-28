package vtxo

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/lib/actormsg"
	"github.com/lightninglabs/wavelength/round"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestAutomaticRefreshMinimumKeepsRecoverableState proves a below-minimum
// replacement never reserves an input or poisons a shared round. Both live
// maintenance and expired reclaim retry when the current terms permit them.
func TestAutomaticRefreshMinimumKeepsRecoverableState(t *testing.T) {
	t.Parallel()
	for _, expired := range []bool{false, true} {
		name := "live"
		if expired {
			name = "expired"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newVTXOTestHarness(t)
			desc := h.newTestDescriptor()
			desc.Amount = 488
			floor := btcutil.Amount(1000)
			expiry := h.env.ExpiryConfig
			expiry.MinRefreshAmount = func() btcutil.Amount {
				return floor
			}
			var state VTXOState = &LiveState{VTXO: desc}
			height := desc.BatchExpiry - 200
			if expired {
				state = &ExpiredState{VTXO: desc}
				height = desc.BatchExpiry
			}
			for i := range 3 {
				tr, err := state.ProcessEvent(
					h.ctx,
					h.newBlockEpochEvent(height+int32(i)),
					h.env,
				)
				require.NoError(t, err)
				require.Same(t, state, tr.NextState)
				require.True(t, tr.NewEvents.IsNone())
				require.False(t, tr.NextState.IsTerminal())
			}
			// A changed operator floor makes the same retained
			// input eligible.
			floor = desc.Amount
			tr, err := state.ProcessEvent(
				h.ctx, h.newBlockEpochEvent(height+3), h.env,
			)
			require.NoError(t, err)
			require.IsType(t, &PendingForfeitState{}, tr.NextState)
			require.True(t, tr.NewEvents.IsSome())
		})
	}
}

// TestAutomaticRefreshMinimumLeavesManualAndCriticalPaths verifies the floor
// only gates one-for-one maintenance, including cohort and unfunded-critical
// attempts. It cannot suppress manual aggregation or a viable unilateral exit.
func TestAutomaticRefreshMinimumLeavesManualAndCriticalPaths(t *testing.T) {
	t.Parallel()
	h := newVTXOTestHarness(t)
	desc := h.newTestDescriptor()
	desc.Amount = 488
	expiry := h.env.ExpiryConfig
	expiry.MinRefreshAmount = func() btcutil.Amount {
		return 1000
	}
	for _, event := range []VTXOEvent{
		&CohortRefreshEvent{
			Height:      desc.BatchExpiry - 200,
			BatchExpiry: desc.BatchExpiry,
		},
		&criticalRefreshEvent{
			Height: desc.BatchExpiry - 1,
		},
	} {
		state := &LiveState{VTXO: desc}
		tr, err := state.ProcessEvent(h.ctx, event, h.env)
		require.NoError(t, err)
		require.Same(t, state, tr.NextState)
		require.True(t, tr.NewEvents.IsNone())
	}
	for _, state := range []VTXOState{
		&LiveState{
			VTXO: desc,
		},
		&ExpiredState{
			VTXO: desc,
		},
	} {
		tr, err := state.ProcessEvent(
			h.ctx, &PendingForfeitEvent{}, h.env,
		)
		require.NoError(t, err)
		require.IsType(t, &PendingForfeitState{}, tr.NextState)
	}
	tr, err := (&LiveState{VTXO: desc}).ProcessEvent(h.ctx,
		h.newBlockEpochEvent(desc.BatchExpiry-1), h.env,
	)
	require.NoError(t, err)
	require.IsType(t, &UnilateralExitState{}, tr.NextState)
}

// TestAutomaticRefreshMinimumUnderMailboxPressure keeps an ineligible reclaim
// out of a full manager-to-round relay. After pressure drains, a valid sibling
// and then the retained small coin under updated terms both reach the round.
func TestAutomaticRefreshMinimumUnderMailboxPressure(t *testing.T) {
	t.Parallel()
	h := newVTXOTestHarness(t)
	ctx, cancel := context.WithTimeout(h.ctx, 5*time.Second)
	defer cancel()
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	delivered := make(chan *round.RefreshVTXORequest, 2)
	behavior := actor.NewFunctionBehavior(func(ctx context.Context,
		m actormsg.RoundReceivable) fn.Result[actormsg.RoundActorResp] {

		req, ok := m.(*round.RefreshVTXORequest)
		if ok {
			delivered <- req

			return fn.Ok[actormsg.RoundActorResp](nil)
		}
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-gate:
		case <-ctx.Done():
		}

		return fn.Ok[actormsg.RoundActorResp](nil)
	})
	roundActor := actor.NewActor(actor.ActorConfig[
		actormsg.RoundReceivable, actormsg.RoundActorResp,
	]{ID: "minimum-round", MailboxSize: 1, Behavior: behavior})
	roundActor.Start()
	t.Cleanup(roundActor.Stop)
	require.NoError(
		t,
		roundActor.Ref().Tell(ctx, &round.GetClientStateRequest{}),
	)
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("round did not enter")
	}
	require.NoError(
		t,
		roundActor.Ref().TryTell(ctx, &round.GetClientStateRequest{}),
	)
	manager := NewManager(&ManagerConfig{RoundActor: roundActor.Ref()})
	managerActor := actor.NewActor(actor.ActorConfig[
		ManagerMsg,
		ManagerResp,
	]{
		ID: "minimum-manager", MailboxSize: 1, Behavior: manager,
	})
	// A queued relay fills the real mailbox before the manager starts.
	require.NoError(
		t,
		managerActor.Ref().TryTell(ctx, &RelayToRoundMsg{
			Payload: &round.GetClientStateRequest{},
		},
		),
	)
	floor := btcutil.Amount(1000)
	expiry := h.env.ExpiryConfig
	expiry.MinRefreshAmount = func() btcutil.Amount {
		return floor
	}
	desc := h.newTestDescriptor()
	desc.Amount = 488
	child := newRefreshTestActor(h, desc, newMockManagerRef(t), nil)
	child.state = &ExpiredState{VTXO: desc}
	child.cfg.Manager = managerActor.TellRef()
	// The old behavior writes a reservation and parks on the full mailbox.
	h.store.
		On(
			"UpdateVTXOStatus",
			mock.Anything,
			desc.Outpoint,
			VTXOStatusPendingForfeit,
		).
		Return(nil)
	for range 3 {
		callCtx, stop := context.WithTimeout(ctx, 100*time.Millisecond)
		result := child.Receive(
			callCtx, h.newBlockEpochEvent(desc.BatchExpiry),
		)
		_, err := result.Unpack()
		stop()
		require.NoError(t, err)
		require.IsType(t, &ExpiredState{}, child.state)
	}
	h.store.AssertNotCalled(
		t, "UpdateVTXOStatus", mock.Anything, desc.Outpoint,
		VTXOStatusPendingForfeit,
	)
	close(gate)
	managerActor.Start()
	t.Cleanup(managerActor.Stop)
	healthy := h.newTestDescriptor()
	healthy.Amount = floor
	h.store.
		On(
			"UpdateVTXOStatus",
			mock.Anything,
			healthy.Outpoint,
			VTXOStatusPendingForfeit,
		).
		Return(nil).
		Once()
	peer := newRefreshTestActor(h, healthy, newMockManagerRef(t), nil)
	peer.state = &ExpiredState{VTXO: healthy}
	peer.cfg.Manager = managerActor.TellRef()
	_, err := peer.
		Receive(ctx, h.newBlockEpochEvent(healthy.BatchExpiry)).
		Unpack()
	require.NoError(t, err)
	select {
	case req := <-delivered:
		require.Equal(t, healthy.Outpoint, req.VTXOOutpoint)
		require.Equal(t, int64(1000), req.Amount)

	case <-ctx.Done():
		t.Fatal("eligible sibling did not progress")
	}
	floor = desc.Amount
	_, err = child.
		Receive(ctx, h.newBlockEpochEvent(desc.BatchExpiry+1)).
		Unpack()
	require.NoError(t, err)
	select {
	case req := <-delivered:
		require.Equal(t, desc.Outpoint, req.VTXOOutpoint)
		require.Equal(t, int64(488), req.Amount)

	case <-ctx.Done():
		t.Fatal("retained input did not recover")
	}
}

// TestAutomaticRefreshMinimumRefreshesStaleTerms proves cached minima cannot
// admit an invalid replacement or permanently suppress one that becomes valid.
// A failed terms lookup must leave the original state and durable row intact.
func TestAutomaticRefreshMinimumRefreshesStaleTerms(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"live", "expired", "critical"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newVTXOTestHarness(t)
			desc := h.newTestDescriptor()
			desc.Amount = 500
			floor := btcutil.Amount(100)
			nextFloor := btcutil.Amount(1000)
			expiry := h.env.ExpiryConfig
			expiry.MinRefreshAmount = func() btcutil.Amount {
				return floor
			}
			var fetchErr error
			fetches := 0
			manager := newMockManagerRef(t)
			fetch := func(context.Context) (*btcec.PublicKey,
				error) {

				fetches++
				if fetchErr != nil {
					return nil, fetchErr
				}
				floor = nextFloor

				return desc.OperatorKey, nil
			}
			child := newRefreshTestActor(h, desc, manager, fetch)
			height := desc.BatchExpiry - 200
			if name == "expired" {
				child.state = &ExpiredState{VTXO: desc}
				height = desc.BatchExpiry
			}
			if name == "critical" {
				height = desc.BatchExpiry - 4
				child.cfg.CriticalExitAssessor = func(
					context.Context, *Descriptor) (
					CriticalExitAssessment, error) {

					return CriticalExitAssessment{
						Feasible: false,
					}, nil
				}
			}
			original := child.state
			_, err := child.
				Receive(h.ctx, h.newBlockEpochEvent(height)).
				Unpack()
			require.NoError(t, err)
			require.Same(t, original, child.state)
			require.Empty(t, manager.getMessages())
			require.Equal(t, 1, fetches)
			fetchErr = errors.New("operator unavailable")
			_, err = child.
				Receive(h.ctx, h.newBlockEpochEvent(height+1)).
				Unpack()
			require.ErrorContains(t, err, "operator unavailable")
			require.Same(t, original, child.state)
			require.Empty(t, manager.getMessages())
			h.store.AssertNotCalled(
				t, "UpdateVTXOStatus", mock.Anything,
				desc.Outpoint, VTXOStatusPendingForfeit,
			)
			fetchErr = nil
			nextFloor = desc.Amount
			h.store.
				On(
					"UpdateVTXOStatus",
					mock.Anything,
					desc.Outpoint,
					VTXOStatusPendingForfeit,
				).
				Return(nil).
				Once()
			_, err = child.
				Receive(h.ctx, h.newBlockEpochEvent(height+2)).
				Unpack()
			require.NoError(t, err)
			require.IsType(t, &PendingForfeitState{}, child.state)
			require.Len(t, manager.getMessages(), 1)
			require.Equal(t, 3, fetches)
			h.store.AssertExpectations(t)
		})
	}
}
