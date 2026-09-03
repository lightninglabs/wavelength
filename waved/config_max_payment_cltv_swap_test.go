//go:build swapruntime

package waved

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSwapRuntimeDefaultsMaxPaymentCLTV verifies payment-capable builds keep
// enough VTXO lifetime in reserve for high-CLTV Lightning routes.
func TestSwapRuntimeDefaultsMaxPaymentCLTV(t *testing.T) {
	t.Parallel()

	require.Equal(
		t, DefaultMaxPaymentCLTV, DefaultConfig().MaxPaymentCLTV,
	)
}
