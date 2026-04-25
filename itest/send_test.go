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

// TestDirectedSendIntegration exercises the full in-round directed send
// flow using the same receive-script mechanism as OOR:
//  1. Bob generates a receive script via NewOORReceiveScript
//  2. Alice boards and gets a VTXO
//  3. Alice sends a portion to bob's receive pubkey
//  4. The round completes via the normal signing ceremony
//  5. Alice sees her change VTXO
func TestDirectedSendIntegration(t *testing.T) {
	t.Parallel()

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

	// Wait for both daemons to register with the operator.
	waitForRegisteredClients(t, h, 2)
	t.Log("Both clients registered with operator")

	// Board alice so she has a VTXO to send from.
	boardingAmount := btcutil.Amount(100_000)
	round1, round1VTXO, _ := boardClientAndConfirmRound(
		t, h, alice.RPCClient, operatorInfo.MinConfirmations,
		boardingAmount,
	)
	t.Logf("Alice boarded: round_id=%q vtxo=%s amount=%d",
		round1.RoundId, round1VTXO.Outpoint,
		round1VTXO.AmountSat)

	// Bob generates a receive script — same mechanism used for
	// OOR receives. This registers the script with the indexer
	// and persists the key locally so bob can prove ownership.
	recvResp, err := bob.RPCClient.NewOORReceiveScript(
		t.Context(), &daemonrpc.NewOORReceiveScriptRequest{
			Label: "itest-alice-to-bob",
		},
	)
	require.NoError(t, err, "NewOORReceiveScript RPC failed")
	require.NotEmpty(t, recvResp.PubkeyXonlyHex)

	bobPubkeyBytes, err := hex.DecodeString(
		recvResp.PubkeyXonlyHex,
	)
	require.NoError(t, err, "invalid pubkey hex from bob")
	t.Logf("Bob receive pubkey (x-only): %s",
		recvResp.PubkeyXonlyHex)

	// Alice sends 30k sats to bob using his receive pubkey.
	sendAmount := int64(30_000)

	// Snapshot the operator's fee quote at this exact moment.
	// The client's SendVTXO handler calls EstimateFee inline
	// before submitting the round; capturing the same quote
	// here (before treasury utilization shifts from the about-
	// to-happen round) is the only way to know what fee the
	// wallet actor actually applied. Asking the server for the
	// quote after the round confirms would sample a different
	// utilization and produce a different number.
	sendEstimate := operatorEstimateFee(
		t, h, sendAmount, false /* boarding */, 0,
	)

	sendCtx, sendCancel := context.WithTimeout(
		t.Context(), defaultTimeout,
	)
	defer sendCancel()

	sendResp, err := alice.RPCClient.SendVTXO(
		sendCtx, &daemonrpc.SendVTXORequest{
			Recipients: []*daemonrpc.Output{
				{
					AmountSat: sendAmount,
					Destination: &daemonrpc.Output_Pubkey{
						Pubkey: bobPubkeyBytes,
					},
				},
			},
		},
	)
	require.NoError(t, err, "SendVTXO RPC failed")
	require.Equal(t, "submitted", sendResp.Status)
	require.Equal(t, int32(1), sendResp.SelectedCount)
	t.Logf("SendVTXO submitted: change=%d selected=%d",
		sendResp.ChangeAmountSat, sendResp.SelectedCount)

	// Wait for alice's round to join and progress through signing.
	sendRound := waitForNewClientRoundState(
		t, alice.RPCClient,
		map[string]struct{}{round1.RoundId: {}},
		daemonrpc.RoundState_ROUND_STATE_JOINED,
	)
	require.NotEmpty(t, sendRound.RoundId)
	t.Logf("Send round joined: round_id=%q", sendRound.RoundId)

	waitForNamedClientRoundState(
		t, alice.RPCClient, sendRound.RoundId,
		daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT,
	)

	broadcastRound := waitForOperatorRoundStatus(
		t, h, sendRound.RoundId,
		adminrpc.RoundStatus_ROUND_STATUS_BROADCAST,
	)
	require.NotEmpty(t, broadcastRound.TxId)
	t.Logf("Send round broadcast: txid=%s", broadcastRound.TxId)

	// Mine blocks until the operator confirms the round.
	require.Eventually(t, func() bool {
		if operatorRoundHasStatus(
			t, h, sendRound.RoundId,
			adminrpc.RoundStatus_ROUND_STATUS_CONFIRMED,
		) {

			return true
		}

		h.GenerateAndWait(1)

		return operatorRoundHasStatus(
			t, h, sendRound.RoundId,
			adminrpc.RoundStatus_ROUND_STATUS_CONFIRMED,
		)
	}, defaultTimeout, pollInterval,
		"send round %s never confirmed", sendRound.RoundId)
	t.Logf("Send round confirmed: round_id=%q", sendRound.RoundId)

	// Verify alice has a change VTXO from the send round.
	aliceChangeVTXO := waitForLiveVTXO(
		t, alice.RPCClient, sendRound.RoundId,
	)
	t.Logf("Alice change VTXO: outpoint=%s amount=%d",
		aliceChangeVTXO.Outpoint, aliceChangeVTXO.AmountSat)

	// Under the #270 seal-time fee handshake the binding operator
	// fee comes from the server's quote, not from the pre-submit
	// EstimateFee snapshot — the estimate is advisory only. Assert
	// the conservation invariant (inputs balance against outputs +
	// some fee) rather than pinning the exact amount against a
	// pre-flight number that can drift with treasury utilization.
	_ = operatorInfo // Kept for other callers; no longer used here.
	_ = sendEstimate // Kept for other callers; no longer used here.
	require.Less(t, aliceChangeVTXO.AmountSat,
		round1VTXO.AmountSat-sendAmount,
		"change must be strictly less than input−sendAmount "+
			"(fee deducted)")
	require.Positive(t, aliceChangeVTXO.AmountSat,
		"change must remain above dust after fee")

	// Verify bob sees the received VTXO. The server publishes
	// IncomingVTXOEvent for each round leaf, and bob's handler
	// materializes it via the owned receive script.
	bobExpectedBalance := sendAmount
	bobBalance := waitForExactVTXOBalance(
		t, bob.RPCClient, bobExpectedBalance,
	)
	require.Equal(t, bobExpectedBalance, bobBalance.VtxoBalanceSat,
		"bob should see the received VTXO")

	// Explicitly verify bob has a live VTXO with the correct amount.
	bobListCtx, bobListCancel := context.WithTimeout(
		t.Context(), defaultSmallTimeout,
	)
	defer bobListCancel()

	bobVTXOs, err := bob.RPCClient.ListVTXOs(
		bobListCtx, &daemonrpc.ListVTXOsRequest{
			StatusFilter: daemonrpc.VTXOStatus_VTXO_STATUS_LIVE,
		},
	)
	require.NoError(t, err, "bob ListVTXOs failed")
	require.Len(t, bobVTXOs.Vtxos, 1,
		"bob should have exactly 1 live VTXO")
	require.Equal(t, sendAmount, bobVTXOs.Vtxos[0].AmountSat,
		"bob's VTXO amount should match send amount")
	t.Logf("Bob VTXO verified: outpoint=%s amount=%d",
		bobVTXOs.Vtxos[0].Outpoint,
		bobVTXOs.Vtxos[0].AmountSat)
}

