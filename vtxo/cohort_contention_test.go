package vtxo

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCohortReservationOwnership keeps expected contention distinguishable
// without weakening which automatic reservation a cohort may adopt.
func TestCohortReservationOwnership(t *testing.T) {
	for _, test := range []struct {
		name           string
		reservedHeight int32
		height         int32
		expiry         int32
		accepted       bool
	}{
		{
			"same observation",
			100,
			100,
			1000,
			true,
		},
		{
			"manual reservation",
			0,
			100,
			1000,
			false,
		},
		{
			"different height",
			100,
			101,
			1000,
			false,
		},
		{
			"different batch",
			100,
			100,
			1001,
			false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			desc := &Descriptor{BatchExpiry: 1000}
			state := &PendingForfeitState{
				VTXO:              desc,
				RequestedAtHeight: test.reservedHeight,
			}
			transition, err := state.ProcessEvent(t.Context(),
				&CohortRefreshEvent{
					Height:      test.height,
					BatchExpiry: test.expiry,
				}, nil,
			)
			if test.accepted {
				require.NoError(t, err)
				require.Same(t, state, transition.NextState)
			} else {
				require.ErrorIs(
					t, err, ErrCohortReservationNotOwned,
				)
				require.Nil(t, transition)
			}
			require.Same(t, desc, state.VTXO)
			require.Equal(
				t, test.reservedHeight, state.RequestedAtHeight,
			)
		})
	}
}

// TestCohortRefusalSurvivesActorResult proves that the manager can recognize
// an ownership refusal after Receive wraps the state-machine error.
func TestCohortRefusalSurvivesActorResult(t *testing.T) {
	h := newVTXOTestHarness(t)
	desc := h.newTestDescriptor()
	state := &PendingForfeitState{
		VTXO:              desc,
		RequestedAtHeight: 100,
	}
	child := &VTXOActor{
		cfg: &VTXOActorConfig{
			VTXO: desc,
		},
		state: state,
		env:   h.env,
	}
	_, err := child.Receive(h.ctx, &CohortRefreshEvent{
		Height:      101,
		BatchExpiry: desc.BatchExpiry,
	}).Unpack()
	require.ErrorIs(t, err, ErrCohortReservationNotOwned)
	require.ErrorContains(t, err, "process event:")
	require.Same(t, state, child.state)
	require.Same(t, desc, state.VTXO)
	require.EqualValues(t, 100, state.RequestedAtHeight)
	h.store.AssertNotCalled(t, "UpdateVTXOStatus")
	h.store.AssertNotCalled(t, "UpdateVTXOStatusReleasingReservation")
}
