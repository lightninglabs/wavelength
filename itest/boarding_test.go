//go:build itest

package itest

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/darepo"
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
	t.Parallel()

	runSingleClientBoardingRound(t, "")
}

// TestRESTGatewayMailboxBoardingRound keeps grpc-gateway coverage on one
// focused long path. The harness talks to client-facing RPCs over REST while
// admin control stays on gRPC, and the client daemon completes the boarding
// round through its mailbox session with arkd.
func TestRESTGatewayMailboxBoardingRound(t *testing.T) {
	t.Parallel()

	runSingleClientBoardingRound(t, harness.RPCTransportREST)
}

func runSingleClientBoardingRound(t *testing.T, rpcTransport string) {
	t.Helper()

	clientOpts := client_harness.DefaultOptions()
	clientOpts.GroupName = t.Name()
	clientOpts.StartTapd = false

	h := harness.NewArkHarness(t, &harness.ArkHarnessOptions{
		ClientOptions: &clientOpts,
		RPCTransport:  rpcTransport,
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
	t.Logf(
		"Client detected confirmed boarding balance=%d sats",
		balance.BoardingConfirmedSat,
	)

	boardResp := waitForBoardRegistered(t, alice.RPCClient)
	require.Equal(t, "registered", boardResp.Status)

	waitForClientRegistration(t, h)
	t.Log("Operator registered the client daemon")

	clientJoinedRound := waitForClientRoundState(
		t, alice.RPCClient, daemonrpc.RoundState_ROUND_STATE_JOINED,
	)
	t.Logf(
		"Client round joined: state=%s round_id=%q temp=%v",
		clientJoinedRound.State.String(), clientJoinedRound.RoundId,
		clientJoinedRound.IsTemp,
	)
	require.NotEmpty(
		t, clientJoinedRound.RoundId,
		"joined client round should have a concrete round id",
	)
	require.False(
		t, clientJoinedRound.IsTemp,
		"joined client round should no longer be temporary",
	)

	waitForNamedClientRoundState(
		t, alice.RPCClient, clientJoinedRound.RoundId,
		daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT,
	)
	t.Logf(
		"Client round reached input-sig-sent: round_id=%q",
		clientJoinedRound.RoundId,
	)

	waitForPersistedClientRoundState(
		t, alice.RPCClient, clientJoinedRound.RoundId,
		daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT, 0,
	)
	t.Logf(
		"Client persisted round checkpoint: round_id=%q",
		clientJoinedRound.RoundId,
	)

	broadcastRound := waitForOperatorRoundStatus(
		t, h, clientJoinedRound.RoundId,
		adminrpc.RoundStatus_ROUND_STATUS_BROADCAST,
	)
	require.NotEmpty(t, broadcastRound.TxId)
	t.Logf(
		"Round transaction broadcast: round_id=%q txid=%s",
		clientJoinedRound.RoundId, broadcastRound.TxId,
	)

	mineUntilOperatorRoundConfirmed(
		t, h, clientJoinedRound.RoundId, broadcastRound.TxId,
	)
	t.Logf(
		"Mined blocks until round confirmed: round_id=%q",
		clientJoinedRound.RoundId,
	)

	confirmedRound := waitForNamedClientRoundState(
		t, alice.RPCClient, clientJoinedRound.RoundId,
		daemonrpc.RoundState_ROUND_STATE_CONFIRMED,
	)
	require.False(
		t, confirmedRound.IsTemp, "confirmed round should be persisted",
	)

	waitForOperatorRoundStatus(
		t, h, clientJoinedRound.RoundId,
		adminrpc.RoundStatus_ROUND_STATUS_CONFIRMED,
	)
	t.Logf(
		"Operator marked round confirmed: round_id=%q",
		clientJoinedRound.RoundId,
	)

	liveVTXO := waitForLiveVTXO(
		t, alice.RPCClient, clientJoinedRound.RoundId,
	)
	// Post-#263 the harness default runs under a non-zero fee
	// schedule. The resulting VTXO value is
	// boardingAmount - boarding_fee where the boarding fee is
	// computed by the operator's fees.Calculator against the
	// default tree size. Assert the expected net directly so a
	// schedule tweak in harness/fees.go is caught here.
	expectedNet := expectedNetAfterBoarding(
		t, int64(boardingAmount), defaultItestBatchSize,
	)
	require.Equal(t, expectedNet, liveVTXO.AmountSat)

	finalBalance := waitForVTXOBalance(
		t, alice.RPCClient, liveVTXO.AmountSat,
	)
	require.Equal(t, liveVTXO.AmountSat, finalBalance.VtxoBalanceSat)
	t.Logf(
		"Client received live VTXO amount=%d round_id=%q "+
			"(boarding_confirmed_sat=%d)", liveVTXO.AmountSat,
		liveVTXO.RoundId, finalBalance.BoardingConfirmedSat,
	)

	// While we already have a confirmed round in hand, drive the
	// admin GetRoundStatus RPC end-to-end on the same round_id.
	// This covers the route wiring, actor reply mapping, and proto
	// translation in one call without spinning up a second
	// harness for a dedicated admin-only test.
	statusResp, err := h.ArkAdminClient.GetRoundStatus(
		t.Context(), &adminrpc.GetRoundStatusRequest{
			RoundId: clientJoinedRound.RoundId,
		},
	)
	require.NoError(t, err, "GetRoundStatus RPC failed")
	require.NotNil(t, statusResp)
	require.Equal(t, clientJoinedRound.RoundId, statusResp.RoundId)
	require.NotEmpty(
		t, statusResp.StateName,
		"state_name must be populated for a known round",
	)
}

// TestBoardingIntegrationNoConfirmedInputs verifies Board returns
// no_boarding_utxos when the client has not funded any boarding address.
func TestBoardingIntegrationNoConfirmedInputs(t *testing.T) {
	t.Parallel()

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
	waitForClientRegistration(t, h)

	ctx, cancel := context.WithTimeout(t.Context(), defaultSmallTimeout)
	defer cancel()

	balanceResp, err := alice.RPCClient.GetBalance(
		ctx, &daemonrpc.GetBalanceRequest{},
	)
	require.NoError(t, err, "GetBalance RPC failed")
	require.Zero(t, balanceResp.BoardingConfirmedSat)

	boardResp, err := alice.RPCClient.Board(
		ctx, &daemonrpc.BoardRequest{},
	)
	require.NoError(t, err, "Board RPC failed")
	require.Equal(t, "no_boarding_utxos", boardResp.Status)

	roundsResp, err := alice.RPCClient.ListRounds(
		ctx, &daemonrpc.ListRoundsRequest{},
	)
	require.NoError(t, err, "ListRounds RPC failed")
	require.Empty(t, roundsResp.Rounds, "no round should be started")
}

// TestBoardingIntegrationTwoClientsSharedRound exercises two real client
// daemons boarding into the same operator-managed round and verifies both
// sides observe shared-round formation through transaction broadcast.
func TestBoardingIntegrationTwoClientsSharedRound(t *testing.T) {
	t.Parallel()

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
		{
			name:   "alice",
			client: alice.RPCClient,
		},
		{
			name:   "bob",
			client: bob.RPCClient,
		},
	} {
		newAddrResp, err := tc.client.NewAddress(
			t.Context(), &daemonrpc.NewAddressRequest{},
		)
		require.NoError(t, err, "%s NewAddress RPC failed", tc.name)
		require.NotEmpty(
			t, newAddrResp.Address, "%s boarding address should "+
				"be set", tc.name,
		)

		fundingTxID := h.Faucet(newAddrResp.Address, boardingAmount)
		t.Logf(
			"%s funded boarding address via txid=%s", tc.name,
			fundingTxID,
		)
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
	t.Logf(
		"Confirmed boarding balances: alice=%d bob=%d",
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

	require.NotEmpty(
		t, aliceJoinedRound.RoundId,
		"alice joined round should have a concrete round id",
	)
	require.Equal(
		t, aliceJoinedRound.RoundId, bobJoinedRound.RoundId,
		"alice and bob should join the same round",
	)
	require.False(
		t, aliceJoinedRound.IsTemp,
		"alice joined round should no longer be temporary",
	)
	require.False(
		t, bobJoinedRound.IsTemp,
		"bob joined round should no longer be temporary",
	)
	t.Logf("Both clients joined round_id=%q", aliceJoinedRound.RoundId)

	waitForNamedClientRoundState(
		t, alice.RPCClient, aliceJoinedRound.RoundId,
		daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT,
	)
	waitForNamedClientRoundState(
		t, bob.RPCClient, bobJoinedRound.RoundId,
		daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT,
	)
	t.Logf(
		"Both clients reached input-sig-sent for round_id=%q",
		aliceJoinedRound.RoundId,
	)

	waitForPersistedClientRoundState(
		t, alice.RPCClient, aliceJoinedRound.RoundId,
		daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT, 0,
	)
	waitForPersistedClientRoundState(
		t, bob.RPCClient, bobJoinedRound.RoundId,
		daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT, 0,
	)
	t.Logf(
		"Both clients persisted round checkpoint: round_id=%q",
		aliceJoinedRound.RoundId,
	)

	broadcastRound := waitForOperatorRoundStatus(
		t, h, aliceJoinedRound.RoundId,
		adminrpc.RoundStatus_ROUND_STATUS_BROADCAST,
	)
	require.NotEmpty(t, broadcastRound.TxId)
	t.Logf(
		"Shared round transaction broadcast: round_id=%q txid=%s",
		aliceJoinedRound.RoundId, broadcastRound.TxId,
	)

	h.WaitMempoolTx(broadcastRound.TxId)
	t.Logf(
		"Shared round transaction reached mempool: txid=%s",
		broadcastRound.TxId,
	)
}

// TestBoardingIntegrationThreeClientsSharedRound verifies three real client
// daemons can join the same round, sign successfully, and observe round
// transaction broadcast via public RPC surfaces.
func TestBoardingIntegrationThreeClientsSharedRound(t *testing.T) {
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

	operatorInfo := getOperatorInfo(t, h)

	type testClient struct {
		name   string
		client daemonrpc.DaemonServiceClient
	}

	clients := []testClient{
		{
			name:   "alice",
			client: h.StartClientDaemon("alice").RPCClient,
		},
		{
			name:   "bob",
			client: h.StartClientDaemon("bob").RPCClient,
		},
		{
			name:   "carol",
			client: h.StartClientDaemon("carol").RPCClient,
		},
	}

	boardingAmount := btcutil.Amount(100_000)
	for _, tc := range clients {
		newAddrResp, err := tc.client.NewAddress(
			t.Context(), &daemonrpc.NewAddressRequest{},
		)
		require.NoError(t, err, "%s NewAddress RPC failed", tc.name)
		require.NotEmpty(
			t, newAddrResp.Address, "%s boarding address should "+
				"be set", tc.name,
		)

		fundingTxID := h.Faucet(newAddrResp.Address, boardingAmount)
		t.Logf(
			"%s funded boarding address via txid=%s", tc.name,
			fundingTxID,
		)
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
		require.NotEmpty(
			t, joined.RoundId, "%s joined round should have a "+
				"concrete round id", tc.name,
		)
		require.False(
			t, joined.IsTemp, "%s joined round should no longer "+
				"be temporary", tc.name,
		)

		if sharedRoundID == "" {
			sharedRoundID = joined.RoundId
		} else {
			require.Equal(
				t, sharedRoundID, joined.RoundId,
				"all clients should join the same round",
			)
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
	t.Logf(
		"Shared round transaction broadcast: round_id=%q txid=%s",
		sharedRoundID, broadcastRound.TxId,
	)

	h.WaitMempoolTx(broadcastRound.TxId)
	t.Logf(
		"Shared round transaction reached mempool: txid=%s",
		broadcastRound.TxId,
	)
}

// TestBoardingIntegrationSingleClientSubsequentRounds verifies a single real
// client daemon can board in multiple rounds back-to-back and persist both
// outputs as independent live VTXOs.
func TestBoardingIntegrationSingleClientSubsequentRounds(t *testing.T) {
	t.Parallel()

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
	t.Logf(
		"Round 1 complete: round_id=%q outpoint=%s amount=%d",
		round1.RoundId, round1VTXO.Outpoint, round1VTXO.AmountSat,
	)

	round2, round2VTXO, round2Balance := boardClientAndConfirmRound(
		t, h, alice.RPCClient, operatorInfo.MinConfirmations, 120_000,
	)
	t.Logf(
		"Round 2 complete: round_id=%q outpoint=%s amount=%d",
		round2.RoundId, round2VTXO.Outpoint, round2VTXO.AmountSat,
	)

	require.NotEqual(
		t, round1.RoundId, round2.RoundId,
		"subsequent rounds must have distinct round IDs",
	)
	require.NotEqual(
		t, round1VTXO.Outpoint, round2VTXO.Outpoint,
		"subsequent rounds must create distinct live VTXOs",
	)

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
			"(total_vtxo_sat=%d)", exactBalance.VtxoBalanceSat,
	)
}

// TestBoardingIntegrationRestartAfterRoundBroadcast verifies a real client
// daemon can restart after the shared commitment transaction is broadcast but
// before confirmation, then resume from persisted state and complete the round.
func TestBoardingIntegrationRestartAfterRoundBroadcast(t *testing.T) {
	t.Parallel()

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
		"Round transaction broadcast before restart: round_id=%q "+
			"txid=%s", joinedRound.RoundId, broadcastRound.TxId,
	)

	oldRPCAddr := alice.RPCAddr
	alice = h.RestartClientDaemon("alice")
	t.Logf(
		"Restarted client daemon: old_rpc=%s new_rpc=%s", oldRPCAddr,
		alice.RPCAddr,
	)

	mineUntilOperatorRoundConfirmed(
		t, h, joinedRound.RoundId, broadcastRound.TxId,
	)
	t.Logf(
		"Mined blocks until round confirmed after restart: round_id=%q",
		joinedRound.RoundId,
	)

	confirmedRound := waitForNamedClientRoundState(
		t, alice.RPCClient, joinedRound.RoundId,
		daemonrpc.RoundState_ROUND_STATE_CONFIRMED,
	)
	require.False(
		t, confirmedRound.IsTemp,
		"confirmed round should be persisted after restart",
	)

	waitForOperatorRoundStatus(
		t, h, joinedRound.RoundId,
		adminrpc.RoundStatus_ROUND_STATUS_CONFIRMED,
	)
	t.Logf(
		"Operator marked round confirmed after restart: round_id=%q",
		joinedRound.RoundId,
	)

	liveVTXO := waitForLiveVTXO(t, alice.RPCClient, joinedRound.RoundId)
	// See TestBoardingIntegrationSingleClient comment above:
	// the harness default runs fees-on; compute expected net
	// via the fee-aware helper so the assertion tracks the
	// schedule in harness/fees.go.
	require.Equal(
		t,
		expectedNetAfterBoarding(
			t, int64(boardingAmount), defaultItestBatchSize,
		),
		liveVTXO.AmountSat,
	)

	finalBalance := waitForVTXOBalance(
		t, alice.RPCClient, liveVTXO.AmountSat,
	)
	require.Equal(t, liveVTXO.AmountSat, finalBalance.VtxoBalanceSat)
	t.Logf(
		"Client recovered and completed round after restart: "+
			"round_id=%q", joinedRound.RoundId,
	)
}

// TestBoardingIntegrationRestartAfterInputSigSent verifies a client daemon can
// restart after the round reaches the documented InputSigSent checkpoint and
// still progress to confirmed completion on the same round.
func TestBoardingIntegrationRestartAfterInputSigSent(t *testing.T) {
	t.Parallel()

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
	checkpointedRound := waitForClientRoundState(
		t, alice.RPCClient,
		daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT,
	)
	require.NotEmpty(t, checkpointedRound.RoundId)
	require.False(t, checkpointedRound.IsTemp)

	waitForPersistedClientRoundState(
		t, alice.RPCClient, checkpointedRound.RoundId,
		daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT, 0,
	)

	oldRPCAddr := alice.RPCAddr
	alice = h.RestartClientDaemon("alice")
	t.Logf(
		"Restarted client daemon after round checkpoint: old_rpc=%s "+
			"new_rpc=%s round_id=%s", oldRPCAddr, alice.RPCAddr,
		checkpointedRound.RoundId,
	)

	resumedRound := waitForNamedClientRoundState(
		t, alice.RPCClient, checkpointedRound.RoundId,
		daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT,
	)
	require.False(t, resumedRound.IsTemp)
	waitForPersistedClientRoundState(
		t, alice.RPCClient, checkpointedRound.RoundId,
		daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT, 0,
	)

	broadcastRound := waitForOperatorRoundStatus(
		t, h, checkpointedRound.RoundId,
		adminrpc.RoundStatus_ROUND_STATUS_BROADCAST,
	)
	require.NotEmpty(t, broadcastRound.TxId)

	mineUntilOperatorRoundConfirmed(
		t, h, checkpointedRound.RoundId, broadcastRound.TxId,
	)

	confirmedRound := waitForNamedClientRoundState(
		t, alice.RPCClient, checkpointedRound.RoundId,
		daemonrpc.RoundState_ROUND_STATE_CONFIRMED,
	)
	require.False(t, confirmedRound.IsTemp)

	waitForOperatorRoundStatus(
		t, h, checkpointedRound.RoundId,
		adminrpc.RoundStatus_ROUND_STATUS_CONFIRMED,
	)

	liveVTXO := waitForLiveVTXO(
		t, alice.RPCClient, checkpointedRound.RoundId,
	)
	require.Equal(
		t,
		expectedNetAfterBoarding(
			t, int64(boardingAmount), defaultItestBatchSize,
		),
		liveVTXO.AmountSat,
	)

	finalBalance := waitForVTXOBalance(
		t, alice.RPCClient, liveVTXO.AmountSat,
	)
	require.Equal(t, liveVTXO.AmountSat, finalBalance.VtxoBalanceSat)
}

// TestBoardingIntegrationTriggerBatchCreatesNewRound verifies that after
// a round is sealed via the admin TriggerBatch RPC (SealEvent path),
// the operator creates a new round. Without the fix that emits
// RoundSealedReq from the SealEvent handler, the actor never spawns
// a replacement round, so later clients cannot register.
func TestBoardingIntegrationTriggerBatchCreatesNewRound(t *testing.T) {
	t.Parallel()

	clientOpts := client_harness.DefaultOptions()
	clientOpts.GroupName = t.Name()
	clientOpts.StartTapd = false

	h := harness.NewArkHarness(t, &harness.ArkHarnessOptions{
		ClientOptions: &clientOpts,
		OperatorConfigMutator: func(cfg *darepo.Config) {
			// This test deliberately observes the
			// REGISTRATION_SENT round state on the
			// client before issuing TriggerBatch, so it
			// needs the operator's registration window
			// to stay open long enough for both the
			// poll and the admin RPC to land. The
			// itest harness default of 500ms is far
			// too short to catch the transient state;
			// pin to 60s to match the production
			// default (10s) with a 6x cushion for
			// busy CI runners.
			cfg.Rounds.RegistrationTimeout = 60 * time.Second
		},
	})
	t.Cleanup(h.Stop)

	h.Start()
	h.FundOperatorLND(btcutil.SatoshiPerBitcoin)

	alice := h.StartClientDaemon("alice")
	operatorInfo := getOperatorInfo(t, h)

	waitForClientRegistration(t, h)

	// Fund and board alice into a round.
	newAddrResp, err := alice.RPCClient.NewAddress(
		t.Context(), &daemonrpc.NewAddressRequest{},
	)
	require.NoError(t, err)

	boardingAmount := btcutil.Amount(100_000)
	h.Faucet(newAddrResp.Address, boardingAmount)
	h.Generate(int(operatorInfo.MinConfirmations) + 1)

	waitForConfirmedBoardingBalance(
		t, alice.RPCClient, int64(boardingAmount),
	)
	boardResp := waitForBoardRegistered(t, alice.RPCClient)
	require.Equal(t, "registered", boardResp.Status)

	registrationSent := daemonrpc.RoundState_ROUND_STATE_REGISTRATION_SENT

	var registeredRoundID string
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(
			t.Context(), defaultSmallTimeout,
		)
		defer cancel()

		resp, err := alice.RPCClient.ListRounds(
			ctx, &daemonrpc.ListRoundsRequest{},
		)
		if err != nil {
			return false
		}

		for _, round := range resp.Rounds {
			if round.IsTemp || round.RoundId == "" {
				continue
			}

			if round.State != registrationSent {
				continue
			}

			registeredRoundID = round.RoundId

			return true
		}

		return false
	}, defaultTimeout, pollInterval,
		"client never reached registration-sent round state")

	t.Logf("Client registered round_id=%q", registeredRoundID)

	// Seal via explicit TriggerBatch (SealEvent path).
	triggerCtx, triggerCancel := context.WithTimeout(
		t.Context(), defaultSmallTimeout,
	)
	defer triggerCancel()

	triggerResp, err := h.ArkAdminClient.TriggerBatch(
		triggerCtx, &adminrpc.TriggerBatchRequest{},
	)
	require.NoError(t, err, "TriggerBatch RPC failed")
	t.Logf("TriggerBatch sealed: returned round_id=%q",
		triggerResp.RoundId)
	require.Equal(t, registeredRoundID, triggerResp.RoundId)

	// The sealed round should progress through signing.
	waitForNamedClientRoundState(
		t, alice.RPCClient, registeredRoundID,
		daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT,
	)

	broadcastRound := waitForOperatorRoundStatus(
		t, h, registeredRoundID,
		adminrpc.RoundStatus_ROUND_STATUS_BROADCAST,
	)
	require.NotEmpty(t, broadcastRound.TxId)
	t.Logf(
		"Round broadcast: round_id=%q txid=%s", registeredRoundID,
		broadcastRound.TxId,
	)

	// The key assertion: after TriggerBatch seals alice's round and
	// the outbox processes RoundSealedReq, a later client should be
	// able to register into a replacement round instead of the sealed
	// one. This observes the externally visible behavior rather than
	// relying on whether an empty CreatedState round is shown by the
	// admin ListRounds RPC.
	bob := h.StartClientDaemon("bob")

	bobAddrResp, err := bob.RPCClient.NewAddress(
		t.Context(), &daemonrpc.NewAddressRequest{},
	)
	require.NoError(t, err, "bob NewAddress RPC failed")
	require.NotEmpty(
		t, bobAddrResp.Address, "bob boarding address should be set",
	)

	fundingTxID := h.Faucet(bobAddrResp.Address, boardingAmount)
	t.Logf("bob funded boarding address via txid=%s", fundingTxID)

	h.Generate(int(operatorInfo.MinConfirmations) + 1)
	waitForConfirmedBoardingBalance(
		t, bob.RPCClient, int64(boardingAmount),
	)

	bobBoardResp := waitForBoardRegistered(t, bob.RPCClient)
	require.Equal(t, "registered", bobBoardResp.Status)

	var replacementRoundID string
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(
			t.Context(), defaultSmallTimeout,
		)
		defer cancel()

		resp, err := bob.RPCClient.ListRounds(
			ctx, &daemonrpc.ListRoundsRequest{},
		)
		if err != nil {
			return false
		}

		for _, round := range resp.Rounds {
			if round.IsTemp || round.RoundId == "" {
				continue
			}

			if round.RoundId == registeredRoundID {
				continue
			}

			if round.State != registrationSent {
				continue
			}

			replacementRoundID = round.RoundId

			return true
		}

		return false
	}, defaultTimeout, pollInterval,
		"bob never registered into a replacement round")

	t.Logf(
		"bob registered into replacement round_id=%q",
		replacementRoundID,
	)
}

