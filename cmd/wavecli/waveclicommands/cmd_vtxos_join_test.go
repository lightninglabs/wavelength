package waveclicommands

import (
	"bytes"
	"context"
	"testing"

	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// joinStubClient is a minimal DaemonServiceClient that implements only
// JoinNextRound with a canned response; every other method is nil and
// would panic if called, which these tests never do.
type joinStubClient struct {
	waverpc.DaemonServiceClient

	status string
	calls  int
}

func (c *joinStubClient) JoinNextRound(_ context.Context,
	_ *waverpc.JoinNextRoundRequest, _ ...grpc.CallOption) (
	*waverpc.JoinNextRoundResponse, error) {

	c.calls++

	return &waverpc.JoinNextRoundResponse{Status: c.status}, nil
}

// TestMaybeJoinNextRoundNothingToJoin pins that when the daemon reports a
// benign "nothing_to_join" (an auto-join after a refresh/leave that queued
// nothing), the CLI succeeds and says so plainly rather than printing the
// ordinary joined notice — the user-facing half of the fix that stops an
// empty --all from surfacing an INTERNAL round-join error.
func TestMaybeJoinNextRoundNothingToJoin(t *testing.T) {
	t.Parallel()

	var stderr bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetErr(&stderr)

	client := &joinStubClient{status: "nothing_to_join"}

	err := maybeJoinNextRound(cmd, client, false /* dryRun */, false)
	require.NoError(t, err)
	require.Equal(t, 1, client.calls)
	require.Contains(t, stderr.String(), "nothing queued to join")
}

// TestMaybeJoinNextRoundJoined pins that an ordinary join still prints the
// auto-join notice and does not misreport it as a no-op.
func TestMaybeJoinNextRoundJoined(t *testing.T) {
	t.Parallel()

	var stderr bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetErr(&stderr)

	client := &joinStubClient{status: "joined"}

	err := maybeJoinNextRound(cmd, client, false /* dryRun */, false)
	require.NoError(t, err)
	require.Equal(t, 1, client.calls)
	require.NotContains(t, stderr.String(), "nothing queued to join")
}
