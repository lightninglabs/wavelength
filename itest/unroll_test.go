//go:build itest

package itest

import (
	"context"
	"encoding/hex"
	"sort"
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
	// 16 blocks is the smallest value that satisfies the fraud
	// responder's startup gate at the default connector tree shape
	// (max depth 5 + 6-block safety margin requires > 11), while still
	// keeping CSV-mining time short for unroll integration tests.
	testVTXOExitDelay = 16

	// unrollMempoolStallBlockIntervalFast is the heartbeat used by
	// the LND-backed client wallet, where the daemon receives CPFP
	// packages via the local LND chain backend's direct package
	// relay path. A short heartbeat is safe because the round-trip
	// latency between the daemon and bitcoind is dominated by
	// in-process queueing, not P2P propagation.
	unrollMempoolStallBlockIntervalFast = 2 * time.Second

	// unrollMempoolStallBlockIntervalSlow is the heartbeat used by
	// the embedded-wallet backends (btcwallet, lwwallet). These
	// propagate through neutrino / electrs HTTP polling instead of
	// direct package relay, so block edges also drive the daemon's
	// txconfirm rebroadcast and fee-bump decisions. A short
	// heartbeat at this layer races the daemon's processing of the
	// prior tip and stalls the chain-depth-2 multi-input lineage
	// at MATERIALIZING under CI load. 10s matches the historical
	// default that was known to be stable on these backends.
	unrollMempoolStallBlockIntervalSlow = 10 * time.Second

	// unrollNoProgressTimeout fails the helper if the public unroll
	// status and bitcoind mempool both stop showing progress. The
	// external status can remain MATERIALIZING while internal recovery
	// nodes confirm one by one, so this is separate from the overall
	// wall-clock bound. The btcwallet / neutrino backend under CI
	// parallel load can sit in MATERIALIZING for several minutes
	// between visible-progress events, so the timer needs comfortable
	// headroom even though the lnd backend usually advances every
	// few seconds.
	unrollNoProgressTimeout = 6 * time.Minute

	// unrollOverallTimeout is a hard cap to prevent a broken test from
	// mining indefinitely even if intermittent mempool activity keeps
	// resetting the no-progress timeout. Sized for the worst observed
	// CI wall-clock on btcwallet, where a chain-depth-2 multi-input
	// lineage spends 10+ minutes total in MATERIALIZING under parallel
	// load even though every individual broadcast is making progress.
	unrollOverallTimeout = 15 * time.Minute
)

// newUnrollHarness creates a test harness with a reduced VTXO exit delay
// so that the unroll CSV wait completes in a reasonable number of blocks.
func newUnrollHarness(t *testing.T) *harness.ArkHarness {
	t.Helper()

	return newUnrollHarnessWithMutator(t, nil)
}

