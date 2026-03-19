//go:build itest

package itest

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	client_harness "github.com/lightninglabs/darepo-client/harness"
	"github.com/lightninglabs/darepo/adminrpc"
	"github.com/lightninglabs/darepo/harness"
	"github.com/stretchr/testify/require"
)

// TestBoardingIntegrationSingleClient exercises real client daemon boarding
// registration against the in-process operator from boarding address creation
// all the way through confirmed round completion.
//
// The flow is driven through public RPCs:
//   - daemonrpc.NewAddress / Board / ListRounds / GetBalance
//   - arkrpc.GetInfo for operator terms
//   - adminrpc.ListRounds / ListClients
func TestBoardingIntegrationSingleClient(t *testing.T) {
	clientOpts := client_harness.DefaultOptions()
	clientOpts.GroupName = t.Name()
	clientOpts.StartTapd = false

	h := harness.NewArkHarness(t, &harness.ArkHarnessOptions{
		ClientOptions: &clientOpts,
	})
	t.Cleanup(h.Stop)

	h.Start()
	h.FundOperatorLND(btcutil.SatoshiPerBitcoin)

	alice := h.StartClientDaemon("alice")
	operatorInfo := getOperatorInfo(t, h)

	newAddrResp, err := alice.RPCClient.NewAddress(
		t.Context(), &daemonrpc.NewAddressRequest{},
	)
	require.NoError(t, err, "NewAddress RPC failed")
	require.NotEmpty(
		t, newAddrResp.Address, "boarding address should be set",
	)

	boardingAmount := btcutil.Amount(100_000)
	fundingTxID := h.Faucet(newAddrResp.Address, boardingAmount)
	t.Logf("Funded boarding address via txid=%s", fundingTxID)

	// Mine one extra block beyond the advertised minimum so both the
	// client wallet view and the operator's direct bitcoind validation
	// path observe the funding transaction before JoinRound runs.
	h.Generate(int(operatorInfo.MinConfirmations) + 1)

	balance := waitForConfirmedBoardingBalance(
		t, alice.RPCClient, int64(boardingAmount),
	)
	t.Logf("Client detected confirmed boarding balance=%d sats",
		balance.BoardingConfirmedSat)

	boardResp := waitForBoardRegistered(t, alice.RPCClient)
	require.Equal(t, "registered", boardResp.Status)

	waitForClientRegistration(t, h)
	t.Log("Operator registered the client daemon")

	clientJoinedRound := waitForClientRoundState(
		t, alice.RPCClient, daemonrpc.RoundState_ROUND_STATE_JOINED,
	)
	t.Logf("Client round joined: state=%s round_id=%q temp=%v",
		clientJoinedRound.State.String(), clientJoinedRound.RoundId,
		clientJoinedRound.IsTemp)
	require.NotEmpty(t, clientJoinedRound.RoundId,
		"joined client round should have a concrete round id")
	require.False(t, clientJoinedRound.IsTemp,
		"joined client round should no longer be temporary")

	waitForNamedClientRoundState(
		t, alice.RPCClient, clientJoinedRound.RoundId,
		daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT,
	)
	t.Logf("Client round reached input-sig-sent: round_id=%q",
		clientJoinedRound.RoundId)

	waitForPersistedClientRoundState(
		t, alice.RPCClient, clientJoinedRound.RoundId,
		daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT, 0,
	)
	t.Logf("Client persisted round checkpoint: round_id=%q",
		clientJoinedRound.RoundId)

	broadcastRound := waitForOperatorRoundStatus(
		t, h, clientJoinedRound.RoundId,
		adminrpc.RoundStatus_ROUND_STATUS_BROADCAST,
	)
	require.NotEmpty(t, broadcastRound.TxId)
	t.Logf("Round transaction broadcast: round_id=%q txid=%s",
		clientJoinedRound.RoundId, broadcastRound.TxId)

	mineUntilOperatorRoundConfirmed(
		t, h, clientJoinedRound.RoundId, broadcastRound.TxId,
	)
	t.Logf("Mined blocks until round confirmed: round_id=%q",
		clientJoinedRound.RoundId)

	confirmedRound := waitForNamedClientRoundState(
		t, alice.RPCClient, clientJoinedRound.RoundId,
		daemonrpc.RoundState_ROUND_STATE_CONFIRMED,
	)
	require.False(t, confirmedRound.IsTemp,
		"confirmed round should be persisted")

	waitForOperatorRoundStatus(
		t, h, clientJoinedRound.RoundId,
		adminrpc.RoundStatus_ROUND_STATUS_CONFIRMED,
	)
	t.Logf("Operator marked round confirmed: round_id=%q",
		clientJoinedRound.RoundId)

	liveVTXO := waitForLiveVTXO(
		t, alice.RPCClient, clientJoinedRound.RoundId,
	)
	require.Equal(t, int64(99_000), liveVTXO.AmountSat)

	finalBalance := waitForVTXOBalance(
		t, alice.RPCClient, liveVTXO.AmountSat,
	)
	require.Equal(t, liveVTXO.AmountSat, finalBalance.VtxoBalanceSat)
	t.Logf("Client received live VTXO amount=%d round_id=%q "+
		"(boarding_confirmed_sat=%d)", liveVTXO.AmountSat,
		liveVTXO.RoundId, finalBalance.BoardingConfirmedSat)
}

