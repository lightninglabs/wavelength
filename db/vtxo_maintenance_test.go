package db

import (
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/round"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/stretchr/testify/require"
)

// TestVTXOMaintenancePageBounds uses the same hash with several output indices,
// terminal history and a new insertion beyond the captured finite ceiling.
func TestVTXOMaintenancePageBounds(t *testing.T) {
	store, rounds, _ := newVTXOStoreForTest(t)
	ctx := t.Context()
	id := testRoundIDDB("maintenance-page")
	r := createTestRound(t, id)
	require.NoError(
		t,
		rounds.CommitState(
			ctx, r, &round.InputSigSentState{
				RoundID: r.RoundID,
			},
		),
	)
	var points []wire.OutPoint
	for i := 0; i < 5; i++ {
		desc := createTestVTXODescriptor(t, id, i+1)
		desc.Outpoint = wire.OutPoint{
			Hash: chainhash.Hash{
				0x10,
			},
			Index: uint32(i),
		}
		require.NoError(t, store.SaveVTXO(ctx, desc))
		points = append(points, desc.Outpoint)
	}
	require.NoError(
		t, store.UpdateVTXOStatus(ctx, points[1], vtxo.VTXOStatusSpent),
	)
	page, err := store.ReadMaintenancePage(ctx, nil, nil, 2)
	require.NoError(t, err)
	require.Equal(t, points[:2], page.Outpoints)
	require.True(t, page.More)
	require.Equal(t, points[4], *page.Through)
	future := createTestVTXODescriptor(t, id, 8)
	future.Outpoint.Hash = chainhash.Hash{0x20}
	require.NoError(t, store.SaveVTXO(ctx, future))
	next, err := store.ReadMaintenancePage(ctx, page.Next, page.Through, 2)
	require.NoError(t, err)
	require.Equal(t, points[2:4], next.Outpoints)
	require.True(t, next.More)
	last, err := store.ReadMaintenancePage(ctx, next.Next, next.Through, 2)
	require.NoError(t, err)
	require.Equal(t, points[4:], last.Outpoints)
	require.False(t, last.More)
	require.Equal(t, *last.Through, *last.Next)
	_, err = store.ReadMaintenancePage(
		ctx, &future.Outpoint, last.Through, 2,
	)
	require.ErrorContains(t, err, "exceeds ceiling")
}

// TestVTXOMaintenanceEmptyAndLimit rejects unbounded reads before store work.
func TestVTXOMaintenanceEmptyAndLimit(t *testing.T) {
	store, _, _ := newVTXOStoreForTest(t)
	page, err := store.ReadMaintenancePage(t.Context(), nil, nil, 1)
	require.NoError(t, err)
	require.Empty(t, page.Outpoints)
	require.Nil(t, page.Through)
	for _, limit := range []int32{0, -1, MaxVTXOMaintenancePage + 1} {
		_, err := store.ReadMaintenancePage(
			t.Context(), nil, nil, limit,
		)
		require.Error(t, err)
	}
}