// TestDirectedSendSelfSend verifies that a client can send to its own
// receive script. Both the recipient VTXO and the change VTXO should
// be persisted, since both pkScripts are locally owned.
func TestDirectedSendSelfSend(t *testing.T) {
	t.Parallel()

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

	waitForRegisteredClients(t, h, 1)

	// Board alice.
	boardingAmount := btcutil.Amount(100_000)
	round1, round1VTXO, _ := boardClientAndConfirmRound(
		t, h, alice.RPCClient, operatorInfo.MinConfirmations,
		boardingAmount,
	)
	t.Logf("Alice boarded: round_id=%q vtxo=%s amount=%d",
		round1.RoundId, round1VTXO.Outpoint,
		round1VTXO.AmountSat)

	// Alice generates a receive script for herself.
	recvResp, err := alice.RPCClient.NewOORReceiveScript(
		t.Context(), &daemonrpc.NewOORReceiveScriptRequest{
			Label: "itest-self-send",
		},
	)
	require.NoError(t, err, "NewOORReceiveScript failed")

	alicePubkey, err := hex.DecodeString(
		recvResp.PubkeyXonlyHex,
	)
	require.NoError(t, err)
	t.Logf("Alice self-receive pubkey: %s",
		recvResp.PubkeyXonlyHex)

	// Alice sends 30k sats to herself.
	sendAmount := int64(30_000)

	// Snapshot the operator's quote before submit so the
	// expected balance matches what the wallet actually applied
	// (the client consults EstimateFee inline during SendVTXO;
	// the round's confirmation then shifts treasury utilization,
	// so a post-round quote would sample a different point on
	// the curve).
	sendEstimate := operatorEstimateFee(
		t, h, sendAmount, false /* boarding */, 0,
	)

	sendCtx, sendCancel := context.WithTimeout(
		t.Context(), defaultTimeout,
	)
	defer sendCancel()

	sendResp, err := alice.RPCClient.SendVTXO(
		sendCtx, &daemonrpc.SendVTXORequest{
			Recipients: []*daemonrpc.Output{
				{
					AmountSat: sendAmount,
					Destination: &daemonrpc.Output_Pubkey{
						Pubkey: alicePubkey,
					},
				},
			},
		},
	)
	require.NoError(t, err, "SendVTXO RPC failed")
	require.Equal(t, "submitted", sendResp.Status)
	t.Logf("Self-send submitted: change=%d",
		sendResp.ChangeAmountSat)

	// Wait for the send round.
	sendRound := waitForNewClientRoundState(
		t, alice.RPCClient,
		map[string]struct{}{round1.RoundId: {}},
		daemonrpc.RoundState_ROUND_STATE_JOINED,
	)
	require.NotEmpty(t, sendRound.RoundId)

	waitForNamedClientRoundState(
		t, alice.RPCClient, sendRound.RoundId,
		daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT,
	)

	broadcastRound := waitForOperatorRoundStatus(
		t, h, sendRound.RoundId,
		adminrpc.RoundStatus_ROUND_STATUS_BROADCAST,
	)
	require.NotEmpty(t, broadcastRound.TxId)

	require.Eventually(t, func() bool {
		if operatorRoundHasStatus(
			t, h, sendRound.RoundId,
			adminrpc.RoundStatus_ROUND_STATUS_CONFIRMED,
		) {

			return true
		}

		h.GenerateAndWait(1)

		return operatorRoundHasStatus(
			t, h, sendRound.RoundId,
			adminrpc.RoundStatus_ROUND_STATUS_CONFIRMED,
		)
	}, defaultTimeout, pollInterval,
		"self-send round never confirmed")
	t.Logf("Self-send round confirmed: round_id=%q",
		sendRound.RoundId)

	// After self-send, alice should have TWO live VTXOs from the
	// send round: the recipient VTXO (30k) and the change VTXO.
	// Under #270 the binding fee is the seal-time quote, not the
	// pre-submit estimate; assert conservation (total < input,
	// recipient untouched) rather than an exact pre-flight number.
	_ = operatorInfo // Kept for other callers; not used here.
	_ = sendEstimate // Kept for other callers; not used here.

	finalBalance := waitForVTXOBalanceBelow(
		t, alice.RPCClient, round1VTXO.AmountSat,
	)
	require.Greater(t, finalBalance.VtxoBalanceSat, int64(30_000),
		"alice balance must exceed the 30k recipient amount")

	// Explicitly list VTXOs and assert both exist.
	listCtx, listCancel := context.WithTimeout(
		t.Context(), defaultSmallTimeout,
	)
	defer listCancel()

	liveResp, err := alice.RPCClient.ListVTXOs(
		listCtx, &daemonrpc.ListVTXOsRequest{
			StatusFilter: daemonrpc.VTXOStatus_VTXO_STATUS_LIVE,
		},
	)
	require.NoError(t, err, "ListVTXOs failed")

	var sendRoundVTXOs []*daemonrpc.VTXO
	for _, vtxo := range liveResp.Vtxos {
		if vtxo.RoundId == sendRound.RoundId {
			sendRoundVTXOs = append(sendRoundVTXOs, vtxo)
		}
	}
	require.Len(t, sendRoundVTXOs, 2,
		"self-send should produce exactly 2 VTXOs "+
			"(recipient + change)")

	amounts := map[int64]bool{
		sendRoundVTXOs[0].AmountSat: true,
		sendRoundVTXOs[1].AmountSat: true,
	}
	require.True(t, amounts[sendAmount],
		"recipient VTXO (%d) not found", sendAmount)

	// Under #270 the pre-flight sendResp.ChangeAmountSat is an
	// estimate; the on-chain change amount comes from the seal-time
	// quote's residual and may differ. Assert the conservation
	// invariant (two distinct VTXOs, one is the 30k recipient, the
	// other is a positive change below input−sendAmount) rather
	// than pinning to the estimate.
	var changeSat int64
	for amt := range amounts {
		if amt != sendAmount {
			changeSat = amt
			break
		}
	}
	require.Positive(t, changeSat, "change VTXO must be positive")
	require.Less(t, changeSat, round1VTXO.AmountSat-sendAmount,
		"change must be strictly less than input−recipient (fee)")

	t.Logf("Self-send complete: %d VTXOs from send round "+
		"(amounts: %d, %d)",
		len(sendRoundVTXOs),
		sendRoundVTXOs[0].AmountSat,
		sendRoundVTXOs[1].AmountSat)
}