// TestBoardingIntegrationTwoClientsSharedRound exercises two real client
// daemons boarding into the same operator-managed round and verifies both
// sides observe shared-round formation through transaction broadcast.
func TestBoardingIntegrationTwoClientsSharedRound(t *testing.T) {
	clientOpts := client_harness.DefaultOptions()
	clientOpts.GroupName = t.Name()
	clientOpts.StartTapd = false

	h := harness.NewArkHarness(t, &harness.ArkHarnessOptions{
		ClientOptions: &clientOpts,
	})
	t.Cleanup(h.Stop)

	h.Start()
	h.FundOperatorLND(btcutil.SatoshiPerBitcoin)

	alice := h.StartClientDaemon("alice")
	bob := h.StartClientDaemon("bob")
	operatorInfo := getOperatorInfo(t, h)

	boardingAmount := btcutil.Amount(100_000)
	for _, tc := range []struct {
		name   string
		client daemonrpc.DaemonServiceClient
	}{
		{name: "alice", client: alice.RPCClient},
		{name: "bob", client: bob.RPCClient},
	} {
		newAddrResp, err := tc.client.NewAddress(
			t.Context(), &daemonrpc.NewAddressRequest{},
		)
		require.NoError(t, err, "%s NewAddress RPC failed", tc.name)
		require.NotEmpty(
			t, newAddrResp.Address,
			"%s boarding address should be set", tc.name,
		)

		fundingTxID := h.Faucet(newAddrResp.Address, boardingAmount)
		t.Logf("%s funded boarding address via txid=%s",
			tc.name, fundingTxID)
	}

	// Mine one extra block beyond the advertised minimum so both
	// clients and the operator's direct chain validation path
	// observe all boarding inputs before JoinRound runs.
	h.Generate(int(operatorInfo.MinConfirmations) + 1)

	aliceBalance := waitForConfirmedBoardingBalance(
		t, alice.RPCClient, int64(boardingAmount),
	)
	bobBalance := waitForConfirmedBoardingBalance(
		t, bob.RPCClient, int64(boardingAmount),
	)
	t.Logf("Confirmed boarding balances: alice=%d bob=%d",
		aliceBalance.BoardingConfirmedSat,
		bobBalance.BoardingConfirmedSat,
	)

	aliceBoardResp := waitForBoardRegistered(t, alice.RPCClient)
	bobBoardResp := waitForBoardRegistered(t, bob.RPCClient)
	require.Equal(t, "registered", aliceBoardResp.Status)
	require.Equal(t, "registered", bobBoardResp.Status)

	clientResp := waitForRegisteredClients(t, h, 2)
	require.Len(t, clientResp.Clients, 2)
	t.Log("Operator registered both real client daemons")

	aliceJoinedRound := waitForClientRoundState(
		t, alice.RPCClient, daemonrpc.RoundState_ROUND_STATE_JOINED,
	)
	bobJoinedRound := waitForClientRoundState(
		t, bob.RPCClient, daemonrpc.RoundState_ROUND_STATE_JOINED,
	)

	require.NotEmpty(t, aliceJoinedRound.RoundId,
		"alice joined round should have a concrete round id")
	require.Equal(t, aliceJoinedRound.RoundId, bobJoinedRound.RoundId,
		"alice and bob should join the same round")
	require.False(t, aliceJoinedRound.IsTemp,
		"alice joined round should no longer be temporary")
	require.False(t, bobJoinedRound.IsTemp,
		"bob joined round should no longer be temporary")
	t.Logf("Both clients joined round_id=%q", aliceJoinedRound.RoundId)

	waitForNamedClientRoundState(
		t, alice.RPCClient, aliceJoinedRound.RoundId,
		daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT,
	)
	waitForNamedClientRoundState(
		t, bob.RPCClient, bobJoinedRound.RoundId,
		daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT,
	)
	t.Logf("Both clients reached input-sig-sent for round_id=%q",
		aliceJoinedRound.RoundId)

	waitForPersistedClientRoundState(
		t, alice.RPCClient, aliceJoinedRound.RoundId,
		daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT, 0,
	)
	waitForPersistedClientRoundState(
		t, bob.RPCClient, bobJoinedRound.RoundId,
		daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT, 0,
	)
	t.Logf("Both clients persisted round checkpoint: round_id=%q",
		aliceJoinedRound.RoundId)

	broadcastRound := waitForOperatorRoundStatus(
		t, h, aliceJoinedRound.RoundId,
		adminrpc.RoundStatus_ROUND_STATUS_BROADCAST,
	)
	require.NotEmpty(t, broadcastRound.TxId)
	t.Logf("Shared round transaction broadcast: round_id=%q txid=%s",
		aliceJoinedRound.RoundId, broadcastRound.TxId)

	h.WaitMempoolTx(broadcastRound.TxId)
	t.Logf("Shared round transaction reached mempool: txid=%s",
		broadcastRound.TxId)
}

