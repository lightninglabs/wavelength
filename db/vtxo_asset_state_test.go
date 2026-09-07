package db

import (
	"bytes"
	"database/sql"
	"math"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/round"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// testAssetDescriptor creates a composed leaf with independently sized asset
// units and carrier satoshis.
func testAssetDescriptor(t *testing.T, idx int) *vtxo.Descriptor {
	t.Helper()
	desc := createTestVTXODescriptor(t, round.RoundID{1}, idx)
	root := chainhash.Hash{3}
	desc.TaprootAssetRoot = &root
	desc.TaprootAssetRef = "group:asset-test"
	desc.TaprootAssetAmount = math.MaxUint64
	desc.TaprootAssetSealedPackage = []byte{1, 2, 3}
	var err error
	desc.PkScript, err = desc.EffectivePkScript()
	require.NoError(t, err)

	return desc
}

// TestAssetVTXOStateRestart preserves the complete unsigned asset amount,
// package, and composed script through every inventory read path.
func TestAssetVTXOStateRestart(t *testing.T) {
	t.Parallel()
	store, _, _ := newVTXOStoreForTest(t)
	ctx := t.Context()
	asset := testAssetDescriptor(t, 1)
	require.NoError(t, store.SaveVTXO(ctx, asset))
	bitcoin := createTestVTXODescriptor(t, round.RoundID{1}, 2)
	require.NoError(t, store.SaveVTXO(ctx, bitcoin))

	// Reconstruct the store to discard both process-local caches.
	store = NewVTXOPersistenceStore(store.db, clock.NewDefaultClock())
	for range 2 {
		got, err := store.GetVTXO(ctx, asset.Outpoint)
		require.NoError(t, err)
		require.Equal(t, asset.TaprootAssetRoot, got.TaprootAssetRoot)
		require.Equal(t, asset.TaprootAssetRef, got.TaprootAssetRef)
		require.Equal(t, uint64(math.MaxUint64), got.TaprootAssetAmount)
		require.Equal(t, asset.Amount, got.Amount)
		require.Equal(
			t, asset.TaprootAssetSealedPackage,
			got.TaprootAssetSealedPackage,
		)
		require.Nil(t, got.TapScript)
		script, err := got.EffectivePkScript()
		require.NoError(t, err)
		require.Equal(t, asset.PkScript, script)
		got.TaprootAssetSealedPackage[0] ^= 1
	}
	live, err := store.ListLiveVTXOs(ctx)
	require.NoError(t, err)
	require.Len(t, live, 2)
	byStatus, err := store.ListVTXOsByStatus(ctx, vtxo.VTXOStatusLive)
	require.NoError(t, err)
	require.Len(t, byStatus, 2)
	candidates, err := store.ListSelectionCandidatesByStatus(
		ctx, vtxo.VTXOStatusLive,
	)
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	require.Equal(t, bitcoin.Outpoint, candidates[0].Outpoint)
}

// TestAssetVTXOReplay rejects contradictory identity and package writes while
// preserving the asset marker when an earlier minimal descriptor is replayed.
func TestAssetVTXOReplay(t *testing.T) {
	t.Parallel()
	store, _, _ := newVTXOStoreForTest(t)
	ctx := t.Context()
	asset := testAssetDescriptor(t, 1)
	minimal := *asset
	minimal.TaprootAssetRoot = nil
	minimal.TaprootAssetRef = ""
	minimal.TaprootAssetAmount = 0
	minimal.TaprootAssetSealedPackage = nil
	require.NoError(t, store.SaveVTXO(ctx, &minimal))
	_, err := store.GetVTXO(ctx, asset.Outpoint)
	require.NoError(t, err)
	require.NoError(t, store.SaveVTXO(ctx, asset))
	require.NoError(t, store.SaveVTXO(ctx, &minimal))
	got, err := store.GetVTXO(ctx, asset.Outpoint)
	require.NoError(t, err)
	require.Equal(t, asset.TaprootAssetAmount, got.TaprootAssetAmount)
	require.Nil(t, got.TapScript)
	tests := []struct {
		name   string
		mutate func(*vtxo.Descriptor)
	}{
		{
			"amount",
			func(d *vtxo.Descriptor) {
				d.TaprootAssetAmount--
			},
		},
		{
			"reference",
			func(d *vtxo.Descriptor) {
				d.TaprootAssetRef = "other"
			},
		},
		{
			"package",
			func(d *vtxo.Descriptor) {
				d.TaprootAssetSealedPackage = []byte{
					9,
				}
			},
		},
		{
			"script",
			func(d *vtxo.Descriptor) {
				d.PkScript = []byte{
					0,
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := *asset
			test.mutate(&changed)
			require.Error(t, store.SaveVTXO(ctx, &changed))
			got, err := store.GetVTXO(ctx, asset.Outpoint)
			require.NoError(t, err)
			require.Equal(
				t, asset.TaprootAssetAmount,
				got.TaprootAssetAmount,
			)
			require.Equal(
				t, asset.TaprootAssetSealedPackage,
				got.TaprootAssetSealedPackage,
			)
		})
	}
}

// TestAssetVTXOInvalidState rejects incomplete, oversized, and script-unbound
// metadata at the persistence boundary, including malformed database rows.
func TestAssetVTXOInvalidState(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*vtxo.Descriptor)
	}{
		{
			"missing root",
			func(d *vtxo.Descriptor) {
				d.TaprootAssetRoot = nil
			},
		},
		{
			"missing reference",
			func(d *vtxo.Descriptor) {
				d.TaprootAssetRef = ""
			},
		},
		{
			"zero units",
			func(d *vtxo.Descriptor) {
				d.TaprootAssetAmount = 0
			},
		},
		{
			"oversized reference",
			func(d *vtxo.Descriptor) {
				d.TaprootAssetRef = string(
					bytes.Repeat(
						[]byte{1}, 513,
					),
				)
			},
		},
		{
			"wrong script",
			func(d *vtxo.Descriptor) {
				d.PkScript = []byte{
					0,
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, _, _ := newVTXOStoreForTest(t)
			desc := testAssetDescriptor(t, 1)
			test.mutate(desc)
			require.Error(t, store.SaveVTXO(t.Context(), desc))
			_, err := store.GetVTXO(t.Context(), desc.Outpoint)
			require.Error(t, err)
		})
	}
	rows := []VTXORow{
		{
			TaprootAssetRoot: []byte{},
		},
		{
			TaprootAssetRoot: make([]byte, 31),
		},
		{
			TaprootAssetRoot: make([]byte, 32),
		},
		{
			TaprootAssetAmount: []byte{
				1,
			},
		},
		{
			TaprootAssetRef: sql.NullString{
				Valid: true,
			},
		},
		{
			TaprootAssetSealedPackage: []byte{
				1,
			},
		},
	}
	for _, row := range rows {
		_, err := decodeAssetVTXOState(row)
		require.Error(t, err)
	}
}
