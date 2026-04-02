//go:build itest

package itest

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	client_harness "github.com/lightninglabs/darepo-client/harness"
	"github.com/lightninglabs/darepo/harness"
	"github.com/stretchr/testify/require"
)

const (
	// testVTXOExitDelay is a short CSV delay used by unroll integration
	// tests to keep block-mining time reasonable.
	testVTXOExitDelay = 10
)

// newUnrollHarness creates a test harness with a reduced VTXO exit delay
// so that the unroll CSV wait completes in a reasonable number of blocks.
func newUnrollHarness(t *testing.T) *harness.ArkHarness {
	t.Helper()

	clientOpts := client_harness.DefaultOptions()
	clientOpts.GroupName = t.Name()
	clientOpts.StartTapd = false

	h := harness.NewArkHarness(t, &harness.ArkHarnessOptions{
		ClientOptions: &clientOpts,
		ServerConfigFunc: func(cfg *darepo.Config) {
			cfg.Rounds.VTXOExitDelay = testVTXOExitDelay
		},
	})
	t.Cleanup(h.Stop)

	h.Start()
	h.FundOperatorLND(btcutil.SatoshiPerBitcoin * 2)

	return h
}

// TestUnilateralExitManualStartSingleParentTree verifies the full
// lifecycle: manual trigger, dedup, VTXO status transition, recovery
// chain materialization, CSV wait, sweep, and completion.
func TestUnilateralExitManualStartSingleParentTree(t *testing.T) {
	h := newUnrollHarness(t)

	alice := h.StartClientDaemon("alice")
	operatorInfo := getOperatorInfo(t, h)

	waitForRegisteredClients(t, h, 1)

	_, aliceVTXO, _ := boardClientAndConfirmRound(
		t, h, alice.RPCClient,
		operatorInfo.MinConfirmations, 100_000,
	)
	require.NotNil(t, aliceVTXO)
	require.True(t, aliceVTXO.AmountSat > 0)

	// Fund Alice's LND wallet for CPFP fees during unroll.
	h.FundClientLND(alice, btcutil.SatoshiPerBitcoin)

	h.Logf("Alice has live VTXO: outpoint=%s amount=%d",
		aliceVTXO.Outpoint, aliceVTXO.AmountSat)

	initialWalletUTXOs := confirmedWalletUTXOValues(t, alice)

	// Trigger the unilateral exit via the Unroll RPC.
	unrollResp, err := alice.RPCClient.Unroll(
		t.Context(), &daemonrpc.UnrollRequest{
			Outpoint: aliceVTXO.Outpoint,
		},
	)
	require.NoError(t, err, "Unroll RPC should succeed")
	require.True(t, unrollResp.Created,
		"should have created a new unroll job")
	require.NotEmpty(t, unrollResp.ActorId,
		"actor ID should be set")

	h.Logf("Unroll job created: actor_id=%s", unrollResp.ActorId)

	// Verify the VTXO is retired from the live set.
	waitForVTXOStatusByOutpoint(
		t, alice.RPCClient, aliceVTXO.Outpoint,
		daemonrpc.VTXOStatus_VTXO_STATUS_UNILATERAL_EXIT,
	)

	// After the status transition is confirmed, a second call for
	// the same outpoint should not create a new job.
	unrollResp2, err := alice.RPCClient.Unroll(
		t.Context(), &daemonrpc.UnrollRequest{
			Outpoint: aliceVTXO.Outpoint,
		},
	)
	require.NoError(t, err, "second Unroll should succeed")
	require.False(t, unrollResp2.Created,
		"second call should return Created=false")

	// Verify the unroll job exists via GetUnrollStatus.
	statusResp, err := alice.RPCClient.GetUnrollStatus(
		t.Context(), &daemonrpc.GetUnrollStatusRequest{
			Outpoint: aliceVTXO.Outpoint,
		},
	)
	require.NoError(t, err)
	require.True(t, statusResp.Found, "unroll job should be found")

	h.Logf("VTXO in UNILATERAL_EXIT, job status=%s",
		statusResp.Status)

	// Mine blocks and wait for the unroll job to reach completion.
	sweptOutpoint := waitForUnrollSweepToWallet(
		t, h, alice, alice.RPCClient, aliceVTXO.Outpoint,
		aliceVTXO.AmountSat, initialWalletUTXOs,
	)

	h.Logf("Unroll completed: VTXO %s swept back to wallet UTXO %s",
		aliceVTXO.Outpoint, sweptOutpoint)
}

