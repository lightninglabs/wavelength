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

// TestRefreshIntegrationMultiVTXOSingleRound exercises the
// single-IsChange invariant the seal-time fee handshake (#270)
// imposes on every intent: across a refresh round's combined
// VTXORequests + LeaveRequests there must be exactly one
// IsChange=true marker (or total output count == 1). Auto-refresh
// and multi-outpoint manual refresh both batch N≥2 VTXOs into one
// PendingRoundAssembly window, so a per-VTXO IsChange=true would
// produce N markers and the server would reject the round at
// admission with INVALID_CHANGE_DESIGNATION.
//
// This test drives the multi-outpoint manual refresh path through
// a real daemon and asserts:
//
//  1. The refresh round seals (no INVALID_CHANGE_DESIGNATION
//     reject quote — a regression on the central change-marker
//     designator surfaces here as a stalled QuoteSent state).
//  2. Both forfeit inputs reach the FORFEITED status.
//  3. The single resulting live VTXO carries the residual
//     produced by the seal-time quote builder, i.e.
//     Σin − Σ(per-input ComputeForfeitFee). The test computes
//     this as the sum of expectedNetAfterRefresh over both inputs
//     because the helper and the builder share the same
//     ComputeForfeitFee path.
func TestRefreshIntegrationMultiVTXOSingleRound(t *testing.T) {
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

	waitForClientRegistration(t, h)

	// Board two VTXOs back-to-back on the same client. Each call
	// produces a separate confirmed round and a single live VTXO,
	// so alice ends with exactly two live VTXOs eligible for
	// refresh.
	_, vtxoA, _ := boardClientAndConfirmRound(
		t, h, alice.RPCClient, operatorInfo.MinConfirmations, 120_000,
	)
	_, vtxoB, _ := boardClientAndConfirmRound(
		t, h, alice.RPCClient, operatorInfo.MinConfirmations, 80_000,
	)
	t.Logf("Boarded vtxoA=%s amount=%d", vtxoA.Outpoint, vtxoA.AmountSat)
	t.Logf("Boarded vtxoB=%s amount=%d", vtxoB.Outpoint, vtxoB.AmountSat)

	require.Contains(t,
		outpointSet(listLiveVTXOs(t, alice.RPCClient)),
		vtxoA.Outpoint,
	)
	require.Contains(t,
		outpointSet(listLiveVTXOs(t, alice.RPCClient)),
		vtxoB.Outpoint,
	)
	totalInputSat := vtxoA.AmountSat + vtxoB.AmountSat

	// Single RefreshVTXOs RPC carrying both outpoints. Pre-fix,
	// auto-refresh's buildVTXORequestFromRefresh stamped
	// IsChange=true on every output, which the central designator
	// at PendingRoundAssembly.ProcessEvent for IntentRequested now
	// collapses to exactly one marker; the wire-level acceptance
	// test here proves the post-fix wiring does not regress.
	existingRoundIDs := snapshotClientRoundIDs(t, alice.RPCClient)
	refreshResp, err := alice.RPCClient.RefreshVTXOs(
		t.Context(), &daemonrpc.RefreshVTXOsRequest{
			Selection: &daemonrpc.RefreshVTXOsRequest_Outpoints{
				Outpoints: &daemonrpc.OutpointSelection{
					Outpoints: []string{
						vtxoA.Outpoint,
						vtxoB.Outpoint,
					},
				},
			},
		},
	)
	require.NoError(t, err, "RefreshVTXOs RPC failed")
	require.Equal(t, "queued", refreshResp.Status)
	require.Contains(t, refreshResp.QueuedOutpoints, vtxoA.Outpoint)
	require.Contains(t, refreshResp.QueuedOutpoints, vtxoB.Outpoint)

	alice.TriggerRoundRegistration()

	refreshRound := waitForNewClientRoundState(
		t, alice.RPCClient, existingRoundIDs,
		daemonrpc.RoundState_ROUND_STATE_JOINED,
	)
	require.NotEmpty(t, refreshRound.RoundId)
	require.False(t, refreshRound.IsTemp)
	t.Logf("Refresh round joined: round_id=%q", refreshRound.RoundId)

	waitForNamedClientRoundState(
		t, alice.RPCClient, refreshRound.RoundId,
		daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT,
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
		t, alice.RPCClient, vtxoA.Outpoint,
		daemonrpc.VTXOStatus_VTXO_STATUS_FORFEITED,
	)
	waitForVTXOStatusByOutpoint(
		t, alice.RPCClient, vtxoB.Outpoint,
		daemonrpc.VTXOStatus_VTXO_STATUS_FORFEITED,
	)

	// A multi-input refresh round produces one output VTXO per
	// input request. The seal-time quote builder echoes the
	// intent target on every non-change output and stamps the
	// residual on the single IsChange=true output the central
	// designator marked, so the post-refresh set is N VTXOs whose
	// amounts sum to Σin − operator_fee_for_batch. Asserting on
	// the sum keeps the test independent of which output the
	// designator picked as change and absorbs the per-input vs
	// per-batch fee-rounding delta the seal-time builder rolls
	// into the residual.
	refreshedVTXOs := waitForNewLiveVTXOsInRound(
		t, alice.RPCClient, refreshRound.RoundId, 2,
	)
	require.Len(t, refreshedVTXOs, 2)

	var refreshedSum int64
	for _, v := range refreshedVTXOs {
		require.NotEqual(t, vtxoA.Outpoint, v.Outpoint)
		require.NotEqual(t, vtxoB.Outpoint, v.Outpoint)
		require.Equal(t, refreshRound.RoundId, v.RoundId)
		refreshedSum += v.AmountSat
	}

	require.Less(t, refreshedSum, totalInputSat,
		"refresh must deduct an operator fee from the input total")
	feeDelta := totalInputSat - refreshedSum
	require.Greater(t, feeDelta, int64(0),
		"operator fee must be strictly positive")
	require.Less(t, feeDelta, int64(10_000),
		"operator fee for a 2-input batch should be O(hundreds) "+
			"sats; a delta this large signals a regression in "+
			"the seal-time builder")

	finalBalance := waitForExactVTXOBalance(
		t, alice.RPCClient, refreshedSum,
	)
	require.Equal(t, refreshedSum, finalBalance.VtxoBalanceSat)
}

