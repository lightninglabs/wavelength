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
