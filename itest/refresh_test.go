//go:build itest

package itest

import (
	"context"
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	client_harness "github.com/lightninglabs/darepo-client/harness"
	"github.com/lightninglabs/darepo/adminrpc"
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
	knownLiveVTXOs := outpointSet(listLiveVTXOs(t, alice.RPCClient))

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
	waitForNamedClientRoundState(
		t, alice.RPCClient, refreshRound.RoundId,
		daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT,
	)
	waitForPersistedClientRoundState(
		t, alice.RPCClient, refreshRound.RoundId,
		daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT, 0,
	)

	broadcastRound := waitForOperatorRoundStatus(
		t, h, refreshRound.RoundId,
		adminrpc.RoundStatus_ROUND_STATUS_BROADCAST,
	)
	require.NotEmpty(t, broadcastRound.TxId)
	t.Logf("Refresh round transaction broadcast: round_id=%q txid=%s",
		refreshRound.RoundId, broadcastRound.TxId)

	mineUntilOperatorRoundConfirmed(
		t, h, refreshRound.RoundId, broadcastRound.TxId,
	)
	waitForVTXOStatusByOutpoint(
		t, alice.RPCClient, liveVTXO.Outpoint,
		daemonrpc.VTXOStatus_VTXO_STATUS_FORFEITED,
	)

	refreshedVTXO := waitForNewLiveVTXOWithAmount(
		t, alice.RPCClient, knownLiveVTXOs, liveVTXO.AmountSat,
	)
	require.NotEqual(t, liveVTXO.Outpoint, refreshedVTXO.Outpoint)
	require.Equal(t, refreshRound.RoundId, refreshedVTXO.RoundId)

	finalBalance := waitForExactVTXOBalance(
		t, alice.RPCClient, refreshedVTXO.AmountSat,
	)
	require.Equal(t, refreshedVTXO.AmountSat, finalBalance.VtxoBalanceSat)
}

// TestRefreshIntegrationReceivedOORVTXOLifecycle verifies the daemon can
// refresh a VTXO that was first materialized through an incoming OOR transfer.
func TestRefreshIntegrationReceivedOORVTXOLifecycle(t *testing.T) {
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
	bob := h.StartClientDaemon("bob")
	operatorInfo := getOperatorInfo(t, h)

	waitForRegisteredClients(t, h, 2)

	_, aliceLiveVTXO, _ := boardClientAndConfirmRound(
		t, h, alice.RPCClient, operatorInfo.MinConfirmations, 120_000,
	)

	bobRecvResp, err := bob.RPCClient.NewOORReceiveScript(
		t.Context(), &daemonrpc.NewOORReceiveScriptRequest{
			Label: "itest-refresh-received-oor-bob",
		},
	)
	require.NoError(t, err, "NewOORReceiveScript RPC failed")
	require.NotEmpty(t, bobRecvResp.PkScriptHex)

	bobPkScript, err := hex.DecodeString(bobRecvResp.PkScriptHex)
	require.NoError(t, err, "pk_script_hex must be valid hex")

	sendAmount := aliceLiveVTXO.AmountSat
	bobLiveBeforeReceive := outpointSet(listLiveVTXOs(t, bob.RPCClient))
	sendResp, err := alice.RPCClient.SendOOR(
		t.Context(), &daemonrpc.SendOORRequest{
			Recipient: &daemonrpc.Output{
				Destination: &daemonrpc.Output_PkScript{
					PkScript: bobPkScript,
				},
				AmountSat: sendAmount,
			},
		},
	)
	require.NoError(t, err, "SendOOR RPC failed")
	require.Equal(t, "submitted", sendResp.Status)
	require.NotEmpty(t, sendResp.SessionId)

	waitForVTXOStatusByOutpoint(
		t, alice.RPCClient, aliceLiveVTXO.Outpoint,
		daemonrpc.VTXOStatus_VTXO_STATUS_SPENT,
	)
	receivedVTXO := waitForNewLiveVTXOWithAmount(
		t, bob.RPCClient, bobLiveBeforeReceive, sendAmount,
	)
	require.Equal(
		t, sendAmount,
		waitForExactVTXOBalance(t, bob.RPCClient, sendAmount).
			VtxoBalanceSat,
	)

	knownBobLiveVTXOs := outpointSet(listLiveVTXOs(t, bob.RPCClient))
	existingRoundIDs := snapshotClientRoundIDs(t, bob.RPCClient)

	refreshResp, err := bob.RPCClient.RefreshVTXOs(
		t.Context(), &daemonrpc.RefreshVTXOsRequest{
			Selection: &daemonrpc.RefreshVTXOsRequest_Outpoints{
				Outpoints: &daemonrpc.OutpointSelection{
					Outpoints: []string{
						receivedVTXO.Outpoint,
					},
				},
			},
		},
	)
	require.NoError(t, err, "RefreshVTXOs RPC failed")
	require.Equal(t, "queued", refreshResp.Status)
	require.Contains(t, refreshResp.QueuedOutpoints, receivedVTXO.Outpoint)

	bob.TriggerRoundRegistration()

	refreshRound := waitForNewClientRoundState(
		t, bob.RPCClient, existingRoundIDs,
		daemonrpc.RoundState_ROUND_STATE_JOINED,
	)
	require.NotEmpty(t, refreshRound.RoundId)
	require.False(t, refreshRound.IsTemp)
	waitForNamedClientRoundState(
		t, bob.RPCClient, refreshRound.RoundId,
		daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT,
	)
	waitForPersistedClientRoundState(
		t, bob.RPCClient, refreshRound.RoundId,
		daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT, 0,
	)

	broadcastRound := waitForOperatorRoundStatus(
		t, h, refreshRound.RoundId,
		adminrpc.RoundStatus_ROUND_STATUS_BROADCAST,
	)
	require.NotEmpty(t, broadcastRound.TxId)

	mineUntilOperatorRoundConfirmed(
		t, h, refreshRound.RoundId, broadcastRound.TxId,
	)
	waitForVTXOStatusByOutpoint(
		t, bob.RPCClient, receivedVTXO.Outpoint,
		daemonrpc.VTXOStatus_VTXO_STATUS_FORFEITED,
	)

	refreshedVTXO := waitForNewLiveVTXOWithAmount(
		t, bob.RPCClient, knownBobLiveVTXOs, receivedVTXO.AmountSat,
	)
	require.NotEqual(t, receivedVTXO.Outpoint, refreshedVTXO.Outpoint)
	require.Equal(t, refreshRound.RoundId, refreshedVTXO.RoundId)

	finalBalance := waitForExactVTXOBalance(
		t, bob.RPCClient, refreshedVTXO.AmountSat,
	)
	require.Equal(t, refreshedVTXO.AmountSat, finalBalance.VtxoBalanceSat)
}

