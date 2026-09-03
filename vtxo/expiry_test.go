package vtxo

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestFreeRefreshWindowThreshold verifies that an advertised waiver delays
// automatic refresh only when the local retry and unilateral-exit buffers
// remain intact.
func TestFreeRefreshWindowThreshold(t *testing.T) {
	t.Parallel()

	desc := &Descriptor{
		BatchExpiry:    1_000,
		RelativeExpiry: 24,
		Ancestry: []Ancestry{{
			TreeDepth: 2,
		}},
	}

	tests := []struct {
		name          string
		window        uint32
		wantThreshold int32
	}{
		{
			name:          "disabled",
			window:        0,
			wantThreshold: 144,
		},
		{
			name:          "safe delayed boundary",
			window:        120,
			wantThreshold: 120,
		},
		{
			name:          "late window keeps safety floor",
			window:        100,
			wantThreshold: 144,
		},
		{
			name:          "wide window keeps ordinary threshold",
			window:        200,
			wantThreshold: 144,
		},
		{
			name:          "large window does not overflow",
			window:        math.MaxUint32,
			wantThreshold: 144,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			cfg := DefaultExpiryConfig()
			cfg.FreeRefreshWindow = func() uint32 {
				return test.window
			}

			require.Equal(
				t, test.wantThreshold,
				cfg.CalculateRefreshThreshold(desc),
			)
		})
	}
}

// TestFreeRefreshWindowBoundary verifies the automatic expiry posture changes
// on the first block inside a safe advertised window.
func TestFreeRefreshWindowBoundary(t *testing.T) {
	t.Parallel()

	desc := &Descriptor{
		BatchExpiry:    1_000,
		RelativeExpiry: 24,
		Ancestry: []Ancestry{{
			TreeDepth: 2,
		}},
	}
	cfg := DefaultExpiryConfig()
	cfg.FreeRefreshWindow = func() uint32 {
		return 120
	}

	require.Equal(
		t, ExpiryStatusSafe, cfg.CheckExpiry(desc, 879),
	)
	require.Equal(
		t, ExpiryStatusNeedsRefresh, cfg.CheckExpiry(desc, 880),
	)
}

// TestMaxPaymentCLTVThreshold verifies automatic maintenance reserves the
// configured Lightning payment window in addition to the VTXO-specific exit
// and retry budgets. A later fee-waiver boundary must not erase that reserve.
func TestMaxPaymentCLTVThreshold(t *testing.T) {
	t.Parallel()

	desc := &Descriptor{
		BatchExpiry:    1_000,
		RelativeExpiry: 144,
		Ancestry: []Ancestry{{
			TreeDepth: 7,
		}},
	}

	cfg := DefaultExpiryConfig()
	require.Equal(t, int32(258), cfg.CalculateRefreshThreshold(desc))

	cfg.MaxPaymentCLTV = 300
	require.Equal(t, int32(558), cfg.CalculateRefreshThreshold(desc))

	cfg.FreeRefreshWindow = func() uint32 {
		return 500
	}
	require.Equal(t, int32(558), cfg.CalculateRefreshThreshold(desc))
	require.False(t, cfg.ShouldWaitForFreeRefreshWindow(desc, 400))

	cfg.FreeRefreshWindow = func() uint32 {
		return 576
	}
	require.Equal(t, int32(558), cfg.CalculateRefreshThreshold(desc))
	require.True(t, cfg.ShouldWaitForFreeRefreshWindow(desc, 400))

	require.Equal(t, ExpiryStatusSafe, cfg.CheckExpiry(desc, 441))
	require.Equal(
		t, ExpiryStatusNeedsRefresh, cfg.CheckExpiry(desc, 442),
	)
}

// TestMaxPaymentCLTVThresholdSaturates verifies an extreme direct package
// configuration cannot wrap the refresh threshold negative and postpone
// maintenance beyond the requested payment reserve.
func TestMaxPaymentCLTVThresholdSaturates(t *testing.T) {
	t.Parallel()

	desc := &Descriptor{
		BatchExpiry:    1_000,
		RelativeExpiry: 144,
	}
	cfg := DefaultExpiryConfig()
	cfg.MaxPaymentCLTV = math.MaxInt32

	require.Equal(
		t, int32(math.MaxInt32), cfg.CalculateRefreshThreshold(desc),
	)
}

// TestMaxPaymentCLTVRequiresUsefulBatchLifetime verifies a payment reserve
// must leave a fresh round-direct VTXO healthy for a full retry buffer. A
// target that merely fits would still create a near-continuous refresh loop.
func TestMaxPaymentCLTVRequiresUsefulBatchLifetime(t *testing.T) {
	t.Parallel()

	desc := &Descriptor{
		BatchExpiry:    1_108,
		CreatedHeight:  100,
		RelativeExpiry: 144,
		Ancestry: []Ancestry{{
			TreeDepth: 7,
		}},
	}
	cfg := DefaultExpiryConfig()

	// A 937-block threshold leaves only 71 healthy blocks in this
	// 1,008-block batch, so the ordinary 258-block policy wins.
	cfg.MaxPaymentCLTV = 679
	require.False(t, cfg.CanReserveMaxPaymentCLTV(desc))
	require.Equal(t, int32(258), cfg.CalculateRefreshThreshold(desc))

	// The same base floor governs subsidy-aware waiting. Without the
	// fallback, the impossible 937-block floor would reject this safe
	// 300-block operator window.
	cfg.FreeRefreshWindow = func() uint32 {
		return 300
	}
	require.True(t, cfg.ShouldWaitForFreeRefreshWindow(desc, 700))

	// A 936-block threshold leaves the full 72-block retry buffer and is
	// therefore useful rather than immediately recurring.
	cfg.FreeRefreshWindow = nil
	cfg.MaxPaymentCLTV = 678
	require.True(t, cfg.CanReserveMaxPaymentCLTV(desc))
	require.Equal(t, int32(936), cfg.CalculateRefreshThreshold(desc))
}

// TestMaxPaymentCLTVOORCanRefreshIntoFullLifetime verifies an OOR descendant
// does not mistake its inherited remaining lifetime for the operator's batch
// duration. Its first refresh can mint a round-direct replacement whose
// creation height makes the useful-lifetime guard authoritative.
func TestMaxPaymentCLTVOORCanRefreshIntoFullLifetime(t *testing.T) {
	t.Parallel()

	desc := &Descriptor{
		BatchExpiry:    676,
		CreatedHeight:  100,
		RelativeExpiry: 144,
		ChainDepth:     1,
		Ancestry: []Ancestry{{
			TreeDepth: 7,
		}},
	}
	cfg := DefaultExpiryConfig()
	cfg.MaxPaymentCLTV = 300

	require.True(t, cfg.CanReserveMaxPaymentCLTV(desc))
	require.Equal(t, int32(564), cfg.CalculateRefreshThreshold(desc))
}
