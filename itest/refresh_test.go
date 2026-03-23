//go:build itest

package itest

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	client_harness "github.com/lightninglabs/darepo-client/harness"
	"github.com/lightninglabs/darepo/harness"
	"github.com/stretchr/testify/require"
)

// TestRefreshIntegrationSingleVTXOLifecycle verifies the daemon-level refresh
// intent path for one live VTXO: queue refresh and observe that the daemon
// joins a new round after an explicit round-registration trigger.
func TestRefreshIntegrationSingleVTXOLifecycle(t *testing.T) {
	clientOpts := client_harness.DefaultOptions()
	clientOpts.GroupName = t.Name()
	clientOpts.StartTapd = false

	h := harness.NewArkHarness(t, &harness.ArkHarnessOptions{
		ClientOptions: &clientOpts,
	})
	t.Cleanup(h.Stop)

	h.Start()
	h.FundOperatorLND(btcutil.SatoshiPerBitcoin * 2)

	alice := h.StartClientDaemon("alice")
	operatorInfo := getOperatorInfo(t, h)

	waitForClientRegistration(t, h)

	_, liveVTXO, startBalance := boardClientAndConfirmRound(
		t, h, alice.RPCClient, operatorInfo.MinConfirmations, 120_000,
	)
	require.Equal(t, liveVTXO.AmountSat, startBalance.VtxoBalanceSat)

	existingRoundIDs := snapshotClientRoundIDs(t, alice.RPCClient)

	refreshResp, err := alice.RPCClient.RefreshVTXOs(
		t.Context(), &daemonrpc.RefreshVTXOsRequest{
			Selection: &daemonrpc.RefreshVTXOsRequest_Outpoints{
				Outpoints: &daemonrpc.OutpointSelection{
					Outpoints: []string{liveVTXO.Outpoint},
				},
			},
		},
	)
	require.NoError(t, err, "RefreshVTXOs RPC failed")
	require.Equal(t, "queued", refreshResp.Status)
	require.Contains(t, refreshResp.QueuedOutpoints, liveVTXO.Outpoint)

	// Refresh intents are already queued durably; they still need the
	// daemon's round actor to emit RegistrationRequested before the
	// queued refresh can leave PendingAssembly.
	alice.TriggerRoundRegistration()

	refreshRound := waitForNewClientRoundState(
		t, alice.RPCClient, existingRoundIDs,
		daemonrpc.RoundState_ROUND_STATE_JOINED,
	)
	require.NotEmpty(t, refreshRound.RoundId)
	require.False(t, refreshRound.IsTemp)

	// TODO(bhandras): Extend this once single-client refresh rounds stop
	// failing with "build connector output: dust amount must be > 0" so we
	// can assert the original VTXO is no longer live and the replacement
	// VTXO is live after the refreshed round settles.
}
