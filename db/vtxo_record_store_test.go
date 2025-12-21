package db

import (
	"testing"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo/vtxo"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

func setupVTXORecordStore(t *testing.T) (*VTXORecordStoreDB, wire.OutPoint) {
	t.Helper()

	sqlStore := NewTestDB(t)
	store := NewStore(
		sqlStore.DB, sqlStore.Queries, sqlStore.Backend(),
		btclog.Disabled, clock.NewDefaultClock(),
	)
	recordStore := store.NewVTXORecordStore()

	outpoint := wire.OutPoint{
		Hash:  testOutpointHash(t, "vtxo-record-store"),
		Index: 0,
	}

	err := recordStore.Create(t.Context(), &vtxo.Record{
		Outpoint: outpoint,
		Value:    1000,
		PkScript: append([]byte{0x51, 0x20}, make([]byte, 32)...),
		Status:   vtxo.StatusLive,
	})
	require.NoError(t, err)

	return recordStore, outpoint
}

// TestVTXORecordStoreRejectsDuplicateOutpoints ensures duplicate outpoints are
// rejected explicitly for lifecycle transitions.
func TestVTXORecordStoreRejectsDuplicateOutpoints(t *testing.T) {
	t.Parallel()

	recordStore, outpoint := setupVTXORecordStore(t)
	ctx := t.Context()

	err := recordStore.MarkInFlight(
		ctx, []wire.OutPoint{outpoint, outpoint},
		vtxo.OORLockOwner("session-1"),
	)
	require.ErrorContains(t, err, "duplicate outpoint")

	rec, err := recordStore.Get(ctx, outpoint)
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Equal(t, vtxo.StatusLive, rec.Status)

	err = recordStore.MarkSpent(ctx, []wire.OutPoint{outpoint, outpoint})
	require.ErrorContains(t, err, "duplicate outpoint")

	rec, err = recordStore.Get(ctx, outpoint)
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Equal(t, vtxo.StatusLive, rec.Status)
}