// TestUnilateralExitRoundBornCompletion verifies the full end-to-end
// unilateral exit flow for a round-born VTXO without the manual-start
// assertions (dedup, status query). This exercises the minimal trigger
// → completion path.
func TestUnilateralExitRoundBornCompletion(t *testing.T) {
	h := newUnrollHarness(t)

	alice := h.StartClientDaemon("alice")
	operatorInfo := getOperatorInfo(t, h)

	waitForRegisteredClients(t, h, 1)

	_, aliceVTXO, _ := boardClientAndConfirmRound(
		t, h, alice.RPCClient,
		operatorInfo.MinConfirmations, 100_000,
	)
	require.NotNil(t, aliceVTXO)

	h.FundClientLND(alice, btcutil.SatoshiPerBitcoin)

	h.Logf("Alice VTXO: outpoint=%s amount=%d",
		aliceVTXO.Outpoint, aliceVTXO.AmountSat)

	initialWalletUTXOs := confirmedWalletUTXOValues(t, alice)

	unrollResp, err := alice.RPCClient.Unroll(
		t.Context(), &daemonrpc.UnrollRequest{
			Outpoint: aliceVTXO.Outpoint,
		},
	)
	require.NoError(t, err)
	require.True(t, unrollResp.Created)

	waitForVTXOStatusByOutpoint(
		t, alice.RPCClient, aliceVTXO.Outpoint,
		daemonrpc.VTXOStatus_VTXO_STATUS_UNILATERAL_EXIT,
	)

	sweptOutpoint := waitForUnrollSweepToWallet(
		t, h, alice, alice.RPCClient, aliceVTXO.Outpoint,
		aliceVTXO.AmountSat, initialWalletUTXOs,
	)

	h.Logf("Unroll completed: VTXO %s swept back to wallet UTXO %s",
		aliceVTXO.Outpoint, sweptOutpoint)
}

// TestUnilateralExitOORDerivedCompletion verifies the full end-to-end
// unilateral exit flow for an OOR-derived VTXO. Alice sends an OOR
// transfer to Bob, then Bob triggers unroll on the received VTXO.
//
// Both clients board in a single round and Bob only boards the
// minimum needed to register. This keeps total mining low to avoid
// overloading the per-client LND instances.
func TestUnilateralExitOORDerivedCompletion(t *testing.T) {
	h := newUnrollHarness(t)

	alice := h.StartClientDaemon("alice")
	bob := h.StartClientDaemon("bob")
	operatorInfo := getOperatorInfo(t, h)

	waitForRegisteredClients(t, h, 2)

	// Board Alice to get a live VTXO for the OOR transfer.
	_, aliceLiveVTXO, _ := boardClientAndConfirmRound(
		t, h, alice.RPCClient,
		operatorInfo.MinConfirmations, 100_000,
	)

	// Fund Bob's LND wallet for CPFP fees during unroll.
	h.FundClientLND(bob, btcutil.SatoshiPerBitcoin)

	// Bob creates an OOR receive script.
	bobLiveBefore := outpointSet(listLiveVTXOs(t, bob.RPCClient))

	recvResp, err := bob.RPCClient.NewOORReceiveScript(
		t.Context(), &daemonrpc.NewOORReceiveScriptRequest{
			Label: "itest-unroll-oor",
		},
	)
	require.NoError(t, err)
	require.NotEmpty(t, recvResp.PkScriptHex)

	recipientPkScript, err := hex.DecodeString(recvResp.PkScriptHex)
	require.NoError(t, err)

	// Alice sends OOR to Bob (full amount).
	sendAmount := aliceLiveVTXO.AmountSat
	sendResp, err := alice.RPCClient.SendOOR(
		t.Context(), &daemonrpc.SendOORRequest{
			Recipient: &daemonrpc.Output{
				Destination: &daemonrpc.Output_PkScript{
					PkScript: recipientPkScript,
				},
				AmountSat: sendAmount,
			},
		},
	)
	require.NoError(t, err)
	require.Equal(t, "submitted", sendResp.Status)

	h.Logf("OOR transfer submitted: session=%s amount=%d",
		sendResp.SessionId, sendAmount)

	// Wait for Bob to receive the OOR VTXO.
	receivedVTXO := waitForNewLiveVTXOWithAmount(
		t, bob.RPCClient, bobLiveBefore, sendAmount,
	)
	require.NotNil(t, receivedVTXO)

	h.Logf("Bob received OOR VTXO: outpoint=%s amount=%d "+
		"round_id=%s",
		receivedVTXO.Outpoint, receivedVTXO.AmountSat,
		receivedVTXO.RoundId)

	initialWalletUTXOs := confirmedWalletUTXOValues(t, bob)

	// Verify the OOR-derived VTXO shares the same parent tree.
	require.Equal(t, aliceLiveVTXO.RoundId, receivedVTXO.RoundId,
		"OOR VTXO should share the same round ID")

	// Trigger unilateral exit on Bob's OOR-derived VTXO.
	unrollResp, err := bob.RPCClient.Unroll(
		t.Context(), &daemonrpc.UnrollRequest{
			Outpoint: receivedVTXO.Outpoint,
		},
	)
	require.NoError(t, err)
	require.True(t, unrollResp.Created)

	h.Logf("Unroll job created for OOR VTXO: actor_id=%s",
		unrollResp.ActorId)

	// Verify the VTXO is retired from Bob's live set.
	waitForVTXOStatusByOutpoint(
		t, bob.RPCClient, receivedVTXO.Outpoint,
		daemonrpc.VTXOStatus_VTXO_STATUS_UNILATERAL_EXIT,
	)

	// Mine blocks and wait for unroll completion.
	sweptOutpoint := waitForUnrollSweepToWallet(
		t, h, bob, bob.RPCClient, receivedVTXO.Outpoint,
		receivedVTXO.AmountSat, initialWalletUTXOs,
	)

	h.Logf("Unroll completed: OOR VTXO %s swept back to wallet UTXO %s",
		receivedVTXO.Outpoint, sweptOutpoint)
}

