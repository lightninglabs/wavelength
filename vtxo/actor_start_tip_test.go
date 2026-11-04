package vtxo

import (
	"errors"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/lightninglabs/wavelength/chainsource"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// TestStartTipEventClassifiesExpiry verifies an answered start ask drives
// the FSM exactly like a block epoch at that height: a VTXO whose batch
// expiry has been reached leaves Live for Expired at once.
func TestStartTipEventClassifiesExpiry(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	desc := h.newTestDescriptor()

	h.store.On(
		"UpdateVTXOStatus", h.ctx, desc.Outpoint,
		VTXOStatusExpired,
	).Return(nil).Once()
	actorUnderTest := &VTXOActor{
		cfg: &VTXOActorConfig{
			VTXO:        desc,
			Store:       h.store,
			Wallet:      h.wallet,
			ChainParams: &chaincfg.RegressionNetParams,
			Manager:     newMockManagerRef(t),
		},
		state: &LiveState{
			VTXO: desc,
		},
		env: h.env,
	}

	msg := &startTipEvent{
		tip: fn.Ok[chainsource.ChainSourceResp](
			&chainsource.BestHeightResponse{
				Height: desc.BatchExpiry,
			},
		),
	}
	_, err := actorUnderTest.Receive(h.ctx, msg).Unpack()
	require.NoError(t, err)
	require.IsType(t, &ExpiredState{}, actorUnderTest.state)

	h.store.AssertExpectations(t)
}

// TestStartTipFailureKeepsState pins that a chain source which cannot
// answer the start ask costs the actor nothing: its block subscription
// still drives expiry, so it is classified one block later instead.
func TestStartTipFailureKeepsState(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	desc := h.newTestDescriptor()

	actorUnderTest := &VTXOActor{
		cfg: &VTXOActorConfig{
			VTXO:        desc,
			Store:       h.store,
			Wallet:      h.wallet,
			ChainParams: &chaincfg.RegressionNetParams,
			Manager:     newMockManagerRef(t),
		},
		state: &LiveState{
			VTXO: desc,
		},
		env: h.env,
	}

	msg := &startTipEvent{
		tip: fn.Err[chainsource.ChainSourceResp](
			errors.New("chain backend not ready"),
		),
	}
	response, err := actorUnderTest.Receive(h.ctx, msg).Unpack()
	require.NoError(t, err)
	require.Equal(t, VTXOActorResponse{
		PriorState: actorUnderTest.state,
		NewState:   actorUnderTest.state,
	}, response)
	require.IsType(t, &LiveState{}, actorUnderTest.state)

	// No store expectations were set, so any persisted transition fails.
	h.store.AssertExpectations(t)
}

// TestStartTipDropsStaleAnswer pins that an answer to the start ask cannot
// walk the actor back past a block it has already processed. The ask and
// the block subscription are separate delivery paths, so the subscription
// can deliver the expiry block before an answer read one block earlier.
// Applying that answer would return the VTXO to Live and persist it as
// spendable until the next block.
func TestStartTipDropsStaleAnswer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		tipOffset int32
	}{{
		name:      "older than the seen block",
		tipOffset: -1,
	}, {
		name:      "same height as the seen block",
		tipOffset: 0,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newVTXOTestHarness(t)
			desc := h.newTestDescriptor()

			// The block expires the VTXO, which is the only
			// status write this test allows.
			h.store.On(
				"UpdateVTXOStatus", h.ctx, desc.Outpoint,
				VTXOStatusExpired,
			).Return(nil).Once()
			params := &chaincfg.RegressionNetParams
			actorUnderTest := &VTXOActor{
				cfg: &VTXOActorConfig{
					VTXO:        desc,
					Store:       h.store,
					Wallet:      h.wallet,
					ChainParams: params,
					Manager:     newMockManagerRef(t),
				},
				state: &LiveState{
					VTXO: desc,
				},
				env: h.env,
			}

			_, err := actorUnderTest.Receive(
				h.ctx, &BlockEpochEvent{
					Height: desc.BatchExpiry,
				},
			).Unpack()
			require.NoError(t, err)
			require.IsType(
				t, &ExpiredState{}, actorUnderTest.state,
			)

			msg := &startTipEvent{
				tip: fn.Ok[chainsource.ChainSourceResp](
					&chainsource.BestHeightResponse{
						Height: desc.BatchExpiry +
							tc.tipOffset,
					},
				),
			}
			response, err := actorUnderTest.Receive(
				h.ctx, msg,
			).Unpack()
			require.NoError(t, err)
			require.Equal(t, VTXOActorResponse{
				PriorState: actorUnderTest.state,
				NewState:   actorUnderTest.state,
			}, response)
			require.IsType(
				t, &ExpiredState{}, actorUnderTest.state,
			)

			h.store.AssertExpectations(t)
		})
	}
}