// TestRefreshIntegrationDryRunPreview verifies RefreshVTXOs dry-run mode
// validates target selection without queuing a real refresh intent.
func TestRefreshIntegrationDryRunPreview(t *testing.T) {
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
	existingRoundIDs := snapshotClientRoundIDs(t, alice.RPCClient)

	refreshResp, err := alice.RPCClient.RefreshVTXOs(
		t.Context(), &daemonrpc.RefreshVTXOsRequest{
			Selection: &daemonrpc.RefreshVTXOsRequest_Outpoints{
				Outpoints: &daemonrpc.OutpointSelection{
					Outpoints: []string{liveVTXO.Outpoint},
				},
			},
			DryRun: true,
		},
	)
	require.NoError(t, err, "RefreshVTXOs dry-run RPC failed")
	require.Equal(t, "preview", refreshResp.Status)
	require.Contains(t, refreshResp.QueuedOutpoints, liveVTXO.Outpoint)

	waitForExactVTXOBalance(
		t, alice.RPCClient, startBalance.VtxoBalanceSat,
	)
	waitForVTXOStatusByOutpoint(
		t, alice.RPCClient, liveVTXO.Outpoint,
		daemonrpc.VTXOStatus_VTXO_STATUS_LIVE,
	)

	require.Never(t, func() bool {
		ctx, cancel := context.WithTimeout(
			t.Context(), defaultSmallTimeout,
		)
		defer cancel()

		resp, err := alice.RPCClient.ListRounds(
			ctx, &daemonrpc.ListRoundsRequest{},
		)
		if err != nil {
			t.Logf(
				"ListRounds failed during dry-run check: %v",
				err,
			)

			return true
		}

		for _, round := range resp.Rounds {
			roundID := round.RoundId
			if _, known := existingRoundIDs[roundID]; !known {
				return true
			}
		}

		return false
	}, 3*pollInterval, pollInterval,
		"dry-run should not create a new round")
}

// TestRefreshIntegrationAllSelectionQueuesLiveOutpoints verifies the all=true
// refresh selector queues every currently live VTXO for refresh.
func TestRefreshIntegrationAllSelectionQueuesLiveOutpoints(t *testing.T) {
	clientOpts := client_harness.DefaultOptions()
	clientOpts.GroupName = t.Name()
	clientOpts.StartTapd = false

	h := harness.NewArkHarness(t, &harness.ArkHarnessOptions{
		ClientOptions: &clientOpts,
	})
	t.Cleanup(h.Stop)

	h.Start()
	h.FundOperatorLND(btcutil.SatoshiPerBitcoin * 3)

	alice := h.StartClientDaemon("alice")
	operatorInfo := getOperatorInfo(t, h)

	waitForClientRegistration(t, h)

	_, liveVTXO1, _ := boardClientAndConfirmRound(
		t, h, alice.RPCClient, operatorInfo.MinConfirmations, 100_000,
	)
	_, liveVTXO2, _ := boardClientAndConfirmRound(
		t, h, alice.RPCClient, operatorInfo.MinConfirmations, 120_000,
	)
	require.NotEqual(t, liveVTXO1.Outpoint, liveVTXO2.Outpoint)

	refreshResp, err := alice.RPCClient.RefreshVTXOs(
		t.Context(), &daemonrpc.RefreshVTXOsRequest{
			Selection: &daemonrpc.RefreshVTXOsRequest_All{
				All: true,
			},
		},
	)
	require.NoError(t, err, "RefreshVTXOs RPC failed")
	require.Equal(t, "queued", refreshResp.Status)
	require.Contains(t, refreshResp.QueuedOutpoints, liveVTXO1.Outpoint)
	require.Contains(t, refreshResp.QueuedOutpoints, liveVTXO2.Outpoint)
}