// TestBoardingIntegrationMultiTreeRound reproduces issue #312: when a round
// produces more than one VTXO tree, the operator merges aggregated nonces
// across every TreeSignCoordinator into one map per client. Each client signs
// the merged map and returns a single sigs map covering every tree. The round
// FSM then feeds that map to every TreeSignCoordinator in turn.
//
// TreeSignCoordinator.AddPartialSignatures rejects any txid it doesn't own
// instead of silently skipping it (as AddNonces already does). With two trees
// the second coordinator immediately rejects the client's submission with
// "tx <txid> not found in coordinator", the round logs submitted=0 in
// AwaitingVTXOSignaturesState, and the VTXO-signature timeout fires the round
// into ROUND_STATUS_FAILED.
//
// This test forces multi-tree rounds by capping each tree at one VTXO
// (MaxVTXOsPerTree=1) and boarding two clients into the same round. It pins
// down the current (buggy) behavior so the follow-up fix can flip the
// assertion to expect a successful broadcast.
func TestBoardingIntegrationMultiTreeRound(t *testing.T) {
	t.Parallel()

	clientOpts := client_harness.DefaultOptions()
	clientOpts.GroupName = t.Name()
	clientOpts.StartTapd = false

	h := harness.NewArkHarness(t, &harness.ArkHarnessOptions{
		ClientOptions: &clientOpts,
		OperatorConfigMutator: func(cfg *darepo.Config) {
			// Force the round to build more than one VTXO tree by
			// capping each tree at a single VTXO. With two clients
			// boarding into the same round this yields two trees,
			// exercising the cross-tree partial-signature path that
			// AddPartialSignatures must accept.
			cfg.Rounds.MaxVTXOsPerTree = 1
		},
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
		{
			name:   "alice",
			client: alice.RPCClient,
		},
		{
			name:   "bob",
			client: bob.RPCClient,
		},
	} {
		newAddrResp, err := tc.client.NewAddress(
			t.Context(), &daemonrpc.NewAddressRequest{},
		)
		require.NoError(t, err, "%s NewAddress RPC failed", tc.name)
		require.NotEmpty(
			t, newAddrResp.Address, "%s boarding address should "+
				"be set", tc.name,
		)

		fundingTxID := h.Faucet(newAddrResp.Address, boardingAmount)
		t.Logf(
			"%s funded boarding address via txid=%s", tc.name,
			fundingTxID,
		)
	}

	h.Generate(int(operatorInfo.MinConfirmations) + 1)

	waitForConfirmedBoardingBalance(
		t, alice.RPCClient, int64(boardingAmount),
	)
	waitForConfirmedBoardingBalance(
		t, bob.RPCClient, int64(boardingAmount),
	)

	require.Equal(
		t, "registered",
		waitForBoardRegistered(t, alice.RPCClient).Status,
	)
	require.Equal(
		t, "registered",
		waitForBoardRegistered(t, bob.RPCClient).Status,
	)

	clientResp := waitForRegisteredClients(t, h, 2)
	require.Len(t, clientResp.Clients, 2)

	aliceJoined := waitForClientRoundState(
		t, alice.RPCClient, daemonrpc.RoundState_ROUND_STATE_JOINED,
	)
	bobJoined := waitForClientRoundState(
		t, bob.RPCClient, daemonrpc.RoundState_ROUND_STATE_JOINED,
	)
	require.Equal(
		t, aliceJoined.RoundId, bobJoined.RoundId,
		"alice and bob should join the same round",
	)
	t.Logf(
		"Both clients joined multi-tree round_id=%q",
		aliceJoined.RoundId,
	)

	// Both clients submit partial signatures across both trees and the
	// round broadcasts successfully. Each TreeSignCoordinator picks out
	// its own txids from the merged sigs map and silently skips the rest.
	broadcastRound := waitForOperatorRoundStatus(
		t, h, aliceJoined.RoundId,
		adminrpc.RoundStatus_ROUND_STATUS_BROADCAST,
	)
	require.NotEmpty(t, broadcastRound.TxId)
	t.Logf(
		"Multi-tree round broadcast: round_id=%q txid=%s",
		aliceJoined.RoundId, broadcastRound.TxId,
	)
}

// TestBoardingIntegrationReplayBoardAfterRestart pins down the fix for
// darepo-client#416: a client daemon restart between Board RPC admission
// and round seal must NOT silently drop the user's boarding request. The
// daemon persists the explicit Board request to disk in handleBoard, and on
// restart the wallet re-issues TriggerBoardMsg through the round actor so
// the client lands back in REGISTRATION_SENT without the user re-typing
// `board`.
//
// The test drives:
//  1. Fund a boarding address and call Board → registered.
//  2. Wait for the client to reach REGISTRATION_SENT (round actor admitted
//     the registration with the server).
//  3. Restart the client daemon. Pre-fix this loses the round and the
//     daemon comes back with zero rounds in any state.
//  4. WITHOUT calling Board again, wait for REGISTRATION_SENT to re-appear
//     on the restarted daemon — this is the replay path's observable.
//  5. Manually seal via the admin TriggerBatch RPC.
//  6. Mine to confirmation and assert a live VTXO of the expected amount.
//
// Step 4 is the key assertion: without the persistence + replay wiring the
// restarted daemon never re-registers, so REGISTRATION_SENT never reappears
// and the eventual seal/confirm assertions can never complete.
func TestBoardingIntegrationReplayBoardAfterRestart(t *testing.T) {
	t.Parallel()

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

	// First Board call admits the registration and persists the request to
	// pending_board_requests via handleBoard's UpsertPendingBoardRequest.
	boardResp := waitForBoardRegistered(t, alice.RPCClient)
	require.Equal(t, "registered", boardResp.Status)
	t.Logf(
		"Board admitted: vtxo_count=%d boarding=%d",
		boardResp.VtxoCount, boardingAmount,
	)

	// Wait for the registration to land server-side so we know the
	// pending row reflects an in-flight request, not a request that
	// silently failed.
	waitForClientRegistration(t, h)
	preRestartRound := waitForClientRoundState(
		t, alice.RPCClient,
		daemonrpc.RoundState_ROUND_STATE_REGISTRATION_SENT,
	)
	preRestartRoundID := preRestartRound.RoundId
	t.Logf(
		"Pre-restart client round: round_id=%q is_temp=%v",
		preRestartRoundID, preRestartRound.IsTemp,
	)

	// Restart the client daemon BEFORE the round seals. The in-memory
	// round FSM is lost; only persisted state survives. Without the #416
	// fix the client comes back with zero rounds and the rest of the
	// flow stalls waiting for a registration that will never re-issue.
	oldRPCAddr := alice.RPCAddr
	alice = h.RestartClientDaemon("alice")
	t.Logf(
		"Restarted client daemon: old_rpc=%s new_rpc=%s", oldRPCAddr,
		alice.RPCAddr,
	)

	// Observable for the replay path: a fresh REGISTRATION_SENT round
	// appears on the daemon WITHOUT the test calling Board again. The
	// round_id may or may not match preRestartRoundID — the operator
	// may admit the replay into a fresh round or reuse an assembling
	// one — so the test asserts the lifecycle state, not the id.
	//
	// We require a NON-TEMP round to ride out the brief temp→assigned
	// re-key window the round actor uses when ack'ing JoinRound. Filtering
	// IsTemp via ListRounds also matches the pattern in
	// TestBoardingIntegrationTriggerBatchCreatesNewRound.
	var replayedRoundID string
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(
			t.Context(), defaultSmallTimeout,
		)
		defer cancel()

		resp, err := alice.RPCClient.ListRounds(
			ctx, &daemonrpc.ListRoundsRequest{},
		)
		if err != nil {
			return false
		}

		for _, round := range resp.Rounds {
			if round.IsTemp || round.RoundId == "" {
				continue
			}

			target := daemonrpc.
				RoundState_ROUND_STATE_REGISTRATION_SENT
			if !roundStateSatisfiesTarget(round.State, target) {
				continue
			}

			replayedRoundID = round.RoundId

			return true
		}

		return false
	}, defaultTimeout, pollInterval,
		"replayed registration never produced a non-temp round")

	t.Logf(
		"Replayed registration after restart: round_id=%q "+
			"(pre_restart_round_id=%q)", replayedRoundID,
		preRestartRoundID,
	)

	// Drive the round to seal via the admin TriggerBatch RPC. Using
	// TriggerBatch (rather than letting the operator auto-seal on a
	// timer) keeps the test deterministic across CI runners with
	// variable scheduling.
	triggerCtx, triggerCancel := context.WithTimeout(
		t.Context(), defaultSmallTimeout,
	)
	defer triggerCancel()

	triggerResp, err := h.ArkAdminClient.TriggerBatch(
		triggerCtx, &adminrpc.TriggerBatchRequest{},
	)
	require.NoError(t, err, "TriggerBatch RPC failed")
	require.NotEmpty(
		t, triggerResp.RoundId,
		"TriggerBatch must seal a concrete round",
	)
	t.Logf(
		"TriggerBatch sealed replayed round_id=%q", triggerResp.RoundId,
	)

	// The sealed round progresses through signing. Track whichever round
	// the operator actually sealed (may differ from replayedRound if the
	// operator admitted the replay into a different assembling round).
	sealedRoundID := triggerResp.RoundId

	waitForNamedClientRoundState(
		t, alice.RPCClient, sealedRoundID,
		daemonrpc.RoundState_ROUND_STATE_INPUT_SIG_SENT,
	)

	broadcastRound := waitForOperatorRoundStatus(
		t, h, sealedRoundID,
		adminrpc.RoundStatus_ROUND_STATUS_BROADCAST,
	)
	require.NotEmpty(t, broadcastRound.TxId)
	t.Logf(
		"Replayed round broadcast: round_id=%q txid=%s", sealedRoundID,
		broadcastRound.TxId,
	)

	mineUntilOperatorRoundConfirmed(
		t, h, sealedRoundID, broadcastRound.TxId,
	)

	confirmedRound := waitForNamedClientRoundState(
		t, alice.RPCClient, sealedRoundID,
		daemonrpc.RoundState_ROUND_STATE_CONFIRMED,
	)
	require.False(
		t, confirmedRound.IsTemp,
		"confirmed round after replay must be persisted",
	)

	waitForOperatorRoundStatus(
		t, h, sealedRoundID,
		adminrpc.RoundStatus_ROUND_STATUS_CONFIRMED,
	)

	liveVTXO := waitForLiveVTXO(t, alice.RPCClient, sealedRoundID)
	expectedNet := expectedNetAfterBoarding(
		t, int64(boardingAmount), defaultItestBatchSize,
	)
	require.Equal(
		t, expectedNet, liveVTXO.AmountSat, "replayed board must "+
			"produce the same net VTXO value as the original "+
			"board would have",
	)

	finalBalance := waitForVTXOBalance(
		t, alice.RPCClient, liveVTXO.AmountSat,
	)
	require.Equal(t, liveVTXO.AmountSat, finalBalance.VtxoBalanceSat)
	t.Logf(
		"Replayed board completed: round_id=%q vtxo_amount=%d",
		sealedRoundID, liveVTXO.AmountSat,
	)
}
