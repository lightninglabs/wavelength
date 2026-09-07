package vtxo

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/stretchr/testify/require"
)

// TestAutomaticRefreshCooldownSuppressesBlockRetries verifies a rejected
// maintenance round retries once after the fixed six-block delay rather than
// creating a new assembling round on every block.
func TestAutomaticRefreshCooldownSuppressesBlockRetries(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	desc := h.newTestDescriptor()
	firstHeight := desc.BatchExpiry - 200

	h.store.On(
		"UpdateVTXOStatus", h.ctx, desc.Outpoint,
		VTXOStatusPendingForfeit,
	).Return(nil).Twice()
	h.store.On(
		"UpdateVTXOStatus", h.ctx, desc.Outpoint, VTXOStatusLive,
	).Return(nil).Once()

	manager := newMockManagerRef(t)
	actor := newRefreshTestActor(h, desc, manager, nil)

	response, err := actor.Receive(
		h.ctx, h.newBlockEpochEvent(firstHeight),
	).Unpack()
	require.NoError(t, err)
	first, ok := response.(VTXOActorResponse)
	require.True(t, ok)
	require.IsType(t, &PendingForfeitState{}, first.NewState)
	require.Len(t, manager.getMessages(), 1)

	_, err = actor.Receive(h.ctx, &ForfeitReleasedEvent{}).Unpack()
	require.NoError(t, err)
	require.IsType(t, &LiveState{}, actor.state)
	require.Equal(
		t, firstHeight+autoRefreshRetryDelayBlocks,
		actor.autoRefreshRetryHeight,
	)

	for height := firstHeight + 1; height < firstHeight+
		autoRefreshRetryDelayBlocks; height++ {

		response, err = actor.Receive(
			h.ctx, h.newBlockEpochEvent(height),
		).Unpack()
		require.NoError(t, err)
		transition, ok := response.(VTXOActorResponse)
		require.True(t, ok)
		require.Same(t, transition.PriorState, transition.NewState)
		require.IsType(t, &LiveState{}, actor.state)
		require.Len(t, manager.getMessages(), 1)
	}

	_, err = actor.Receive(
		h.ctx, h.newBlockEpochEvent(
			firstHeight+autoRefreshRetryDelayBlocks,
		),
	).Unpack()
	require.NoError(t, err)
	require.IsType(t, &PendingForfeitState{}, actor.state)
	require.Len(t, manager.getMessages(), 2)
	h.store.AssertExpectations(t)
}

// TestAutomaticRefreshCooldownNeverBlocksFundedCriticalExit verifies a
// critical block inside the six-block cooldown clears it and immediately
// hands a funded VTXO to the unilateral-exit path.
func TestAutomaticRefreshCooldownNeverBlocksFundedCriticalExit(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	desc := h.newTestDescriptor()
	desc.BatchExpiry = 1_000
	desc.RelativeExpiry = 0
	firstHeight := int32(990)

	expiryCfg := &ExpiryConfig{
		RefreshThresholdBlocks:  10,
		CriticalThresholdBlocks: 5,
		TreeDepthMultiplier:     0,
	}
	h.withExpiryConfig(expiryCfg)

	h.store.On(
		"UpdateVTXOStatus", h.ctx, desc.Outpoint,
		VTXOStatusPendingForfeit,
	).Return(nil).Once()
	h.store.On(
		"UpdateVTXOStatus", h.ctx, desc.Outpoint, VTXOStatusLive,
	).Return(nil).Once()
	h.store.On(
		"UpdateVTXOStatus", h.ctx, desc.Outpoint,
		VTXOStatusUnilateralExit,
	).Return(nil).Once()

	manager := newMockManagerRef(t)
	resolver := newMockChainResolverRef(t)
	actor := newRefreshTestActor(h, desc, manager, nil)
	actor.cfg.ChainResolver = resolver
	actor.cfg.CriticalExitAssessor = func(context.Context, *Descriptor) (
		CriticalExitAssessment, error) {

		return CriticalExitAssessment{Feasible: true}, nil
	}

	_, err := actor.Receive(
		h.ctx, h.newBlockEpochEvent(firstHeight),
	).Unpack()
	require.NoError(t, err)
	_, err = actor.Receive(h.ctx, &ForfeitReleasedEvent{}).Unpack()
	require.NoError(t, err)
	require.Equal(t, int32(996), actor.autoRefreshRetryHeight)

	_, err = actor.Receive(
		h.ctx, h.newBlockEpochEvent(995),
	).Unpack()
	require.NoError(t, err)
	require.IsType(t, &UnilateralExitState{}, actor.state)
	require.Zero(t, actor.autoRefreshRetryHeight)
	require.Len(t, resolver.getMessages(), 1)
	require.Len(t, manager.getMessages(), 1)
	h.store.AssertExpectations(t)
}

