//go:build !swapruntime && !swapdirect

package darepoclicommands

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newSwapCmd returns a discoverable placeholder in default CLI builds. It
// avoids linking the direct swap SDK driver while giving users an actionable
// error when they try swap commands without the swapruntime tag.
func newSwapCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "swap",
		Short: "Lightning swap operations",
		Args:  cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return fmt.Errorf(
				"swap commands require a swapruntime build",
			)
		},
	}
}
