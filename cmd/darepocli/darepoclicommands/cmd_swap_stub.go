//go:build !swapruntime

package darepoclicommands

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newSwapCmd returns a discoverable placeholder in non-swapruntime CLI builds.
// Swap execution is daemon-owned, so the CLI only exposes real swap commands
// when built with the swapruntime tag.
func newSwapCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "swap",
		Short: "Lightning swap operations",
		Args:  cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return fmt.Errorf("swap commands require a " +
				"swapruntime build: rebuild with " +
				"-tags=swapruntime")
		},
	}
}
