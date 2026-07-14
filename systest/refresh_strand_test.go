//go:build systest

package systest

import (
	"context"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/waved"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
)

// refreshRestartRecoveryWindow bounds how long we wait after the restart for
// the manager's startup orphan-forfeit sweep to return the VTXO to LIVE. The
// sweep runs during VTXO manager startup, but the daemon must first reconnect
// its LND backend and recover actors, so we allow generous slack under parallel
// Dockerized systest load.
const refreshRestartRecoveryWindow = 60 * time.Second

// TestRefreshStrandedVTXORecoversOnRestart proves the startup orphan-forfeit
// sweep. A cooperative refresh reserves the seeded VTXO into pending-forfeit
// and parks a temp REGISTRATION_SENT round, but the fake operator never returns
// a RoundJoined admission watermark. The admission timeout is disabled, so the
// in-flight round's own recovery path (wavelength#653) can never fire — the
// only thing that can rescue the reserved VTXO is the manager's startup sweep.
//
// We then restart the daemon. The in-memory round FSM dies, and the VTXO is
// recovered from disk still in pending-forfeit. Because no forfeit signature
// was ever submitted (that happens only on the pending-forfeit -> forfeiting
// transition), the VTXO is provably orphaned and the startup sweep returns it
// to LIVE with the spendable balance restored. On an unfixed daemon the VTXO
// stays pending-forfeit forever and the final assertion times out.
func TestRefreshStrandedVTXORecoversOnRestart(t *testing.T) {
	ParallelN(t)

	// EagerRoundJoin drives the refresh into IntentSentState immediately. A
	// negative RegistrationTimeout disables the admission timeout entirely,
	// so the round never self-recovers and the startup sweep is isolated as
	// the sole recovery mechanism under test.
	fixture := newDirectedSendFixture(t, func(c *waved.Config) {
		c.EagerRoundJoin = true
		c.RegistrationTimeout = -1 * time.Second
	})

	// The seeded VTXO starts LIVE and is the entire spendable balance.
	startVTXOs := listAllVTXOs(t, fixture.client)
	require.Len(t, startVTXOs, 1)
	require.Equal(
		t, waverpc.VTXOStatus_VTXO_STATUS_LIVE, startVTXOs[0].Status,
	)
	require.Equal(
		t, testSeededAmountSat, vtxoBalanceSat(t, fixture.client),
	)

	// Issue the refresh. The reservation into pending-forfeit is
	// synchronous, so by the time this returns the VTXO is already
	// reserved.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	refreshResp, err := fixture.client.RefreshVTXOs(
		ctx, &waverpc.RefreshVTXOsRequest{
			Selection: &waverpc.RefreshVTXOsRequest_Outpoints{
				Outpoints: &waverpc.OutpointSelection{
					Outpoints: []string{
						outpointString(
							fixture.seededOutpoint,
						),
					},
				},
			},
		},
	)
	require.NoError(t, err, "RefreshVTXOs RPC failed")
	require.Equal(t, "queued", refreshResp.Status)
	require.Contains(
		t, refreshResp.QueuedOutpoints,
		outpointString(fixture.seededOutpoint),
	)

	// The stranded condition: the refresh reserves the VTXO into
	// pending-forfeit synchronously. With the admission timeout disabled
	// and the operator never admitting the round, the reservation is owned
	// only by the in-memory round FSM and holds indefinitely until the
	// restart.
	requireVTXOStatusEventually(
		t, fixture.client, fixture.seededOutpoint,
		waverpc.VTXOStatus_VTXO_STATUS_PENDING_FORFEIT,
		parkedObserveWindow,
	)

	// The crash: restart the daemon against the same data directory. The
	// in-memory round FSM is lost; the reserved VTXO is recovered from disk
	// still in pending-forfeit.
	fixture.restart()

	// The fix: the startup orphan-forfeit sweep returns the VTXO to LIVE.
	requireVTXOStatusEventually(
		t, fixture.client, fixture.seededOutpoint,
		waverpc.VTXOStatus_VTXO_STATUS_LIVE,
		refreshRestartRecoveryWindow,
	)

	// The spendable balance is restored to its starting value.
	require.Eventually(
		t,
		func() bool {
			return vtxoBalanceSat(t, fixture.client) ==
				testSeededAmountSat
		},
		20*time.Second, 100*time.Millisecond,
		"vtxo balance not restored after restart recovery",
	)
}
