//go:build systest

package systest

import (
	"testing"

	"github.com/lightninglabs/wavelength/waved"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
)

// TestDurableManualRefreshRegistration exercises the public daemon RPCs with
// the SQL-backed round runtime. An empty join must not block a later refresh;
// the queued refresh must cross durable encoding and reach the mailbox edge.
func TestDurableManualRefreshRegistration(t *testing.T) {
	ParallelN(t)

	f := newDirectedSendFixture(t, func(c *waved.Config) {
		c.DurableRoundRuntime = true
		c.EagerRoundJoin = false
	})
	empty, err := f.client.JoinNextRound(
		t.Context(), &waverpc.JoinNextRoundRequest{},
	)
	require.NoError(t, err)
	require.Equal(t, "nothing_to_join", empty.Status)
	require.Empty(t, f.mailboxServer.joinRoundRequests())

	refresh, err := f.client.RefreshVTXOs(
		t.Context(), &waverpc.RefreshVTXOsRequest{
			Selection: &waverpc.RefreshVTXOsRequest_All{
				All: true,
			},
		},
	)
	require.NoError(t, err)
	require.Equal(t, "queued", refresh.Status)
	require.Equal(
		t, []string{outpointString(f.seededOutpoint)},
		refresh.QueuedOutpoints,
	)
	require.Empty(t, f.mailboxServer.joinRoundRequests())

	joined, err := f.client.JoinNextRound(
		t.Context(), &waverpc.JoinNextRoundRequest{},
	)
	require.NoError(t, err)
	require.Equal(t, "joined", joined.Status)

	// The operator acknowledgement returns through production ingress and
	// promotes the temporary registration to a concrete persisted round.
	admitRefresh(t, f)
}
