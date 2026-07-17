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
