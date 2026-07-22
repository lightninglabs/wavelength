package waveclicommands

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/require"
)

// TestCanonicalFlagNamesUseKebabCase walks the real command tree so help and
// schema-oriented callers never learn a snake_case CLI spelling.
func TestCanonicalFlagNamesUseKebabCase(t *testing.T) {
	t.Parallel()

	root := newRootCmd(true)
	var walk func(*cobra.Command)
	walk = func(cmd *cobra.Command) {
		cmd.LocalFlags().VisitAll(func(flag *pflag.Flag) {
			require.NotContainsf(
				t, flag.Name, "_", "command %q flag %q is "+
					"not kebab-case", cmd.CommandPath(),
				flag.Name,
			)
		})
		for _, child := range cmd.Commands() {
			walk(child)
		}
	}
	walk(root)
}

// TestSnakeCaseFlagsRemainAliases verifies normalization preserves scripts
// that copied snake_case proto field names before launch.
func TestSnakeCaseFlagsRemainAliases(t *testing.T) {
	t.Parallel()

	root := newRootCmd(false)
	send := findRootCommand(t, root, "send")
	require.NoError(t, send.Flags().Set("max_fee", "21"))

	maxFee, err := send.Flags().GetUint64("max-fee")
	require.NoError(t, err)
	require.Equal(t, uint64(21), maxFee)

	canonical := send.Flags().Lookup("max-fee")
	alias := send.Flags().Lookup(strings.ReplaceAll(
		canonical.Name, "-", "_",
	))
	require.Same(t, canonical, alias)
}
