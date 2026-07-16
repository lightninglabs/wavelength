package waved

import (
	"testing"

	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
)

// testExpiryStatusUnknown keeps expiry status assertions readable.
const testExpiryStatusUnknown = waverpc.
	VTXOExpiryStatus_VTXO_EXPIRY_STATUS_UNKNOWN

// TestExpiryInfoFromDescriptorUsesDefaultThresholds verifies that local VTXO
// descriptors are classified with the wallet's dynamic expiry thresholds.
func TestExpiryInfoFromDescriptorUsesDefaultThresholds(t *testing.T) {
	t.Parallel()

	desc := &vtxo.Descriptor{
		BatchExpiry:    1000,
		RelativeExpiry: 144,
		Ancestry: []vtxo.Ancestry{
			{
				TreeDepth: 1,
			},
			{
				TreeDepth: 3,
			},
		},
		ChainDepth: 2,
	}

	info := expiryInfoFromDescriptor(
		desc, 784, vtxo.DefaultExpiryConfig(),
	)

	require.Equal(
		t, waverpc.
			VTXOExpiryStatus_VTXO_EXPIRY_STATUS_NEEDS_REFRESH,
		info.GetStatus(),
	)
	require.Equal(t, int32(784), info.GetCurrentHeight())
	require.Equal(t, int32(1000), info.GetBatchExpiry())
	require.Equal(t, int32(216), info.GetBlocksRemaining())
	require.Equal(t, int32(234), info.GetRefreshThresholdBlocks())
	require.Equal(t, int32(162), info.GetCriticalThresholdBlocks())
	require.Equal(t, uint32(144), info.GetRelativeExpiry())
	require.Equal(t, uint32(3), info.GetMaxTreeDepth())
	require.Equal(t, uint32(2), info.GetChainDepth())
}

// TestExpiryInfoUsesFreeRefreshWindow verifies RPC expiry posture reports the
// same safe delayed boundary used by the live VTXO manager.
func TestExpiryInfoUsesFreeRefreshWindow(t *testing.T) {
	t.Parallel()

	cfg := vtxo.DefaultExpiryConfig()
	cfg.FreeRefreshWindow = func() uint32 {
		return 120
	}

	info := expiryInfoFromTiming(
		1_000, 24, 2, 0, 880, cfg,
	)
	require.Equal(t, int32(120), info.GetRefreshThresholdBlocks())
	require.Equal(
		t, waverpc.
			VTXOExpiryStatus_VTXO_EXPIRY_STATUS_NEEDS_REFRESH,
		info.GetStatus(),
	)
}

// TestExpiryInfoFromTimingClassifiesPostures verifies the boundary conditions
// returned to RPC callers.
func TestExpiryInfoFromTimingClassifiesPostures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		currentHeight int32
		wantStatus    waverpc.VTXOExpiryStatus
	}{{
		name:          "safe",
		currentHeight: 700,
		wantStatus: waverpc.
			VTXOExpiryStatus_VTXO_EXPIRY_STATUS_SAFE,
	}, {
		name:          "needs refresh",
		currentHeight: 784,
		wantStatus: waverpc.
			VTXOExpiryStatus_VTXO_EXPIRY_STATUS_NEEDS_REFRESH,
	}, {
		name:          "critical",
		currentHeight: 850,
		wantStatus: waverpc.
			VTXOExpiryStatus_VTXO_EXPIRY_STATUS_CRITICAL,
	}, {
		name:          "expired",
		currentHeight: 1000,
		wantStatus: waverpc.
			VTXOExpiryStatus_VTXO_EXPIRY_STATUS_EXPIRED,
	}, {
		name:          "unknown without height",
		currentHeight: 0,
		wantStatus:    testExpiryStatusUnknown,
	}}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			info := expiryInfoFromTiming(
				1000, 144, 3, 0, test.currentHeight,
				vtxo.DefaultExpiryConfig(),
			)

			require.Equal(t, test.wantStatus, info.GetStatus())
		})
	}
}

// TestExpiryInfoFromIndexedVTXOUsesMaxAncestryPathDepth verifies indexed
// pkScript lookups classify against the deepest indexer ancestry path.
func TestExpiryInfoFromIndexedVTXOUsesMaxAncestryPathDepth(t *testing.T) {
	t.Parallel()

	indexed := &arkrpc.VTXO{
		BatchExpiryHeight: 1000,
		RelativeExpiry:    24,
		ChainDepth:        5,
		AncestryPaths: []*arkrpc.AncestryPath{
			{
				TreeDepth: 2,
			},
			{
				TreeDepth: 9,
			},
		},
	}

	info := expiryInfoFromIndexedVTXO(
		indexed, 850, vtxo.DefaultExpiryConfig(),
	)

	require.Equal(
		t, waverpc.
			VTXOExpiryStatus_VTXO_EXPIRY_STATUS_NEEDS_REFRESH,
		info.GetStatus(),
	)
	require.Equal(t, int32(150), info.GetBlocksRemaining())
	require.Equal(t, int32(150), info.GetRefreshThresholdBlocks())
	require.Equal(t, int32(78), info.GetCriticalThresholdBlocks())
	require.Equal(t, uint32(24), info.GetRelativeExpiry())
	require.Equal(t, uint32(9), info.GetMaxTreeDepth())
	require.Equal(t, uint32(5), info.GetChainDepth())
}

// TestIndexedExpiryStatusFilterDefaultsToLive verifies pkScript expiry lookups
// prefer the active generation when callers do not request historical statuses.
func TestIndexedExpiryStatusFilterDefaultsToLive(t *testing.T) {
	t.Parallel()

	statusFilter, err := indexedExpiryStatusFilter(nil)
	require.NoError(t, err)
	require.Equal(
		t,
		[]arkrpc.VTXOStatus{
			arkrpc.VTXOStatus_VTXO_STATUS_LIVE,
		},
		statusFilter,
	)

	statusFilter, err = indexedExpiryStatusFilter([]waverpc.VTXOStatus{
		waverpc.VTXOStatus_VTXO_STATUS_FORFEITED,
	})
	require.NoError(t, err)
	require.Equal(
		t,
		[]arkrpc.VTXOStatus{
			arkrpc.VTXOStatus_VTXO_STATUS_FORFEITED,
		},
		statusFilter,
	)
}
