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

// TestAssetRefSelectionCaseInsensitive verifies that asset selection matches
// asset references regardless of hex casing.
func TestAssetRefSelectionCaseInsensitive(t *testing.T) {
	t.Parallel()

	root := chainhash.Hash{2}
	descs := []*Descriptor{{
		Amount:                    1_000,
		Status:                    VTXOStatusLive,
		TaprootAssetRoot:          &root,
		TaprootAssetRef:           "AABBCC",
		TaprootAssetAmount:        500,
		TaprootAssetSealedPackage: []byte("sealed-pkg"),
	}}

	selected, total, err := SelectAssetVTXOCandidates(
		descs, "aabbcc", 250, 4,
	)
	require.NoError(t, err)
	require.Len(t, selected, 1)
	require.EqualValues(t, 500, total)
}

// TestMaterializedNotificationSkipsTerminalStoredVTXO verifies that
// handleVTXOsMaterialized refuses to resurrect a VTXO whose stored row is
// already terminal (e.g. VTXOStatusSpent).
func TestMaterializedNotificationSkipsTerminalStoredVTXO(t *testing.T) {
	t.Parallel()

	desc := makeDescriptor(t, 1_000, 0)
	spentDesc := *desc
	spentDesc.Status = VTXOStatusSpent

	mgr, store := newTestManager(t, nil)
	store.On("GetVTXO", t.Context(), desc.Outpoint).Return(
		&spentDesc, nil,
	).Once()

	res := mgr.handleVTXOsMaterialized(
		t.Context(), &VTXOsMaterializedNotification{
			VTXOs: []*Descriptor{desc},
		},
	)
	require.NoError(t, res.Err())
	require.Empty(t, mgr.actors)
	store.AssertExpectations(t)
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

// TestAssetVTXOAllowsExplicitAssetSpendReserve verifies that an asset-aware
// OOR spend reservation transitions a live asset VTXO into SpendingState, and
// that the resulting SpendingState can complete or release normally.
func TestAssetVTXOAllowsExplicitAssetSpendReserve(t *testing.T) {
	t.Parallel()

	h := newVTXOTestHarness(t)
	desc := h.newTestDescriptor()
	root := chainhash.Hash{1}
	desc.TaprootAssetRoot = &root
	h.withState(&LiveState{VTXO: desc})

	_, err := h.sendEvent(&SpendReserveEvent{AllowAssetTransition: true})
	require.NoError(t, err)
	assertState[*SpendingState](h)
	require.Len(t, h.outboxMessages, 1)
	statusUpdate, ok := h.outboxMessages[0].(*VTXOStatusUpdate)
	require.True(t, ok)
	require.Equal(t, VTXOStatusSpending, statusUpdate.NewStatus)

	_, err = h.sendEvent(&SpendReleasedEvent{})
	require.NoError(t, err)
	assertState[*LiveState](h)

	_, err = h.sendEvent(&SpendReserveEvent{AllowAssetTransition: true})
	require.NoError(t, err)
	assertState[*SpendingState](h)

	_, err = h.sendEvent(&SpendCompletedEvent{})
	require.NoError(t, err)
	assertState[*SpentState](h)
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