// TestCriticalUnderfundedExitUsesCooperativeRefresh pins the incident shape
// from wavelength#1212: a live VTXO first observed 187 blocks before expiry
// cannot fund its unilateral package, so it stays on cooperative refresh until
// a later block observes that the wallet can execute the exit.
func TestCriticalUnderfundedExitUsesCooperativeRefresh(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	desc := h.newTestDescriptor()
	desc.BatchExpiry = 1_000
	desc.RelativeExpiry = 0

	expiryCfg := &ExpiryConfig{
		RefreshThresholdBlocks:  264,
		CriticalThresholdBlocks: 192,
		TreeDepthMultiplier:     0,
	}
	h.withExpiryConfig(expiryCfg)

	h.store.On(
		"UpdateVTXOStatus", h.ctx, desc.Outpoint,
		VTXOStatusPendingForfeit,
	).Return(nil).Once()
	h.store.On(
		"UpdateVTXOStatus", h.ctx, desc.Outpoint,
		VTXOStatusUnilateralExit,
	).Return(nil).Once()

	manager := newMockManagerRef(t)
	resolver := newMockChainResolverRef(t)
	actor := newRefreshTestActor(h, desc, manager, nil)
	actor.cfg.ChainResolver = resolver
	exitFeasible := false
	actor.cfg.CriticalExitAssessor = func(context.Context, *Descriptor) (
		CriticalExitAssessment, error) {

		return CriticalExitAssessment{
			Feasible: exitFeasible,
			Reason: "wallet_too_few_inputs: need 2 usable " +
				"fee inputs, have 0",
		}, nil
	}

	_, err := actor.Receive(
		h.ctx, h.newBlockEpochEvent(813),
	).Unpack()
	require.NoError(t, err)
	pending, ok := actor.state.(*PendingForfeitState)
	require.True(t, ok)
	require.Equal(t, int32(813), pending.RequestedAtHeight)
	require.Len(t, manager.getMessages(), 1)
	require.Empty(t, resolver.getMessages())

	_, err = actor.Receive(
		h.ctx, h.newBlockEpochEvent(814),
	).Unpack()
	require.NoError(t, err)
	require.IsType(t, &PendingForfeitState{}, actor.state)
	require.Len(t, manager.getMessages(), 1)
	require.Empty(t, resolver.getMessages())

	exitFeasible = true
	_, err = actor.Receive(
		h.ctx, h.newBlockEpochEvent(815),
	).Unpack()
	require.NoError(t, err)
	require.IsType(t, &UnilateralExitState{}, actor.state)
	require.Len(t, manager.getMessages(), 1)
	require.Len(t, resolver.getMessages(), 1)
	h.store.AssertExpectations(t)
}

// TestCohortRollbackGenerationRejectsStaleRelease verifies an ABA-delayed
// rollback from attempt A cannot release attempt B when the same leader
// retries.
func TestCohortRollbackGenerationRejectsStaleRelease(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	desc := h.newTestDescriptor()
	desc.BatchExpiry = 1_000
	leader := h.newTestOutpoint()

	h.store.On(
		"UpdateVTXOStatus", h.ctx, desc.Outpoint,
		VTXOStatusPendingForfeit,
	).Return(nil).Twice()
	h.store.On(
		"UpdateVTXOStatus", h.ctx, desc.Outpoint, VTXOStatusLive,
	).Return(nil).Once()

	actor := &VTXOActor{
		cfg: &VTXOActorConfig{
			VTXO:        desc,
			Store:       h.store,
			Wallet:      h.wallet,
			ChainParams: &chaincfg.RegressionNetParams,
		},
		state: &LiveState{
			VTXO: desc,
		},
		env: h.env,
	}

	_, err := actor.Receive(h.ctx, &CohortRefreshEvent{
		Height:         700,
		BatchExpiry:    desc.BatchExpiry,
		LeaderOutpoint: leader,
		Generation:     1,
	}).Unpack()
	require.NoError(t, err)
	require.IsType(t, &PendingForfeitState{}, actor.state)

	_, err = actor.Receive(h.ctx, &CohortRefreshReleaseEvent{
		LeaderOutpoint: leader,
		Generation:     1,
	}).Unpack()
	require.NoError(t, err)
	require.IsType(t, &LiveState{}, actor.state)

	_, err = actor.Receive(h.ctx, &CohortRefreshEvent{
		Height:         706,
		BatchExpiry:    desc.BatchExpiry,
		LeaderOutpoint: leader,
		Generation:     2,
	}).Unpack()
	require.NoError(t, err)
	require.IsType(t, &PendingForfeitState{}, actor.state)

	_, err = actor.Receive(h.ctx, &CohortRefreshReleaseEvent{
		LeaderOutpoint: leader,
		Generation:     1,
	}).Unpack()
	require.NoError(t, err)
	require.IsType(t, &PendingForfeitState{}, actor.state)
	require.Equal(t, uint64(2), actor.autoRefreshCohortGeneration)
	h.store.AssertExpectations(t)
}
