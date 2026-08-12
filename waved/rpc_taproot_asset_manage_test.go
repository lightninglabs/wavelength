package waved

import (
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/stretchr/testify/require"
)

// assetDescriptor builds a minimal asset-bearing descriptor for aggregation
// tests.
func assetDescriptor(ref string, amount uint64,
	status vtxo.VTXOStatus) *vtxo.Descriptor {

	root := chainhash.Hash{1}

	return &vtxo.Descriptor{
		TaprootAssetRef:    ref,
		TaprootAssetAmount: amount,
		TaprootAssetRoot:   &root,
		Status:             status,
	}
}

// TestTaprootAssetBalances verifies the per-reference aggregation buckets
// live, pending, and exiting amounts and skips Bitcoin-only VTXOs.
func TestTaprootAssetBalances(t *testing.T) {
	t.Parallel()

	live := []*vtxo.Descriptor{
		assetDescriptor("usd", 600, vtxo.VTXOStatusLive),
		assetDescriptor("usd", 300, vtxo.VTXOStatusLive),
		assetDescriptor("usd", 100, vtxo.VTXOStatusSpending),
		assetDescriptor("eur", 50, vtxo.VTXOStatusLive),
		{
			Status: vtxo.VTXOStatusLive,
		},
	}
	exiting := []*vtxo.Descriptor{
		assetDescriptor("usd", 200, vtxo.VTXOStatusUnilateralExit),
		{
			Status: vtxo.VTXOStatusUnilateralExit,
		},
	}

	balances := taprootAssetBalances(live, exiting)
	require.Len(t, balances, 2)

	require.Equal(t, "eur", balances[0].AssetRef)
	require.EqualValues(t, 50, balances[0].LiveAmount)
	require.EqualValues(t, 1, balances[0].LiveVtxoCount)

	require.Equal(t, "usd", balances[1].AssetRef)
	require.EqualValues(t, 900, balances[1].LiveAmount)
	require.EqualValues(t, 100, balances[1].PendingAmount)
	require.EqualValues(t, 200, balances[1].ExitingAmount)
	require.EqualValues(t, 2, balances[1].LiveVtxoCount)
}

// TestFilterDescriptorsByAssetRef verifies exact-match filtering and that
// Bitcoin-only VTXOs never match.
func TestFilterDescriptorsByAssetRef(t *testing.T) {
	t.Parallel()

	descriptors := []*vtxo.Descriptor{
		assetDescriptor("usd", 600, vtxo.VTXOStatusLive),
		assetDescriptor("eur", 50, vtxo.VTXOStatusLive),
		{
			Status: vtxo.VTXOStatusLive,
		},
	}

	filtered := filterDescriptorsByAssetRef(descriptors, "usd")
	require.Len(t, filtered, 1)
	require.Equal(t, "usd", filtered[0].TaprootAssetRef)

	require.Empty(t, filterDescriptorsByAssetRef(descriptors, "gbp"))
}
