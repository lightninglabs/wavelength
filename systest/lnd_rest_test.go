//go:build systest

package systest

import (
	"testing"

	"github.com/lightninglabs/wavelength/waved"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
)

// TestLNDRESTBackendListVTXO exercises the lnd wallet backend over the REST
// transport end to end: it stands up a full waved daemon whose LndConfig
// selects the rest transport pointed at the harness lnd REST gateway, then
// verifies the daemon connects (GetInfo over REST seeds the node identity) and
// serves the seeded live VTXO through its wallet surface.
//
// This complements the gRPC-backed send/receive coverage: it proves the REST
// adapter's connect path (GetInfo), the lnd chain backend wired over REST
// (block-epoch notifications + fee estimation), and the daemon reaching
// readiness while backed by REST rather than gRPC.
func TestLNDRESTBackendListVTXO(t *testing.T) {
	ParallelN(t)

	fixture := newDirectedSendFixture(t, func(cfg *waved.Config) {
		cfg.Lnd.Transport = waved.RPCTransportREST
	})

	// The daemon reached readiness in launch(); confirm the REST GetInfo
	// round-trip populated the lnd node identity on the daemon status.
	info, err := fixture.client.GetInfo(
		t.Context(), &waverpc.GetInfoRequest{},
	)
	require.NoError(t, err)
	require.NotEmpty(t, info.LndIdentityPubkey)

	// The seeded live VTXO must be visible through the REST-backed wallet.
	vtxos := listAllVTXOs(t, fixture.client)
	require.Len(t, vtxos, 1)
	require.Equal(
		t, waverpc.VTXOStatus_VTXO_STATUS_LIVE, vtxos[0].Status,
	)

	vtxoInfo := findVTXOByOutpoint(vtxos, fixture.seededOutpoint)
	require.NotNil(t, vtxoInfo)
}