// TestBoardingIntegrationThreeClientsSharedRound verifies three real client
// daemons can join the same round, sign successfully, and observe round
// transaction broadcast via public RPC surfaces.
func TestBoardingIntegrationThreeClientsSharedRound(t *testing.T) {
	clientOpts := client_harness.DefaultOptions()
	clientOpts.GroupName = t.Name()
	clientOpts.StartTapd = false

	h := harness.NewArkHarness(t, &harness.ArkHarnessOptions{
		ClientOptions: &clientOpts,
	})
	t.Cleanup(h.Stop)

	h.Start()
	h.FundOperatorLND(btcutil.SatoshiPerBitcoin * 2)

	operatorInfo := getOperatorInfo(t, h)

	type testClient struct {
		name   string
		client daemonrpc.DaemonServiceClient
	}

	clients := []testClient{
		{name: "alice", client: h.StartClientDaemon("alice").RPCClient},
		{name: "bob", client: h.StartClientDaemon("bob").RPCClient},
		{name: "carol", client: h.StartClientDaemon("carol").RPCClient},
	}

	boardingAmount := btcutil.Amount(100_000)
	for _, tc := range clients {
		newAddrResp, err := tc.client.NewAddress(
			t.Context(), &daemonrpc.NewAddressRequest{},
		)
		require.NoError(t, err, "%s NewAddress RPC failed", tc.name)
		require.NotEmpty(
			t, newAddrResp.Address,
			"%s boarding address should be set", tc.name,
		)

		fundingTxID := h.Faucet(newAddrResp.Address, boardingAmount)
		t.Logf("%s funded boarding address via txid=%s",
			tc.name, fundingTxID)
	}

	// Mine one extra block beyond the advertised minimum so all clients and
	// the operator observe every boarding input before JoinRound runs.
	h.Generate(int(operatorInfo.MinConfirmations) + 1)

	for _, tc := range clients {
		waitForConfirmedBoardingBalance(
			t, tc.client, int64(boardingAmount),
		)
		resp := waitForBoardRegistered(t, tc.client)
		require.Equal(t, "registered", resp.Status)
	}

	clientResp := waitForRegisteredClients(t, h, len(clients))
	require.Len(t, clientResp.Clients, len(clients))
	t.Log("Operator registered all three real client daemons")

	sharedRoundID := ""
	for _, tc := range clients {
		joined := waitForClientRoundState(
			t, tc.client, daemonrpc.RoundState_ROUND_STATE_JOINED,
		)
		require.NotEmpty(t, joined.RoundId,
			"%s joined round should have a concrete round id",
			tc.name,
		)
		require.False(t, joined.IsTemp,
			"%s joined round should no longer be temporary",
			tc.name,
		)

		if sharedRoundID == "" {
			sharedRoundID = joined.RoundId
		} else {
			require.Equal(t, sharedRoundID, joined.RoundId,
				"all clients should join the same round")
		}
	}
	t.Logf("All clients joined shared round_id=%q", sharedRoundID)

	for _, tc := range clients {
		waitForNamedClientRoundState(
			t, tc.client, sharedRoundID,
			daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT,
		)
		waitForPersistedClientRoundState(
			t, tc.client, sharedRoundID,
			daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT, 0,
		)
	}

	broadcastRound := waitForOperatorRoundStatus(
		t, h, sharedRoundID,
		adminrpc.RoundStatus_ROUND_STATUS_BROADCAST,
	)
	require.NotEmpty(t, broadcastRound.TxId)
	t.Logf("Shared round transaction broadcast: round_id=%q txid=%s",
		sharedRoundID, broadcastRound.TxId)

	h.WaitMempoolTx(broadcastRound.TxId)
	t.Logf("Shared round transaction reached mempool: txid=%s",
		broadcastRound.TxId)
}

