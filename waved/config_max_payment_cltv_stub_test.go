//go:build !swapruntime

package waved

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCoreBuildDisablesMaxPaymentCLTV verifies clients without Lightning swap
// support do not refresh early for a payment capability they cannot use.
func TestCoreBuildDisablesMaxPaymentCLTV(t *testing.T) {
	t.Parallel()

	require.Zero(t, DefaultConfig().MaxPaymentCLTV)
}