// newUnrollHarnessWithMutator creates a reduced-delay harness and applies an
// optional operator config mutator after the default unroll test settings.
func newUnrollHarnessWithMutator(t *testing.T,
	mutator func(*darepo.Config)) *harness.ArkHarness {

	t.Helper()

	clientOpts := client_harness.DefaultOptions()
	clientOpts.GroupName = t.Name()
	clientOpts.StartTapd = false

	h := harness.NewArkHarness(t, &harness.ArkHarnessOptions{
		ClientOptions: &clientOpts,
		OperatorConfigMutator: func(cfg *darepo.Config) {
			cfg.Rounds.VTXOExitDelay = testVTXOExitDelay
			if mutator != nil {
				mutator(cfg)
			}
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
		t, h, alice.RPCClient, operatorInfo.MinConfirmations, 100_000,
	)
	require.NotNil(t, aliceVTXO)
	require.True(t, aliceVTXO.AmountSat > 0)

	// Fund Alice's wallet for CPFP fees during unroll.
	h.FundClientWallet(alice, btcutil.SatoshiPerBitcoin)

	h.Logf(
		"Alice has live VTXO: outpoint=%s amount=%d",
		aliceVTXO.Outpoint, aliceVTXO.AmountSat,
	)

	initialWalletUTXOs := confirmedWalletUTXOValues(t, alice)

	// Trigger the unilateral exit via the Unroll RPC.
	unrollResp, err := alice.RPCClient.Unroll(
		t.Context(), &daemonrpc.UnrollRequest{
			Outpoint: aliceVTXO.Outpoint,
		},
	)
	require.NoError(t, err, "Unroll RPC should succeed")
	require.True(
		t, unrollResp.Created, "should have created a new unroll job",
	)
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
	require.False(
		t, unrollResp2.Created,
		"second call should return Created=false",
	)

	// The unroll job is created asynchronously through the chain
	// resolver, so poll until it appears.
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(
			t.Context(), defaultSmallTimeout,
		)
		defer cancel()

		resp, err := alice.RPCClient.GetUnrollStatus(
			ctx, &daemonrpc.GetUnrollStatusRequest{
				Outpoint: aliceVTXO.Outpoint,
			},
		)

		return err == nil && resp.Found
	}, 30*time.Second, 250*time.Millisecond,
		"unroll job should be found for %s",
		aliceVTXO.Outpoint)

	h.Logf("VTXO in UNILATERAL_EXIT, unroll job found")

	// Mine blocks and wait for the unroll job to reach completion.
	sweptOutpoint := waitForUnrollSweepToWallet(
		t, h, alice, alice.RPCClient, aliceVTXO.Outpoint,
		aliceVTXO.AmountSat, initialWalletUTXOs,
	)

	h.Logf(
		"Unroll completed: VTXO %s swept back to wallet UTXO %s",
		aliceVTXO.Outpoint, sweptOutpoint,
	)
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
		t, h, alice.RPCClient, operatorInfo.MinConfirmations, 100_000,
	)
	require.NotNil(t, aliceVTXO)

	h.FundClientWallet(alice, btcutil.SatoshiPerBitcoin)

	h.Logf(
		"Alice VTXO: outpoint=%s amount=%d", aliceVTXO.Outpoint,
		aliceVTXO.AmountSat,
	)

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

	h.Logf(
		"Unroll completed: VTXO %s swept back to wallet UTXO %s",
		aliceVTXO.Outpoint, sweptOutpoint,
	)
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
		t, h, alice.RPCClient, operatorInfo.MinConfirmations, 100_000,
	)

	// Fund Bob's wallet for CPFP fees during unroll.
	h.FundClientWallet(bob, btcutil.SatoshiPerBitcoin)

	// Bob creates a receive script for the OOR transfer.
	bobLiveBefore := outpointSet(listLiveVTXOs(t, bob.RPCClient))

	recvResp, err := bob.RPCClient.NewReceiveScript(
		t.Context(), &daemonrpc.NewReceiveScriptRequest{
			Label: "itest-unroll-oor",
		},
	)
	require.NoError(t, err)
	require.NotEmpty(t, recvResp.PubkeyXonlyHex)

	recipientPubkey, err := hex.DecodeString(recvResp.PubkeyXonlyHex)
	require.NoError(t, err)

	// Alice sends OOR to Bob (full amount).
	sendAmount := aliceLiveVTXO.AmountSat
	sendResp, err := alice.RPCClient.SendOOR(
		t.Context(), &daemonrpc.SendOORRequest{
			Recipient: &daemonrpc.Output{
				Destination: &daemonrpc.Output_Pubkey{
					Pubkey: recipientPubkey,
				},
				AmountSat: sendAmount,
			},
		},
	)
	require.NoError(t, err)
	require.Equal(t, "submitted", sendResp.Status)

	h.Logf(
		"OOR transfer submitted: session=%s amount=%d",
		sendResp.SessionId, sendAmount,
	)

	// Wait for Bob to receive the OOR VTXO.
	receivedVTXO := waitForNewLiveVTXOWithAmount(
		t, bob.RPCClient, bobLiveBefore, sendAmount,
	)
	require.NotNil(t, receivedVTXO)

	h.Logf(
		"Bob received OOR VTXO: outpoint=%s amount=%d round_id=%s "+
			"commitment_txid=%s chain_depth=%d",
		receivedVTXO.Outpoint, receivedVTXO.AmountSat,
		receivedVTXO.RoundId, receivedVTXO.CommitmentTxid,
		receivedVTXO.ChainDepth,
	)

	h.Logf(
		"Alice source VTXO: outpoint=%s round_id=%s "+
			"commitment_txid=%s chain_depth=%d",
		aliceLiveVTXO.Outpoint, aliceLiveVTXO.RoundId,
		aliceLiveVTXO.CommitmentTxid, aliceLiveVTXO.ChainDepth,
	)

	initialWalletUTXOs := confirmedWalletUTXOValues(t, bob)

	// Verify the OOR-derived VTXO shares the same parent tree
	// metadata as the source VTXO.
	require.Equal(
		t, aliceLiveVTXO.RoundId, receivedVTXO.RoundId,
		"OOR VTXO should share the same round ID",
	)

	require.Equal(
		t, aliceLiveVTXO.CommitmentTxid, receivedVTXO.CommitmentTxid,
		"OOR VTXO should share the same commitment txid as the "+
			"source (same-parent-tree constraint)",
	)

	// A round-born VTXO has ChainDepth 0. One OOR hop from it
	// should yield ChainDepth 1.
	require.Equal(
		t, uint32(0), aliceLiveVTXO.ChainDepth,
		"source round-born VTXO should have chain depth 0",
	)

	require.Equal(
		t, uint32(1), receivedVTXO.ChainDepth, "OOR-derived VTXO "+
			"should have chain depth 1 (one hop from on-chain "+
			"commitment)",
	)

	// Trigger unilateral exit on Bob's OOR-derived VTXO.
	unrollResp, err := bob.RPCClient.Unroll(
		t.Context(), &daemonrpc.UnrollRequest{
			Outpoint: receivedVTXO.Outpoint,
		},
	)
	require.NoError(t, err)
	require.True(t, unrollResp.Created)

	h.Logf(
		"Unroll job created for OOR VTXO: actor_id=%s",
		unrollResp.ActorId,
	)

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

	// Verify sweep observability beyond control-plane completion:
	// the GetUnrollStatus response should expose the sweep txid.
	statusResp, err := bob.RPCClient.GetUnrollStatus(
		t.Context(), &daemonrpc.GetUnrollStatusRequest{
			Outpoint: receivedVTXO.Outpoint,
		},
	)
	require.NoError(t, err)
	require.True(t, statusResp.Found)
	require.Equal(
		t, daemonrpc.UnrollJobStatus_UNROLL_JOB_STATUS_COMPLETED,
		statusResp.Status, "unroll job should be in COMPLETED state",
	)
	require.NotEmpty(
		t, statusResp.SweepTxid,
		"completed unroll should expose the sweep txid",
	)

	h.Logf(
		"Unroll completed: OOR VTXO %s swept to wallet UTXO %s, "+
			"sweep_txid=%s", receivedVTXO.Outpoint, sweptOutpoint,
		statusResp.SweepTxid,
	)
}