// TestBoardingIntegrationSingleClientSubsequentRounds verifies a single real
// client daemon can board in multiple rounds back-to-back and persist both
// outputs as independent live VTXOs.
func TestBoardingIntegrationSingleClientSubsequentRounds(t *testing.T) {
	clientOpts := client_harness.DefaultOptions()
	clientOpts.GroupName = t.Name()
	clientOpts.StartTapd = false

	h := harness.NewArkHarness(t, &harness.ArkHarnessOptions{
		ClientOptions: &clientOpts,
	})
	t.Cleanup(h.Stop)

	h.Start()
	h.FundOperatorLND(btcutil.SatoshiPerBitcoin)

	alice := h.StartClientDaemon("alice")
	operatorInfo := getOperatorInfo(t, h)

	waitForClientRegistration(t, h)
	t.Log("Operator registered the client daemon")

	round1, round1VTXO, _ := boardClientAndConfirmRound(
		t, h, alice.RPCClient, operatorInfo.MinConfirmations, 100_000,
	)
	t.Logf("Round 1 complete: round_id=%q outpoint=%s amount=%d",
		round1.RoundId, round1VTXO.Outpoint, round1VTXO.AmountSat)

	round2, round2VTXO, round2Balance := boardClientAndConfirmRound(
		t, h, alice.RPCClient, operatorInfo.MinConfirmations, 120_000,
	)
	t.Logf("Round 2 complete: round_id=%q outpoint=%s amount=%d",
		round2.RoundId, round2VTXO.Outpoint, round2VTXO.AmountSat)

	require.NotEqual(t, round1.RoundId, round2.RoundId,
		"subsequent rounds must have distinct round IDs")
	require.NotEqual(t, round1VTXO.Outpoint, round2VTXO.Outpoint,
		"subsequent rounds must create distinct live VTXOs")

	expectedTotal := round1VTXO.AmountSat + round2VTXO.AmountSat
	exactBalance := waitForExactVTXOBalance(
		t, alice.RPCClient, expectedTotal,
	)
	require.Equal(t, expectedTotal, exactBalance.VtxoBalanceSat)
	require.Equal(t, expectedTotal, round2Balance.VtxoBalanceSat)

	ctx, cancel := context.WithTimeout(t.Context(), defaultSmallTimeout)
	defer cancel()

	liveResp, err := alice.RPCClient.ListVTXOs(
		ctx, &daemonrpc.ListVTXOsRequest{
			StatusFilter: daemonrpc.VTXOStatus_VTXO_STATUS_LIVE,
		},
	)
	require.NoError(t, err, "ListVTXOs RPC failed")

	hasRound1 := false
	hasRound2 := false
	for _, vtxo := range liveResp.Vtxos {
		switch vtxo.RoundId {
		case round1.RoundId:
			hasRound1 = true

		case round2.RoundId:
			hasRound2 = true
		}
	}

	require.True(t, hasRound1, "missing live VTXO from round 1")
	require.True(t, hasRound2, "missing live VTXO from round 2")
	t.Logf(
		"Client retained live VTXOs from both rounds "+
			"(total_vtxo_sat=%d)",
		exactBalance.VtxoBalanceSat)
}