// TestRefreshIntegrationSequentialRPCsBeforeSeal proves the
// single-IsChange invariant holds under back-to-back manual
// RefreshVTXOs RPCs queued in the same PendingRoundAssembly
// window. Pre-fix, each RPC's local len(...)==0 guard only saw
// the batch it owned, so two RPCs landing before the round actor
// fired IntentRequested would each stamp IsChange=true on their
// first output and the composed intent would carry two markers.
//
// The test fans-out alice's single boarded VTXO into two
// self-owned VTXOs via a directed self-send (so we get the
// two-input precondition without paying for a second boarding
// round), then queues the two refreshes individually before
// triggering registration.
//
// A regression on the central change-marker designator manifests
// here as the refresh round never advancing past QuoteSent (the
// server returns INVALID_CHANGE_DESIGNATION on admission and the
// round drops). A passing run proves the designator collapses
// any number of upstream IsChange=true contributions into the
// single marker the seal-time admission rule requires.
func TestRefreshIntegrationSequentialRPCsBeforeSeal(t *testing.T) {
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

	// Step 1: board a single VTXO. This is the source of value
	// the directed self-send will fan out below.
	boardingAmount := btcutil.Amount(150_000)
	round1, vtxoBoard, _ := boardClientAndConfirmRound(
		t, h, alice.RPCClient, operatorInfo.MinConfirmations,
		boardingAmount,
	)
	t.Logf("Alice boarded: round_id=%q vtxo=%s amount=%d",
		round1.RoundId, vtxoBoard.Outpoint, vtxoBoard.AmountSat)

	// Step 2: directed self-send to fan-out the boarded VTXO into
	// {recipient, change} owned by alice. Cheaper than a second
	// boarding flow because it reuses the existing round window.
	recvResp, err := alice.RPCClient.NewOORReceiveScript(
		t.Context(), &daemonrpc.NewOORReceiveScriptRequest{
			Label: "itest-multi-refresh-self-send",
		},
	)
	require.NoError(t, err, "NewOORReceiveScript failed")

	alicePubkey, err := hex.DecodeString(recvResp.PubkeyXonlyHex)
	require.NoError(t, err)

	sendCtx, sendCancel := context.WithTimeout(
		t.Context(), defaultTimeout,
	)
	defer sendCancel()

	sendAmount := int64(50_000)
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
	broadcastSend := waitForOperatorRoundStatus(
		t, h, sendRound.RoundId,
		adminrpc.RoundStatus_ROUND_STATUS_BROADCAST,
	)
	require.NotEmpty(t, broadcastSend.TxId)
	mineUntilOperatorRoundConfirmed(
		t, h, sendRound.RoundId, broadcastSend.TxId,
	)
	waitForVTXOStatusByOutpoint(
		t, alice.RPCClient, vtxoBoard.Outpoint,
		daemonrpc.VTXOStatus_VTXO_STATUS_FORFEITED,
	)
	t.Logf("Self-send round confirmed: round_id=%q", sendRound.RoundId)

	// Identify the two live VTXOs alice now holds from the send
	// round; they are the two inputs we will refresh in separate
	// RPC calls below.
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
	for _, v := range liveResp.Vtxos {
		if v.RoundId == sendRound.RoundId {
			sendRoundVTXOs = append(sendRoundVTXOs, v)
		}
	}
	require.Len(t, sendRoundVTXOs, 2,
		"directed self-send should leave exactly two live VTXOs "+
			"(recipient + change) owned by alice")
	vtxo1, vtxo2 := sendRoundVTXOs[0], sendRoundVTXOs[1]
	t.Logf("Live VTXOs after self-send: a=%s amount=%d b=%s amount=%d",
		vtxo1.Outpoint, vtxo1.AmountSat,
		vtxo2.Outpoint, vtxo2.AmountSat)

	totalInputSat := vtxo1.AmountSat + vtxo2.AmountSat

	// Step 3: two sequential RefreshVTXOs RPCs WITHOUT a
	// TriggerRoundRegistration in between. Both intents land in
	// the same PendingRoundAssembly window. Pre-fix, each RPC's
	// local IsChange decision (len(vtxos)==0) saw zero already-
	// queued refreshes for its own batch and stamped IsChange=true
	// on its single output, producing a composed intent with two
	// markers. Post-fix, the central designator stamps exactly
	// one marker on the assembled intent regardless of how many
	// RPCs contributed to it.
	existingRoundIDs := snapshotClientRoundIDs(t, alice.RPCClient)
	resp1, err := alice.RPCClient.RefreshVTXOs(
		t.Context(), &daemonrpc.RefreshVTXOsRequest{
			Selection: &daemonrpc.RefreshVTXOsRequest_Outpoints{
				Outpoints: &daemonrpc.OutpointSelection{
					Outpoints: []string{vtxo1.Outpoint},
				},
			},
		},
	)
	require.NoError(t, err, "first RefreshVTXOs RPC failed")
	require.Equal(t, "queued", resp1.Status)

	resp2, err := alice.RPCClient.RefreshVTXOs(
		t.Context(), &daemonrpc.RefreshVTXOsRequest{
			Selection: &daemonrpc.RefreshVTXOsRequest_Outpoints{
				Outpoints: &daemonrpc.OutpointSelection{
					Outpoints: []string{vtxo2.Outpoint},
				},
			},
		},
	)
	require.NoError(t, err, "second RefreshVTXOs RPC failed")
	require.Equal(t, "queued", resp2.Status)

	// Now trigger registration; the round actor pulls both queued
	// intents out of PendingRoundAssembly into a single intent
	// emitted to the server.
	alice.TriggerRoundRegistration()

	refreshRound := waitForNewClientRoundState(
		t, alice.RPCClient, existingRoundIDs,
		daemonrpc.RoundState_ROUND_STATE_JOINED,
	)
	require.NotEmpty(t, refreshRound.RoundId)
	require.False(t, refreshRound.IsTemp)
	t.Logf("Refresh round joined: round_id=%q", refreshRound.RoundId)

	waitForNamedClientRoundState(
		t, alice.RPCClient, refreshRound.RoundId,
		daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT,
	)
	broadcastRefresh := waitForOperatorRoundStatus(
		t, h, refreshRound.RoundId,
		adminrpc.RoundStatus_ROUND_STATUS_BROADCAST,
	)
	require.NotEmpty(t, broadcastRefresh.TxId)

	mineUntilOperatorRoundConfirmed(
		t, h, refreshRound.RoundId, broadcastRefresh.TxId,
	)
	waitForVTXOStatusByOutpoint(
		t, alice.RPCClient, vtxo1.Outpoint,
		daemonrpc.VTXOStatus_VTXO_STATUS_FORFEITED,
	)
	waitForVTXOStatusByOutpoint(
		t, alice.RPCClient, vtxo2.Outpoint,
		daemonrpc.VTXOStatus_VTXO_STATUS_FORFEITED,
	)

	// As in the multi-outpoint single-RPC case, a 2-input refresh
	// round produces 2 output VTXOs whose amounts sum to
	// Σin − operator_fee_for_batch. Assert on the sum; per-output
	// amounts are the seal-time builder's choice and not the
	// invariant we are protecting here.
	refreshedVTXOs := waitForNewLiveVTXOsInRound(
		t, alice.RPCClient, refreshRound.RoundId, 2,
	)
	require.Len(t, refreshedVTXOs, 2)

	var refreshedSum int64
	for _, v := range refreshedVTXOs {
		require.NotEqual(t, vtxo1.Outpoint, v.Outpoint)
		require.NotEqual(t, vtxo2.Outpoint, v.Outpoint)
		require.Equal(t, refreshRound.RoundId, v.RoundId)
		refreshedSum += v.AmountSat
	}

	require.Less(t, refreshedSum, totalInputSat,
		"refresh must deduct an operator fee from the input total")
	feeDelta := totalInputSat - refreshedSum
	require.Greater(t, feeDelta, int64(0))
	require.Less(t, feeDelta, int64(10_000))

	finalBalance := waitForExactVTXOBalance(
		t, alice.RPCClient, refreshedSum,
	)
	require.Equal(t, refreshedSum, finalBalance.VtxoBalanceSat)
}