// TestStartTipFencesLiveActorBySeenHeight pins the height fence on its own,
// on an actor that stays Live: an answer no newer than the last block it
// received is dropped, and a newer one still classifies.
func TestStartTipFencesLiveActorBySeenHeight(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	desc := h.newTestDescriptor()

	actorUnderTest := &VTXOActor{
		cfg: &VTXOActorConfig{
			VTXO:        desc,
			Store:       h.store,
			Wallet:      h.wallet,
			ChainParams: &chaincfg.RegressionNetParams,
			Manager:     newMockManagerRef(t),
		},
		state: &LiveState{
			VTXO: desc,
		},
		env: h.env,
	}

	// A block well before any refresh threshold keeps the actor Live
	// and writes nothing.
	seen := desc.CreatedHeight + 1
	_, err := actorUnderTest.Receive(
		h.ctx, &BlockEpochEvent{
			Height: seen,
		},
	).Unpack()
	require.NoError(t, err)
	require.IsType(t, &LiveState{}, actorUnderTest.state)

	answer := func(height int32) *startTipEvent {
		return &startTipEvent{
			tip: fn.Ok[chainsource.ChainSourceResp](
				&chainsource.BestHeightResponse{
					Height: height,
				},
			),
		}
	}

	for _, height := range []int32{seen - 1, seen} {
		_, ok := actorUnderTest.startTipEpoch(h.ctx, answer(height))
		require.False(t, ok, "tip at %d after block %d", height, seen)
	}

	epoch, ok := actorUnderTest.startTipEpoch(h.ctx, answer(seen+1))
	require.True(t, ok)
	require.Equal(t, seen+1, epoch.Height)

	h.store.AssertExpectations(t)
}

// TestStartTipIgnoredOutsideLive pins that the start tip only classifies.
// An actor spawned in Expired, as a conflicted exit is, would otherwise
// start its reclaim the moment it is spawned instead of on the next block,
// and a reserved actor could escalate to an exit.
func TestStartTipIgnoredOutsideLive(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	desc := h.newTestDescriptor()
	desc.Status = VTXOStatusExpired

	actorUnderTest := &VTXOActor{
		cfg: &VTXOActorConfig{
			VTXO:        desc,
			Store:       h.store,
			Wallet:      h.wallet,
			ChainParams: &chaincfg.RegressionNetParams,
			Manager:     newMockManagerRef(t),
		},
		state: &ExpiredState{
			VTXO: desc,
		},
		env: h.env,
	}

	msg := &startTipEvent{
		tip: fn.Ok[chainsource.ChainSourceResp](
			&chainsource.BestHeightResponse{
				Height: desc.BatchExpiry + 10,
			},
		),
	}
	response, err := actorUnderTest.Receive(h.ctx, msg).Unpack()
	require.NoError(t, err)
	require.Equal(t, VTXOActorResponse{
		PriorState: actorUnderTest.state,
		NewState:   actorUnderTest.state,
	}, response)
	require.IsType(t, &ExpiredState{}, actorUnderTest.state)

	// No store expectations were set, so a reclaim's PendingForfeit
	// write would fail the test.
	h.store.AssertExpectations(t)
}
