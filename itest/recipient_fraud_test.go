//go:build itest

package itest

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo/harness"
	"github.com/stretchr/testify/require"
)

// TestRecipientFraudWatcherBroadcastsArkAfterRestart verifies the recipient
// daemon's fraud watcher is restored from durable VTXO/OOR state after restart.
// Once the previous owner's source VTXO is revealed and the operator
// checkpoint confirms, the restarted recipient must broadcast the matching ark
// transaction without harness-side intervention.
func TestRecipientFraudWatcherBroadcastsArkAfterRestart(t *testing.T) {
	h := newUnrollHarness(t)
	h.FundOperatorLNDTaproot(btcutil.SatoshiPerBitcoin)

	flow := setupRecipientFraudFlow(t, h)

	// Fund fee UTXOs before restart. The fraud watcher uses txconfirm to
	// CPFP the ark broadcast after the operator checkpoint confirms.
	h.FundClientWalletN(flow.bob, btcutil.SatoshiPerBitcoin/2, 3)

	oldRPCAddr := flow.bob.RPCAddr
	flow.bob = h.RestartClientDaemon("bob")
	t.Logf(
		"restarted recipient daemon: old_rpc=%s new_rpc=%s "+
			"received_outpoint=%s", oldRPCAddr, flow.bob.RPCAddr,
		flow.bobReceived.Outpoint,
	)

	waitForDaemonInfoReachable(t, flow.bob.RPCClient)
	waitForRegisteredClients(t, h, 2)

	driveRecipientFraudThroughArkConfirm(t, h, flow)
	waitForFraudTriggeredUnroll(
		t, flow.bob.RPCClient, flow.bobReceived.Outpoint,
	)
	waitForUnrollJobCompletion(
		t, h, flow.bob.RPCClient, flow.bobReceived.Outpoint,
	)
}

// TestRecipientFraudDefersToOperator verifies that when the operator's fraud
// responder is healthy, the recipient defers the checkpoint broadcast and only
// pays for its own ark and sweep transactions. The check is a wallet-UTXO
// accounting one: every recipient broadcast through txconfirm consumes exactly
// one wallet UTXO for the CPFP fee input, so the count of pre-existing wallet
// UTXOs that disappear over the run is the count of distinct broadcasts the
// recipient performed. A deferring recipient broadcasts at most ark + sweep —
// two — so a baseline-UTXO consumption greater than two would mean the
// recipient also funded a redundant checkpoint CPFP.
func TestRecipientFraudDefersToOperator(t *testing.T) {
	h := newUnrollHarness(t)
	h.FundOperatorLNDTaproot(btcutil.SatoshiPerBitcoin)

	flow := setupRecipientFraudFlow(t, h)

	// Fund fee UTXOs before the fraud event so the recipient's own
	// broadcasts have known wallet inputs to choose from.
	h.FundClientWalletN(flow.bob, btcutil.SatoshiPerBitcoin/2, 3)

	// Snapshot the recipient's confirmed wallet UTXOs after funding.
	// Subsequent CPFP broadcasts consume entries from this baseline.
	baselineUTXOs := confirmedWalletUTXOValues(t, flow.bob)
	require.GreaterOrEqual(
		t, len(baselineUTXOs), 3, "need 3+ baseline UTXOs so an "+
			"extra checkpoint CPFP would have a wallet input "+
			"to consume",
	)

	driveRecipientFraudThroughArkConfirm(t, h, flow)
	waitForFraudTriggeredUnroll(
		t, flow.bob.RPCClient, flow.bobReceived.Outpoint,
	)
	waitForUnrollJobCompletion(
		t, h, flow.bob.RPCClient, flow.bobReceived.Outpoint,
	)

	// Each distinct recipient parent broadcast (ark, sweep) consumes one
	// baseline UTXO via the txconfirm CPFP fee-input reservation. A
	// deferring recipient never funds a checkpoint, so at most two
	// baseline UTXOs may disappear.
	final := confirmedWalletUTXOValues(t, flow.bob)
	consumed := 0
	for op := range baselineUTXOs {
		if _, ok := final[op]; !ok {
			consumed++
		}
	}
	require.LessOrEqual(
		t, consumed, 2, "recipient consumed %d baseline wallet "+
			"UTXOs; expected <=2 (ark + sweep CPFP). Excess "+
			"implies recipient also funded a redundant "+
			"checkpoint CPFP", consumed,
	)
}

// recipientFraudFlow groups the daemons and OOR'd VTXO that every recipient
// fraud-response itest in this file builds on top of.
type recipientFraudFlow struct {
	alice          *harness.ClientDaemonHarness
	bob            *harness.ClientDaemonHarness
	aliceLiveVTXO  *daemonrpc.VTXO
	bobReceived    *daemonrpc.VTXO
	sourceOutpoint wire.OutPoint
}

