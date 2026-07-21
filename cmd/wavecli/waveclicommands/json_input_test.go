package waveclicommands

import (
	"testing"

	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// TestRootJSONFlagsHaveDistinctDirections pins the pre-launch contract that
// --json controls output while --request-json supplies raw request input.
func TestRootJSONFlagsHaveDistinctDirections(t *testing.T) {
	t.Parallel()

	root := newRootCmd(false)
	jsonFlag := root.PersistentFlags().Lookup("json")
	require.NotNil(t, jsonFlag)
	require.Equal(t, "bool", jsonFlag.Value.Type())

	requestFlag := root.PersistentFlags().Lookup("request-json")
	require.NotNil(t, requestFlag)
	require.Equal(t, "string", requestFlag.Value.Type())
}

// TestParseRequestUsesRequestJSON verifies raw input wins over bespoke flags
// under the unambiguous request-side flag name.
func TestParseRequestUsesRequestJSON(t *testing.T) {
	t.Parallel()

	cmd := &cobra.Command{}
	cmd.Flags().String("request-json", "", "")
	require.NoError(t, cmd.Flags().Set(
		"request-json", `{"round_id":"from-json"}`,
	))

	req := &waverpc.GetRoundRequest{}
	flagsCalled := false
	err := parseRequest(cmd, req, func() error {
		flagsCalled = true
		req.RoundId = "from-flags"

		return nil
	})
	require.NoError(t, err)
	require.False(t, flagsCalled)
	require.Equal(t, "from-json", req.GetRoundId())
}

// TestParseRequestNamesInvalidInputFlag keeps error remediation precise for
// agents migrating from the old overloaded --json spelling.
func TestParseRequestNamesInvalidInputFlag(t *testing.T) {
	t.Parallel()

	cmd := &cobra.Command{}
	cmd.Flags().String("request-json", "", "")
	require.NoError(t, cmd.Flags().Set("request-json", `{`))

	err := parseRequest(cmd, &waverpc.GetRoundRequest{}, nil)
	require.ErrorContains(t, err, "invalid --request-json payload")
}
