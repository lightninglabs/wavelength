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
