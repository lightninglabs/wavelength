package waveclicommands

import (
	"strings"
	"testing"

	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
)

// TestValidStatusesMatchEnum pins validStatuses to the proto enum.
func TestValidStatusesMatchEnum(t *testing.T) {
	t.Parallel()

	all := allVTXOStatuses()
	require.NotContains(
		t, all, waverpc.VTXOStatus_VTXO_STATUS_UNSPECIFIED,
	)
	require.IsIncreasing(t, all)

	expected := make([]string, 0, len(all))
	for _, vtxoStatus := range all {
		expected = append(
			expected,
			strings.ToLower(
				strings.TrimPrefix(
					vtxoStatus.String(),
					"VTXO_STATUS_",
				),
			),
		)
	}
	require.ElementsMatch(t, expected, validStatuses)
}

// TestParseVTXOStatus checks accepted and rejected status names.
func TestParseVTXOStatus(t *testing.T) {
	t.Parallel()

	for _, name := range validStatuses {
		_, ok := parseVTXOStatus(name)
		require.True(t, ok, name)
	}

	for _, name := range []string{"unspecified", "bogus", ""} {
		_, ok := parseVTXOStatus(name)
		require.False(t, ok, name)
	}
}

// TestWantsCheckpointPSBTs covers the --fields and --all combinations.
func TestWantsCheckpointPSBTs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		fields string
		all    bool
		want   bool
	}{
		{
			name: "full projection",
			want: true,
		},
		{
			name: "all without fields",
			all:  true,
			want: false,
		},
		{
			name:   "fields omit psbts",
			fields: "outpoint,amount_sat",
			want:   false,
		},
		{
			name:   "fields name psbts under all",
			fields: "outpoint, " + checkpointPSBTsField,
			all:    true,
			want:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cmd := newVTXOsListCmd()
			if tc.fields != "" {
				require.NoError(
					t, cmd.Flags().Set("fields", tc.fields),
				)
			}

			require.Equal(
				t, tc.want, wantsCheckpointPSBTs(cmd, tc.all),
			)
		})
	}
}
