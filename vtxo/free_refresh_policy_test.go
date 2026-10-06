package vtxo

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/stretchr/testify/require"
)

// TestAutomaticRefreshWindowAdmission exercises live, cohort and critical
// fallback transitions with payment reserves and deeper transfer histories.
func TestAutomaticRefreshWindowAdmission(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name      string
		window    uint32
		remaining int32
		hops      int
		cohort    bool
		critical  bool
		refresh   bool
		exit      bool
		manual    bool
	}{
		{name: "late window blocks early maintenance", window: 144,
			remaining: 558},
		{name: "wide window allows maintenance", window: 648,
			remaining: 558, refresh: true},
		{name: "deep history still waits", window: 648,
			remaining: 654, hops: 16},
		{name: "deep history enters window", window: 648,
			remaining: 648, hops: 16, refresh: true},
		{name: "cohort cannot pull input outside window", window: 144,
			remaining: 558, cohort: true},
		{
			name:      "disabled window",
			remaining: 558,
		},
		{name: "unfunded critical waits for window", window: 144,
			remaining: 180, critical: true},
		{name: "unfunded critical free recovery", window: 144,
			remaining: 144, critical: true, refresh: true},
		{name: "late window does not postpone exit", window: 144,
			remaining: 186, exit: true},
		{name: "manual refresh outside window", window: 144,
			remaining: 558, manual: true, refresh: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			h := newVTXOTestHarness(t)
			desc := h.newTestDescriptor()
			desc.BatchExpiry = 3_000
			desc.CreatedHeight = 984
			desc.RelativeExpiry = 144
			desc.ChainDepth = test.hops
			desc.Ancestry = []Ancestry{{TreeDepth: 7}}
			cfg := DefaultExpiryConfig()
			cfg.MaxPaymentCLTV = 300
			cfg.FreeRefreshWindow = func() uint32 {
				return test.window
			}
			h.withExpiryConfig(cfg)
			height := desc.BatchExpiry - test.remaining
			var event VTXOEvent = &BlockEpochEvent{Height: height}
			if test.cohort {
				event = &CohortRefreshEvent{
					Height:      height,
					BatchExpiry: desc.BatchExpiry,
				}
			}
			if test.critical {
				event = &criticalRefreshEvent{Height: height}
			}
			if test.manual {
				event = &PendingForfeitEvent{}
			}
			state := &LiveState{VTXO: desc}
			transition, err := state.ProcessEvent(
				t.Context(), event, h.env,
			)
			require.NoError(t, err)
			if test.exit {
				require.IsType(
					t, &UnilateralExitState{},
					transition.NextState,
				)

				return
			}
			if test.refresh {
				require.IsType(
					t, &PendingForfeitState{},
					transition.NextState,
				)

				return
			}
			require.Same(t, state, transition.NextState)
			require.True(t, transition.NewEvents.IsNone())
		})
	}
}

// TestAutomaticRefreshObservesWidenedWindow verifies an old, late cached
// waiver does not suppress the terms lookup needed to discover a wider one.
func TestAutomaticRefreshObservesWidenedWindow(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	desc := h.newTestDescriptor()
	desc.RelativeExpiry = 144
	desc.Ancestry = []Ancestry{{TreeDepth: 7}}
	window := uint32(144)
	cfg := DefaultExpiryConfig()
	cfg.MaxPaymentCLTV = 300
	cfg.FreeRefreshWindow = func() uint32 { return window }
	h.withExpiryConfig(cfg)
	h.store.On("UpdateVTXOStatus", h.ctx, desc.Outpoint,
		VTXOStatusPendingForfeit).Return(nil).Once()
	manager := newMockManagerRef(t)
	var fetches int
	actor := newRefreshTestActor(h, desc, manager,
		func(context.Context) (*btcec.PublicKey, error) {
			fetches++
			window = 648

			return desc.OperatorKey, nil
		})
	_, err := actor.Receive(h.ctx, h.newBlockEpochEvent(
		desc.BatchExpiry-558,
	)).Unpack()
	require.NoError(t, err)
	require.Equal(t, 1, fetches)
	require.IsType(t, &PendingForfeitState{}, actor.state)
	require.Len(t, manager.getMessages(), 1)
	h.store.AssertExpectations(t)
}
