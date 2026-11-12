package waveclicommands

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestExitBatchCommandValidation proves the CLI accepts repeated selections and
// refuses empty, duplicate, malformed or unilateral requests without dialing.
func TestExitBatchCommandValidation(t *testing.T) {
	t.Parallel()
	first := strings.Repeat("a", 64) + ":0"
	second := strings.Repeat("b", 64) + ":1"
	for _, test := range []struct {
		name      string
		args      []string
		wantError bool
	}{
		{
			name: "batch",
			args: []string{
				"--outpoint",
				first,
				"--outpoint",
				second,
				"--dry-run",
			},
		},
		{
			name:      "empty",
			wantError: true,
		},
		{
			name: "duplicate",
			args: []string{
				"--outpoint",
				first,
				"--outpoint",
				first,
				"--dry-run",
			},
			wantError: true,
		},
		{
			name: "malformed",
			args: []string{
				"--outpoint",
				"bad",
				"--dry-run",
			},
			wantError: true,
		},
		{
			name: "unilateral",
			args: []string{
				"--outpoint",
				first,
				"--force-unroll-ack",
				forceUnrollAck,
			},
			wantError: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			cmd := newExitBatchCmd()
			cmd.SetArgs(test.args)
			err := cmd.Execute()
			if test.wantError {
				require.Error(t, err)

				return
			}
			require.NoError(t, err)
		})
	}
}
