package vtxo

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/stretchr/testify/require"
)

// TestAssetVTXORejectsBitcoinOperations refuses direct admission and signing
// requests before the actor emits a reservation or a signature.
func TestAssetVTXORejectsBitcoinOperations(t *testing.T) {
	t.Parallel()
	for _, event := range []VTXOEvent{
		&SpendReserveEvent{},
		&PendingForfeitEvent{},
		&ForfeitRequestEvent{},
	} {
		h := newVTXOTestHarness(t)
		desc := h.newTestDescriptor()
		root := chainhash.Hash{1}
		desc.TaprootAssetRoot = &root
		h.withState(&LiveState{VTXO: desc})
		_, err := h.sendEvent(event)
		require.ErrorIs(t, err, ErrAssetVTXORequiresTransition)
		assertState[*LiveState](h)
		require.Empty(t, h.outboxMessages)
	}
}

// TestAssetVTXOSkipsAutomaticRefresh exercises threshold, cohort, critical
// fallback, and expired-reclaim triggers. None may request a Bitcoin round.
func TestAssetVTXOSkipsAutomaticRefresh(t *testing.T) {
	t.Parallel()
	for _, event := range []VTXOEvent{
		&BlockEpochEvent{
			Height: 850,
		},
		&CohortRefreshEvent{
			Height:      850,
			BatchExpiry: 1000,
		},
		&criticalRefreshEvent{
			Height: 990,
		},
	} {
		h := newVTXOTestHarness(t)
		desc := h.newTestDescriptor()
		root := chainhash.Hash{1}
		desc.TaprootAssetRoot = &root
		desc.BatchExpiry = 1000
		h.withExpiryConfig(&ExpiryConfig{
			RefreshThresholdBlocks:  200,
			CriticalThresholdBlocks: 50,
		})
		h.withState(&LiveState{VTXO: desc})
		_, err := h.sendEvent(event)
		require.NoError(t, err)
		assertState[*LiveState](h)
		require.Empty(t, h.outboxMessages)
	}
	h := newVTXOTestHarness(t)
	desc := h.newTestDescriptor()
	root := chainhash.Hash{1}
	desc.TaprootAssetRoot = &root
	desc.BatchExpiry = 1000
	h.withState(&ExpiredState{VTXO: desc, ObservedHeight: 1001})
	_, err := h.sendEvent(&BlockEpochEvent{Height: 1002})
	require.NoError(t, err)
	assertState[*ExpiredState](h)
	require.Empty(t, h.outboxMessages)
	_, err = h.sendEvent(&PendingForfeitEvent{})
	require.ErrorIs(t, err, ErrAssetVTXORequiresTransition)
}

// TestAssetVTXOCarrierBalance reports asset carriers in total holdings while
// excluding them from the Bitcoin spendable balance.
func TestAssetVTXOCarrierBalance(t *testing.T) {
	t.Parallel()
	root := chainhash.Hash{1}
	descs := []*Descriptor{
		{
			Amount:           1000,
			Status:           VTXOStatusLive,
			TaprootAssetRoot: &root,
		},
		{
			Amount: 2000,
			Status: VTXOStatusLive,
		},
		{
			Amount: 3000,
			Status: VTXOStatusSpending,
		},
	}
	require.EqualValues(t, 6000, SumBalance(descs))
	require.EqualValues(t, 2000, SumSpendableBalance(descs))
}

// TestAssetCriticalExpiryBypassesBitcoinFeeAssessment prevents a small
// carrier from being routed back into the unsupported Bitcoin refresh path.
func TestAssetCriticalExpiryBypassesBitcoinFeeAssessment(t *testing.T) {
	t.Parallel()
	h := newVTXOTestHarness(t)
	desc := h.newTestDescriptor()
	root := chainhash.Hash{1}
	desc.TaprootAssetRoot = &root
	manager := newMockManagerRef(t)
	a := newRefreshTestActor(h, desc, manager, nil)
	a.cfg.CriticalExitAssessor = func(context.Context, *Descriptor) (
		CriticalExitAssessment, error) {

		t.Fatal("asset exit must not assess a Bitcoin sweep")

		return CriticalExitAssessment{}, nil
	}
	event := &BlockEpochEvent{Height: desc.BatchExpiry - 1}
	require.Same(t, event, a.preflightCriticalExit(h.ctx, event))
}

// TestPendingAssetVTXORejectsForfeit protects recovered pending descriptors
// even when their reservation predates the current process.
func TestPendingAssetVTXORejectsForfeit(t *testing.T) {
	t.Parallel()
	h := newVTXOTestHarness(t)
	desc := h.newTestDescriptor()
	root := chainhash.Hash{1}
	desc.TaprootAssetRoot = &root
	h.withState(&PendingForfeitState{VTXO: desc})
	_, err := h.sendEvent(&ForfeitRequestEvent{})
	require.ErrorIs(t, err, ErrAssetVTXORequiresTransition)
	require.Empty(t, h.outboxMessages)
}
