package vtxo

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/stretchr/testify/require"
)

// TestAutoRefreshTermsPolling bounds lookups during a long wait while checking
// that a due poll, window entry, shared update, restart or reorg stays live.
func TestAutoRefreshTermsPolling(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		window  uint32
		heights []int32
		fetches []int
		widen   bool
		shared  bool
		restart bool
		refresh bool
	}{
		{name: "late window polls every six blocks", window: 144,
			heights: []int32{
				442,
				443,
				447,
				448,
			},
			fetches: []int{
				1,
				1,
				1,
				2,
			}},
		{name: "disabled window is throttled",
			heights: []int32{
				442,
				443,
				447,
				448,
			},
			fetches: []int{
				1,
				1,
				1,
				2,
			}},
		{name: "window entry bypasses throttle", window: 556,
			heights: []int32{
				442,
				443,
				444,
			},
			fetches: []int{
				1,
				1,
				2,
			}, refresh: true},
		{name: "next poll discovers widened window", window: 144,
			heights: []int32{
				442,
				443,
				447,
				448,
			},
			fetches: []int{
				1,
				1,
				1,
				2,
			}, widen: true, refresh: true},
		{name: "shared terms update bypasses throttle", window: 144,
			heights: []int32{
				442,
				443,
			}, fetches: []int{
				1,
				2,
			},
			widen: true, shared: true, refresh: true},
		{name: "restart fetches fresh terms", window: 144,
			heights: []int32{
				442,
				443,
			}, fetches: []int{
				1,
				2,
			},
			restart: true},
		{name: "backward epoch fetches fresh terms", window: 144,
			heights: []int32{
				447,
				448,
				446,
			}, fetches: []int{
				1,
				1,
				2,
			}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			h := newVTXOTestHarness(t)
			desc := h.newTestDescriptor()
			desc.BatchExpiry = 1_000
			desc.CreatedHeight = 1
			desc.RelativeExpiry = 144
			desc.Ancestry = []Ancestry{{TreeDepth: 7}}
			cachedWindow := test.window
			remoteWindow := test.window
			cfg := DefaultExpiryConfig()
			cfg.MaxPaymentCLTV = 300
			cfg.FreeRefreshWindow = func() uint32 {
				return cachedWindow
			}
			h.withExpiryConfig(cfg)
			if test.refresh {
				h.store.
					On(
						"UpdateVTXOStatus",
						h.ctx,
						desc.Outpoint,
						VTXOStatusPendingForfeit,
					).
					Return(nil).
					Once()
			}
			manager := newMockManagerRef(t)
			var fetches int
			fetch := func(context.Context) (*btcec.PublicKey,
				error) {

				fetches++
				cachedWindow = remoteWindow

				return desc.OperatorKey, nil
			}
			actor := newRefreshTestActor(h, desc, manager, fetch)
			for i, height := range test.heights {
				if i == 1 && test.widen {
					remoteWindow = 648
					if test.shared {
						cachedWindow = remoteWindow
					}
				}
				if i == 1 && test.restart {
					actor = newRefreshTestActor(
						h, desc, manager, fetch,
					)
				}
				_, err := actor.Receive(
					h.ctx, h.newBlockEpochEvent(height),
				).Unpack()
				require.NoError(t, err)
				require.Equal(t, test.fetches[i], fetches)
			}
			if test.refresh {
				require.IsType(
					t, &PendingForfeitState{}, actor.state,
				)
				require.Len(t, manager.getMessages(), 1)
			} else {
				require.IsType(t, &LiveState{}, actor.state)
				require.Empty(t, manager.getMessages())
			}
			h.store.AssertExpectations(t)
		})
	}
}

// TestTermsPollingDoesNotDelayFundedExit verifies the exit assessment runs on
// every critical epoch even while terms lookups wait for their next poll.
func TestTermsPollingDoesNotDelayFundedExit(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	desc := h.newTestDescriptor()
	desc.BatchExpiry = 1_000
	desc.RelativeExpiry = 144
	desc.Ancestry = []Ancestry{{TreeDepth: 7}}
	cfg := DefaultExpiryConfig()
	cfg.MaxPaymentCLTV = 300
	cfg.FreeRefreshWindow = func() uint32 { return 144 }
	h.withExpiryConfig(cfg)
	h.store.On("UpdateVTXOStatus", h.ctx, desc.Outpoint,
		VTXOStatusUnilateralExit).Return(nil).Once()
	manager := newMockManagerRef(t)
	resolver := newMockChainResolverRef(t)
	var fetches, assessments int
	actor := newRefreshTestActor(h, desc, manager,
		func(context.Context) (*btcec.PublicKey, error) {
			fetches++

			return desc.OperatorKey, nil
		})
	actor.cfg.ChainResolver = resolver
	actor.cfg.CriticalExitAssessor = func(context.Context, *Descriptor) (
		CriticalExitAssessment, error) {

		assessments++

		return CriticalExitAssessment{Feasible: assessments == 3}, nil
	}
	for _, height := range []int32{814, 815, 816} {
		_, err := actor.
			Receive(h.ctx, h.newBlockEpochEvent(height)).
			Unpack()
		require.NoError(t, err)
	}
	require.Equal(t, 1, fetches)
	require.Equal(t, 3, assessments)
	require.IsType(t, &UnilateralExitState{}, actor.state)
	require.Len(t, resolver.getMessages(), 1)
	require.Empty(t, manager.getMessages())
	h.store.AssertExpectations(t)
}
