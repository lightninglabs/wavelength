package darepoclicommands

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNewReceiveScriptRequestFromCmdForwardsLabel verifies the generic
// receive command carries its label flag into the daemon request.
func TestNewReceiveScriptRequestFromCmdForwardsLabel(t *testing.T) {
	t.Parallel()

	cmd := newReceiveCmd()
	require.NoError(t, cmd.Flags().Set("label", "coffee invoice"))

	req, err := newReceiveScriptRequestFromCmd(cmd)
	require.NoError(t, err)
	require.Equal(t, "coffee invoice", req.GetLabel())
}