// setupRecipientFraudFlow boards Alice, OORs a single VTXO from Alice to Bob,
// and returns the daemons plus the descriptors needed to drive the recipient
// fraud-response paths.
func setupRecipientFraudFlow(t *testing.T,
	h *harness.ArkHarness) recipientFraudFlow {

	t.Helper()

	alice := h.StartClientDaemon("alice")
	bob := h.StartClientDaemon("bob")
	operatorInfo := getOperatorInfo(t, h)

	waitForRegisteredClients(t, h, 2)

	_, aliceLiveVTXO, _ := boardClientAndConfirmRound(
		t, h, alice.RPCClient, operatorInfo.MinConfirmations, 100_000,
	)

	bobLiveBefore := outpointSet(listLiveVTXOs(t, bob.RPCClient))
	bobRecv, err := bob.RPCClient.NewReceiveScript(
		t.Context(), &daemonrpc.NewReceiveScriptRequest{
			Label: "itest-recipient-fraud",
		},
	)
	require.NoError(t, err)

	bobPubkey, err := hex.DecodeString(bobRecv.PubkeyXonlyHex)
	require.NoError(t, err)

	sendAmount := aliceLiveVTXO.AmountSat
	sendResp, err := alice.RPCClient.SendOOR(
		t.Context(), &daemonrpc.SendOORRequest{
			Recipient: &daemonrpc.Output{
				Destination: &daemonrpc.Output_Pubkey{
					Pubkey: bobPubkey,
				},
				AmountSat: sendAmount,
			},
		},
	)
	require.NoError(t, err)
	require.Equal(t, "submitted", sendResp.Status)

	bobReceived := waitForNewLiveVTXOWithAmount(
		t, bob.RPCClient, bobLiveBefore, sendAmount,
	)
	require.NotNil(t, bobReceived)

	return recipientFraudFlow{
		alice:          alice,
		bob:            bob,
		aliceLiveVTXO:  aliceLiveVTXO,
		bobReceived:    bobReceived,
		sourceOutpoint: mustParseOutpoint(t, aliceLiveVTXO.Outpoint),
	}
}

// driveRecipientFraudThroughArkConfirm force-broadcasts Alice's source VTXO
// onto chain, mines the operator's responding checkpoint, and waits for Bob's
// recipient fraud watcher to broadcast and confirm the matching ark
// transaction. Returns once the ark tx is confirmed.
func driveRecipientFraudThroughArkConfirm(t *testing.T, h *harness.ArkHarness,
	flow recipientFraudFlow) {

	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()

	forceBroadcastLineageToOutpoint(
		t, h, flow.bob, flow.bobReceived.Outpoint,
		flow.aliceLiveVTXO.Outpoint,
	)

	checkpointTxid, checkpointTx := waitForMempoolSpend(
		t, h, nil, flow.sourceOutpoint,
	)
	require.Equal(
		t, flow.sourceOutpoint, checkpointTx.TxIn[0].PreviousOutPoint,
	)

	h.Generate(1)
	require.NoError(
		t, h.WaitForTxConfirmed(
			ctx, checkpointTxid, 30*time.Second,
		),
	)

	checkpointOutpoint := wire.OutPoint{
		Hash:  checkpointTxid,
		Index: 0,
	}
	bobOutpoint := mustParseOutpoint(t, flow.bobReceived.Outpoint)

	arkTxid, arkTx := waitForSpendOnChainOrMempool(
		t, h, checkpointOutpoint, map[string]struct{}{
			checkpointTxid.String(): {},
		},
	)
	require.Equal(t, bobOutpoint.Hash, arkTxid)
	require.Equal(t, checkpointOutpoint,
		arkTx.TxIn[0].PreviousOutPoint)
	require.Less(t, int(bobOutpoint.Index), len(arkTx.TxOut))

	h.Generate(1)
	require.NoError(
		t, h.WaitForTxConfirmed(
			ctx, arkTxid, 30*time.Second,
		),
	)
}

// waitForFraudTriggeredUnroll waits until the recipient watcher-created unroll
// job is visible over the daemon RPC surface.
func waitForFraudTriggeredUnroll(t *testing.T,
	client daemonrpc.DaemonServiceClient, outpoint string) {

	t.Helper()

	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(
			t.Context(), defaultSmallTimeout,
		)
		defer cancel()

		resp, err := client.GetUnrollStatus(
			ctx, &daemonrpc.GetUnrollStatusRequest{
				Outpoint: outpoint,
			},
		)
		if err != nil {
			return false
		}

		return resp.Found
	}, defaultTimeout, pollInterval,
		"fraud watcher did not create unroll job for %s", outpoint)
}