// waitForUnrollJobCompletion drives regtest mining in lockstep with the
// unroll FSM until the job reaches COMPLETED status. Materialization and sweep
// phases only need blocks after the next transaction reaches the mempool, while
// the CSV phase needs empty blocks to mature the exit delay. Keeping those
// phases separate prevents the test from flooding every block subscriber while
// the unroll actor is still preparing its next transaction.
func waitForUnrollJobCompletion(t *testing.T, h *harness.ArkHarness,
	client daemonrpc.DaemonServiceClient, outpoint string) {

	t.Helper()

	overallDeadline := time.Now().Add(unrollOverallTimeout)
	progressDeadline := time.Now().Add(unrollNoProgressTimeout)
	lastBlockDrive := time.Now()
	var lastStatus daemonrpc.UnrollJobStatus
	var loggedStatus bool

	// Give the actor system time to submit initial recovery
	// packages before mining starts.
	time.Sleep(2 * time.Second)

	for time.Now().Before(overallDeadline) {
		if time.Now().After(progressDeadline) {
			require.Failf(
				t, "unroll job stopped progressing", "unroll"+
					" job stopped progressing for %s "+
					"(last status: %s)", outpoint,
				lastStatus,
			)
		}

		resp, ok := pollUnrollJobStatus(t, client, outpoint)
		if !ok {
			time.Sleep(pollInterval)
			continue
		}

		if !loggedStatus || resp.Status != lastStatus {
			h.Logf(
				"Unroll job %s status: %s", outpoint,
				resp.Status,
			)
			lastStatus = resp.Status
			loggedStatus = true
			lastBlockDrive = time.Now()
			progressDeadline = time.Now().Add(
				unrollNoProgressTimeout,
			)
		}

		isFailed := resp.Status ==
			daemonrpc.UnrollJobStatus_UNROLL_JOB_STATUS_FAILED
		if isFailed {
			t.Fatalf("Unroll job failed: %s", resp.LastError)
		}

		isCompleted := resp.Status ==
			daemonrpc.UnrollJobStatus_UNROLL_JOB_STATUS_COMPLETED
		if isCompleted {
			return
		}

		csvPending := daemonrpc.
			UnrollJobStatus_UNROLL_JOB_STATUS_CSV_PENDING
		if resp.Status == csvPending {
			h.Logf(
				"Unroll job %s is CSV pending; mining 1 block",
				outpoint,
			)
			h.GenerateAndWait(1)
			lastBlockDrive = time.Now()
			progressDeadline = time.Now().Add(
				unrollNoProgressTimeout,
			)

			continue
		}

		if mineMempoolBlock(t, h, outpoint) {
			lastBlockDrive = time.Now()
			progressDeadline = time.Now().Add(
				unrollNoProgressTimeout,
			)

			continue
		}

		if shouldMineUnrollHeartbeat(
			h, resp.Status, lastBlockDrive,
		) {

			h.Logf(
				"Unroll job %s has no mempool tx; mining 1 "+
					"heartbeat block", outpoint,
			)
			h.GenerateAndWait(1)
			lastBlockDrive = time.Now()
			// A heartbeat block is real driver of the
			// embedded-wallet rebroadcast / fee-bump
			// pipeline, so it counts as progress for the
			// purpose of the no-progress watchdog. The
			// overall test budget (unrollOverallTimeout)
			// still bounds a genuinely stuck run. Without
			// this reset, a deep btcwallet unroll lineage
			// that walks through many empty-mempool ticks
			// between visible-progress events trips the
			// 6-minute no-progress timer while it is in
			// fact making protocol-level progress block by
			// block.
			progressDeadline = time.Now().Add(
				unrollNoProgressTimeout,
			)

			continue
		}

		time.Sleep(pollInterval)
	}

	require.Failf(
		t, "unroll job never completed", "unroll job never "+
			"completed for %s before %s (last status: %s)",
		outpoint, unrollOverallTimeout, lastStatus,
	)
}

