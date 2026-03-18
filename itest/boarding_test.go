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
