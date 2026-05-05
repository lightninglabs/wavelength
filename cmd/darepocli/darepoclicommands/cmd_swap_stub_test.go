//go:build !swapruntime && !swapdirect

package darepoclicommands

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSwapStubFailsGracefully(t *testing.T) {
	t.Parallel()

	err := newSwapCmd().Execute()
	require.ErrorContains(t, err, "swapruntime build")
}