// pollUnrollJobStatus returns the current unroll status if the daemon has
// registered the job.
func pollUnrollJobStatus(t *testing.T, client daemonrpc.DaemonServiceClient,
	outpoint string) (*daemonrpc.GetUnrollStatusResponse, bool) {

	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), defaultSmallTimeout)
	defer cancel()

	resp, err := client.GetUnrollStatus(
		ctx, &daemonrpc.GetUnrollStatusRequest{
			Outpoint: outpoint,
		},
	)
	if err != nil || !resp.Found {
		return nil, false
	}

	return resp, true
}

// shouldMineUnrollHeartbeat returns true when the helper should mine one empty
// block to advance block-driven broadcast or fee-bump logic. The cadence
// depends on the client wallet backend: LND tolerates a tight 2s heartbeat
// because the daemon receives CPFP packages via direct local relay, while
// the embedded-wallet backends (btcwallet, lwwallet) need a longer 10s
// interval because their neutrino / electrs propagation cannot keep up with
// a fast tick under CI parallel load.
func shouldMineUnrollHeartbeat(h *harness.ArkHarness,
	status daemonrpc.UnrollJobStatus, lastBlockDrive time.Time) bool {

	switch status {
	case daemonrpc.UnrollJobStatus_UNROLL_JOB_STATUS_MATERIALIZING,
		daemonrpc.UnrollJobStatus_UNROLL_JOB_STATUS_SWEEPING:

	default:
		return false
	}

	interval := unrollMempoolStallBlockIntervalFast
	if h.ClientWalletBackend() != harness.ClientWalletBackendLND {
		interval = unrollMempoolStallBlockIntervalSlow
	}

	return time.Since(lastBlockDrive) >= interval
}

// mineMempoolBlock mines one block if bitcoind has transactions waiting. The
// short grace period gives package relay and confirmation watchers time to
// observe the full package before the block is generated.
func mineMempoolBlock(t *testing.T, h *harness.ArkHarness,
	outpoint string) bool {

	t.Helper()

	txIDs := sortedMempoolTxIDs(h)
	if len(txIDs) == 0 {
		return false
	}

	time.Sleep(confirmationGrace)

	txIDs = sortedMempoolTxIDs(h)
	if len(txIDs) == 0 {
		return false
	}

	h.Logf(
		"Unroll job %s mining %d mempool tx(s): %v", outpoint,
		len(txIDs), txIDs,
	)
	h.GenerateAndWait(1)

	return true
}

// sortedMempoolTxIDs returns the current mempool transaction IDs in stable
// order so log lines are deterministic across runs.
func sortedMempoolTxIDs(h *harness.ArkHarness) []string {
	h.T.Helper()

	txIDs := h.MempoolTxIDs()
	sort.Strings(txIDs)

	return txIDs
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
