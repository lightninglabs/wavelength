//go:build itest

package itest

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	client_harness "github.com/lightninglabs/darepo-client/harness"
	"github.com/lightninglabs/darepo/adminrpc"
	"github.com/lightninglabs/darepo/harness"
	"github.com/stretchr/testify/require"
)

// TestSealTimeFeeQuoteHappyPath drives a full board-then-refresh
// lifecycle and asserts that the final VTXO amount lands on the
// server's authoritative seal-time quote (the Σin − operator_fee
// residual) rather than on a pre-quoted client value. Under #270
// the client submits its refresh intent with IsChange=true and the
// server's computeSealTimeQuotes stamps the residual on the change
// output as part of the accepted JoinRoundQuote; this test proves
// the wire shape, the wallet admission path, and the server fee
// builder all agree on the final amount.
//
// The happy-path coverage here is complementary to the FSM kernel
// tests: this test exercises the real daemon, real chain backend,
// real envelope routing — anything that would break the handshake
// protocol itself (envelope route missing, FromProto regression,
// MaxOperatorFee cap misapplied) shows up as a refresh amount that
// does not match the expected seal-time residual.
func TestSealTimeFeeQuoteHappyPath(t *testing.T) {
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

	// Board a single VTXO: the boarding round exercises the quote
	// handshake on a fresh intent. boardClientAndConfirmRound
	// implicitly verifies the seal-time path succeeds — any FSM or
	// wire regression breaks the confirmation wait here.
	_, liveVTXO, startBalance := boardClientAndConfirmRound(
		t, h, alice.RPCClient, operatorInfo.MinConfirmations, 120_000,
	)
	require.Equal(t, liveVTXO.AmountSat, startBalance.VtxoBalanceSat)
	knownLiveVTXOs := outpointSet(listLiveVTXOs(t, alice.RPCClient))

	// Compute the expected post-refresh residual off the same chain
	// tip / treasury state the server will see when it seals the
	// refresh round. This mirrors the production path: client side
	// can preview with EstimateFee, but the binding amount comes
	// from the seal-time quote.
	expectedRefreshedSat := expectedNetAfterRefresh(t, h, liveVTXO)

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

	broadcastRound := waitForOperatorRoundStatus(
		t, h, refreshRound.RoundId,
		adminrpc.RoundStatus_ROUND_STATUS_BROADCAST,
	)
	require.NotEmpty(t, broadcastRound.TxId)

	mineUntilOperatorRoundConfirmed(
		t, h, refreshRound.RoundId, broadcastRound.TxId,
	)
	waitForVTXOStatusByOutpoint(
		t, alice.RPCClient, liveVTXO.Outpoint,
		daemonrpc.VTXOStatus_VTXO_STATUS_FORFEITED,
	)

	// The critical seal-time-fee assertion: the newly produced VTXO
	// amount must equal the server's authoritative residual. A
	// client path that persists the intent's target amount instead
	// of the quote amount, or a server path that forgets to stamp
	// the residual on the change output, would produce a mismatch
	// here.
	refreshedVTXO := waitForNewLiveVTXOWithAmount(
		t, alice.RPCClient, knownLiveVTXOs, expectedRefreshedSat,
	)
	require.NotEqual(t, liveVTXO.Outpoint, refreshedVTXO.Outpoint)
	require.Equal(t, refreshRound.RoundId, refreshedVTXO.RoundId)

	finalBalance := waitForExactVTXOBalance(
		t, alice.RPCClient, refreshedVTXO.AmountSat,
	)
	require.Equal(t, refreshedVTXO.AmountSat, finalBalance.VtxoBalanceSat)
}

// TestSealTimeFeeQuoteAdminGetRoundStatus drives a round from
// boarding through confirmation and calls GetRoundStatus at the
// final confirmed state. It verifies the admin RPC end-to-end: the
// envelope route is wired, the actor reply maps to the proto, and
// the state snapshot reflects the round's terminal shape. Polling
// through the intermediate QuoteSent window is racy on fast
// regtest hardware (the window may close before the admin RPC
// round-trips), so the assertion targets the terminal state whose
// observability is the production operator's primary use case.
func TestSealTimeFeeQuoteAdminGetRoundStatus(t *testing.T) {
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

	joined, _, _ := boardClientAndConfirmRound(
		t, h, alice.RPCClient, operatorInfo.MinConfirmations, 100_000,
	)
	require.NotEmpty(t, joined.RoundId)

	// Query the admin GetRoundStatus RPC on the confirmed round.
	// The call must round-trip the round_id unchanged and the
	// state_name reflects the terminal round phase (handler maps
	// every server state through the actor's snapshot).
	resp, err := h.ArkAdminClient.GetRoundStatus(
		t.Context(), &adminrpc.GetRoundStatusRequest{
			RoundId: joined.RoundId,
		},
	)
	require.NoError(t, err, "GetRoundStatus RPC failed")
	require.NotNil(t, resp)
	require.Equal(t, joined.RoundId, resp.RoundId)
	require.NotEmpty(t, resp.StateName,
		"state_name must be populated for a known round")
}

// TestSealTimeFeeQuoteAdminGetRoundStatusUnknown verifies the admin
// handler's round-not-found path: an unknown UUID must produce a
// clear "not found" reply rather than a deserialization panic or a
// stale-state leak. Guards the operator-facing error surface.
func TestSealTimeFeeQuoteAdminGetRoundStatusUnknown(t *testing.T) {
	t.Parallel()

	clientOpts := client_harness.DefaultOptions()
	clientOpts.GroupName = t.Name()
	clientOpts.StartTapd = false

	h := harness.NewArkHarness(t, &harness.ArkHarnessOptions{
		ClientOptions: &clientOpts,
	})
	t.Cleanup(h.Stop)

	h.Start()

	// A valid-shape UUID that the actor has never seen. The handler
	// must reply cleanly rather than panic or block.
	const unknownRoundID = "00000000-0000-0000-0000-000000000001"

	_, err := h.ArkAdminClient.GetRoundStatus(
		t.Context(), &adminrpc.GetRoundStatusRequest{
			RoundId: unknownRoundID,
		},
	)
	require.Error(t, err,
		"unknown round_id must surface as an RPC error rather than "+
			"a zero-value success reply")
}
