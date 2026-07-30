package waveclicommands

import (
	"strings"

	"github.com/spf13/pflag"
)

// snakeToKebabFlags folds a snake_case flag name onto its canonical
// kebab-case spelling so both forms resolve to the same flag.
func snakeToKebabFlags(_ *pflag.FlagSet, name string) pflag.NormalizedName {
	return pflag.NormalizedName(strings.ReplaceAll(name, "_", "-"))
}