// TestBoardingIntegrationRestartAfterRoundBroadcast verifies a real client
// daemon can restart after the shared commitment transaction is broadcast but
// before confirmation, then resume from persisted state and complete the round.
func TestBoardingIntegrationRestartAfterRoundBroadcast(t *testing.T) {
	clientOpts := client_harness.DefaultOptions()
	clientOpts.GroupName = t.Name()
	clientOpts.StartTapd = false

	h := harness.NewArkHarness(t, &harness.ArkHarnessOptions{
		ClientOptions: &clientOpts,
	})
	t.Cleanup(h.Stop)

	h.Start()
	h.FundOperatorLND(btcutil.SatoshiPerBitcoin)

	alice := h.StartClientDaemon("alice")
	operatorInfo := getOperatorInfo(t, h)

	newAddrResp, err := alice.RPCClient.NewAddress(
		t.Context(), &daemonrpc.NewAddressRequest{},
	)
	require.NoError(t, err, "NewAddress RPC failed")
	require.NotEmpty(
		t, newAddrResp.Address, "boarding address should be set",
	)

	boardingAmount := btcutil.Amount(100_000)
	fundingTxID := h.Faucet(newAddrResp.Address, boardingAmount)
	t.Logf("Funded boarding address via txid=%s", fundingTxID)

	// Mine one extra block beyond the advertised minimum so both the
	// client wallet view and the operator's direct bitcoind validation
	// path observe the funding transaction before JoinRound runs.
	h.Generate(int(operatorInfo.MinConfirmations) + 1)

	waitForConfirmedBoardingBalance(
		t, alice.RPCClient, int64(boardingAmount),
	)
	boardResp := waitForBoardRegistered(t, alice.RPCClient)
	require.Equal(t, "registered", boardResp.Status)

	waitForClientRegistration(t, h)
	joinedRound := waitForClientRoundState(
		t, alice.RPCClient, daemonrpc.RoundState_ROUND_STATE_JOINED,
	)
	require.NotEmpty(t, joinedRound.RoundId)
	require.False(t, joinedRound.IsTemp)

	waitForNamedClientRoundState(
		t, alice.RPCClient, joinedRound.RoundId,
		daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT,
	)
	waitForPersistedClientRoundState(
		t, alice.RPCClient, joinedRound.RoundId,
		daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT, 0,
	)

	broadcastRound := waitForOperatorRoundStatus(
		t, h, joinedRound.RoundId,
		adminrpc.RoundStatus_ROUND_STATUS_BROADCAST,
	)
	require.NotEmpty(t, broadcastRound.TxId)
	t.Logf(
		"Round transaction broadcast before restart: "+
			"round_id=%q txid=%s",
		joinedRound.RoundId, broadcastRound.TxId,
	)

	oldRPCAddr := alice.RPCAddr
	alice = h.RestartClientDaemon("alice")
	t.Logf("Restarted client daemon: old_rpc=%s new_rpc=%s",
		oldRPCAddr, alice.RPCAddr)

	mineUntilOperatorRoundConfirmed(
		t, h, joinedRound.RoundId, broadcastRound.TxId,
	)
	t.Logf("Mined blocks until round confirmed after restart: round_id=%q",
		joinedRound.RoundId)

	confirmedRound := waitForNamedClientRoundState(
		t, alice.RPCClient, joinedRound.RoundId,
		daemonrpc.RoundState_ROUND_STATE_CONFIRMED,
	)
	require.False(t, confirmedRound.IsTemp,
		"confirmed round should be persisted after restart")

	waitForOperatorRoundStatus(
		t, h, joinedRound.RoundId,
		adminrpc.RoundStatus_ROUND_STATUS_CONFIRMED,
	)
	t.Logf("Operator marked round confirmed after restart: round_id=%q",
		joinedRound.RoundId)

	liveVTXO := waitForLiveVTXO(t, alice.RPCClient, joinedRound.RoundId)
	require.Equal(t, int64(99_000), liveVTXO.AmountSat)

	finalBalance := waitForVTXOBalance(
		t, alice.RPCClient, liveVTXO.AmountSat,
	)
	require.Equal(t, liveVTXO.AmountSat, finalBalance.VtxoBalanceSat)
	t.Logf(
		"Client recovered and completed round after restart: "+
			"round_id=%q",
		joinedRound.RoundId,
	)
}