// waitForUnrollJobCompletion mines blocks and polls GetUnrollStatus
// until the job reaches COMPLETED status.
func waitForUnrollJobCompletion(t *testing.T, h *harness.ArkHarness,
	client daemonrpc.DaemonServiceClient, outpoint string) {

	t.Helper()

	var lastStatus daemonrpc.UnrollJobStatus

	// Give the actor system time to submit initial recovery
	// packages before mining starts.
	time.Sleep(2 * time.Second)

	require.Eventually(t, func() bool {
		h.Generate(3)

		ctx, cancel := context.WithTimeout(
			t.Context(), defaultSmallTimeout,
		)
		defer cancel()

		resp, err := client.GetUnrollStatus(
			ctx, &daemonrpc.GetUnrollStatusRequest{
				Outpoint: outpoint,
			},
		)
		if err != nil || !resp.Found {
			return false
		}

		if resp.Status != lastStatus {
			h.Logf("Unroll job %s status: %s",
				outpoint, resp.Status)
			lastStatus = resp.Status
		}

		if resp.Status == daemonrpc.UnrollJobStatus_UNROLL_JOB_STATUS_FAILED {
			t.Fatalf("Unroll job failed: %s", resp.LastError)
		}

		return resp.Status == daemonrpc.UnrollJobStatus_UNROLL_JOB_STATUS_COMPLETED
	}, 90*time.Second, 1*time.Second,
		"unroll job never completed for %s (last status: %s)",
		outpoint, lastStatus)
}

// waitForUnrollSweepToWallet waits for the unroll job to complete, then
// confirms the resulting sweep funds are reflected in the daemon's
// confirmed backing-wallet balance.
func waitForUnrollSweepToWallet(t *testing.T, h *harness.ArkHarness,
	daemon *harness.ClientDaemonHarness,
	client daemonrpc.DaemonServiceClient, outpoint string,
	maxSweptValueSat int64,
	initialWalletUTXOs map[wire.OutPoint]btcutil.Amount) string {

	t.Helper()

	waitForUnrollJobCompletion(t, h, client, outpoint)

	sweptOutpoint := waitForNewConfirmedWalletUTXOWithMaxValue(
		t, daemon, initialWalletUTXOs, maxSweptValueSat,
	)

	return sweptOutpoint.String()
}
