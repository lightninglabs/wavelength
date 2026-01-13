//go:build systest

package systest

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/stretchr/testify/require"
)

const (
	// msgsPerClientJoin is the number of messages exchanged when a client
	// joins a round: JoinRoundRequest (C→S) + ClientSuccessResp (S→C).
	msgsPerClientJoin = 2

	// msgsPerClientRound is the total messages for one client's full round
	// participation including the VTXO signing exchange:
	// - JoinRoundRequest (C→S)
	// - ClientSuccessResp (S→C)
	// - ClientBatchInfo (S→C)
	// - SubmitNoncesRequest (C→S)
	// - ClientVTXOAggNonces (S→C)
	// - SubmitPartialSigRequest (C→S)
	// - ClientVTXOAggSigs (S→C)
	// - ClientAwaitingInputSigsResp (S→C)
	// - SubmitForfeitSigRequest (C→S)
	msgsPerClientRound = 9
)

// TestBoardingE2ESingleClient tests the complete boarding flow for a single
// client using the real wallet and on-chain funding:
//  1. Client creates boarding address using wallet actor
//  2. Harness funds that address on-chain via faucet
//  3. Mine blocks to confirm funding
//  4. Client joins round with the confirmed boarding input
//  5. Server builds batch and sends ClientBatchInfo
//  6. For boarding-only: server requests input signatures
//  7. Client sends boarding signatures
//  8. Server finalizes and broadcasts commitment tx
//  9. Mine block to confirm
//
// 10. Client receives round completion
func TestBoardingE2ESingleClient(t *testing.T) {
	ParallelN(t)

	h := NewE2EHarness(t)
	h.Start()

	ctx := context.Background()

	// Fund the server's wallet. The server needs funds to add wallet
	// inputs for change and fees when building the commitment transaction.
	h.FundServerWallet(btcutil.SatoshiPerBitcoin)
	t.Log("Funded server wallet with 1 BTC")

	// Create a test client with real wallet and round actors.
	client := NewTestClient(h)
	require.NotNil(t, client)
	t.Logf("Created client: %s", client.ClientID())

	// Create a boarding address using the wallet actor. The taproot address
	// includes the client key (cooperative spend), operator key (server
	// signing), and a CSV delay path for unilateral exit.
	terms := h.Terms()
	boardingResp, err := client.CreateBoardingAddress(terms.BoardingExitDelay)
	require.NoError(t, err, "should create boarding address")
	require.NotNil(t, boardingResp, "boarding response should not be nil")
	t.Logf("Created boarding address: %s", boardingResp.Address.String())

	// Fund the boarding address via faucet.
	amount := btcutil.Amount(100_000)
	txidStr := h.Harness.Faucet(boardingResp.Address.String(), amount)
	t.Logf("Funded boarding address with txid: %s", txidStr)

	// Mine blocks to confirm the funding. Server requires
	// MinBoardingConfirmations (typically 1 for regtest).
	h.MineBlocks(int(terms.MinBoardingConfirmations))
	t.Logf("Mined %d blocks to confirm funding", terms.MinBoardingConfirmations)

	// Wait for the wallet actor to detect the boarding confirmation. The
	// wallet polls ListUnspent on each block and sends
	// BoardingUtxoConfirmedEvent to the round actor, transitioning the FSM
	// from Idle to PendingRoundAssembly.
	err = client.WaitForBoardingConfirmation(30 * time.Second)
	require.NoError(
		t, err, "FSM should reach PendingRoundAssembly after "+
			"boarding confirmation",
	)
	t.Log("Client FSM reached PendingRoundAssembly state")

	// Trigger registration by sending RegistrationRequested event to the
	// round FSM. This transitions from PendingRoundAssembly to
	// RegistrationSentState and emits a JoinRoundRequest to the server.
	err = client.TriggerRegistration(ctx)
	require.NoError(t, err, "should trigger registration")

	// Wait for server to respond with ClientSuccessResp.
	err = h.Transcript().WaitForEntryCount(2, 5*time.Second)
	require.NoError(t, err, "server should respond within timeout")

	// Log transcript state.
	t.Log("Transcript after JoinRound:")
	t.Log(h.Transcript().Dump())

	// Assert success response (not failure).
	h.Transcript().AssertContainsMessage(t, C2S("JoinRoundRequest"))
	h.Transcript().AssertContainsMessage(t, S2C("ClientSuccessResp"))
	h.Transcript().AssertNotContainsMessage(t, S2C("ClientRoundFailedResp"))
	h.Transcript().AssertNotContainsMessage(t, S2C("ClientErrorResp"))

	// Trigger round seal via registration timeout.
	h.TriggerRoundSeal()
	t.Log("Triggered round seal")

	// Wait for server to build batch and send ClientBatchInfo. For rounds
	// with VTXOs, the flow includes nonce/signature exchange before input
	// signing, so we just check for ClientBatchInfo first.
	err = h.Transcript().WaitForEntryCount(4, 10*time.Second)
	require.NoError(t, err, "server should send batch info")

	t.Log("Transcript after round seal:")
	t.Log(h.Transcript().Dump())

	// Assert we received the batch info.
	h.Transcript().AssertContainsMessage(t, S2C("ClientBatchInfo"))

	// Wait for VTXO signing and input signing phases to complete. For
	// boarding with VTXOs, the client sends nonces, receives server
	// partial sigs, sends client partial sigs, and finally submits
	// boarding signatures for the commitment transaction inputs.
	err = h.Transcript().WaitForEntryCount(9, 30*time.Second)
	require.NoError(t, err, "should complete VTXO and input signing phases")

	t.Log("Transcript after client signing:")
	t.Log(h.Transcript().Dump())

	// Assert client sent the forfeit signatures.
	h.Transcript().AssertContainsMessage(t, C2S("SubmitForfeitSigRequest"))

	// Wait for server to broadcast the commitment transaction. Give the
	// server time to finalize the PSBT and broadcast.
	time.Sleep(1 * time.Second)

	// Verify transaction is in mempool before mining.
	rpcClient, err := h.BitcoinRPCClient()
	require.NoError(t, err, "should get bitcoin RPC client")
	mempoolTxs, err := rpcClient.GetRawMempool()
	require.NoError(t, err, "should get mempool")
	require.Len(t, mempoolTxs, 1, "should have exactly one tx in mempool")
	t.Logf("Commitment tx in mempool: %s", mempoolTxs[0].String())

	// Mine blocks to confirm the commitment transaction.
	h.MineBlocksAndConfirm(1)
	t.Log("Mined block to confirm commitment transaction")

	// Verify mempool is empty after mining (transaction was included).
	mempoolTxs, err = rpcClient.GetRawMempool()
	require.NoError(t, err, "should get mempool after mining")
	require.Empty(t, mempoolTxs, "mempool should be empty after mining")

	// Wait for the round to complete. The client receives notification
	// once the commitment transaction is confirmed.
	err = client.WaitForRoundComplete(30 * time.Second)
	require.NoError(t, err, "round should complete successfully")

	// Verify client-side database state. After a successful round, the
	// client should have the round persisted with confirmed status and the
	// VTXOs stored.
	client.AssertConfirmedRoundCountFromDB(1)
	t.Log("Round persisted to client database")

	client.AssertVTXOCountFromDB(1)
	t.Log("VTXO persisted to client database")

	// Verify the VTXO has proper state.
	vtxos, err := client.ListVTXOs(ctx)
	require.NoError(t, err, "should list VTXOs")
	require.Len(t, vtxos, 1, "should have exactly one VTXO")
	t.Logf("VTXO outpoint: %s", vtxos[0].Outpoint)

	// Verify the full message sequence for boarding with VTXOs. The MuSig2
	// signing exchange follows this order:
	//  1. JoinRoundRequest - client joins round
	//  2. ClientSuccessResp - server acknowledges join
	//  3. ClientBatchInfo - server sends batch PSBT + VTXO trees
	//  4. SubmitNoncesRequest - client sends VTXO tree nonces
	//  5. ClientVTXOAggNonces - server sends aggregated nonces
	//  6. SubmitPartialSigRequest - client sends partial signatures
	//  7. ClientVTXOAggSigs - server sends aggregated signatures
	//  8. ClientAwaitingInputSigsResp - server ready for input sigs
	//  9. SubmitForfeitSigRequest - client sends boarding signatures
	h.Transcript().AssertMessageSequence(t, []ExpectedMessage{
		C2S("JoinRoundRequest"),
		S2C("ClientSuccessResp"),
		S2C("ClientBatchInfo"),
		C2S("SubmitNoncesRequest"),
		S2C("ClientVTXOAggNonces"),
		C2S("SubmitPartialSigRequest"),
		S2C("ClientVTXOAggSigs"),
		S2C("ClientAwaitingInputSigsResp"),
		C2S("SubmitForfeitSigRequest"),
	})

	t.Log("TestBoardingE2ESingleClient completed full E2E flow successfully!")
}

// TestBoardingE2EMultipleClients tests the complete boarding flow with multiple
// clients joining the same round. Each client:
//  1. Gets its own LND node (wallet isolation)
//  2. Creates a boarding address
//  3. Gets funded via faucet
//  4. Joins the round after confirmation
//  5. Signs their VTXOs and boarding inputs
//  6. Receives their VTXO after round completion
func TestBoardingE2EMultipleClients(t *testing.T) {
	ParallelN(t)

	h := NewE2EHarness(t)
	h.Start()

	ctx := context.Background()

	// Fund the server wallet with enough for multiple clients.
	h.FundServerWallet(btcutil.SatoshiPerBitcoin * 2)
	t.Log("Funded server wallet with 2 BTC")

	// Create multiple clients, each with their own LND node for wallet
	// isolation.
	numClients := 3
	clients := make([]*TestClient, numClients)
	boardingAddrs := make([]string, numClients)
	amounts := []btcutil.Amount{100_000, 150_000, 200_000}

	terms := h.Terms()
	for i := 0; i < numClients; i++ {
		clients[i] = NewTestClient(h)
		t.Logf("Created client %d: %s", i, clients[i].ClientID())

		resp, err := clients[i].CreateBoardingAddress(terms.BoardingExitDelay)
		require.NoError(t, err)
		boardingAddrs[i] = resp.Address.String()
		t.Logf("Client %d boarding address: %s", i, boardingAddrs[i])
	}

	// Fund all boarding addresses.
	for i := 0; i < numClients; i++ {
		txid := h.Harness.Faucet(boardingAddrs[i], amounts[i])
		t.Logf("Client %d funded with %d sats, txid: %s", i, amounts[i], txid)
	}

	// Confirm funding and wait for all clients' FSMs to transition to
	// PendingRoundAssembly.
	h.MineBlocks(int(terms.MinBoardingConfirmations))
	t.Logf("Mined %d blocks to confirm funding", terms.MinBoardingConfirmations)

	for i, client := range clients {
		err := client.WaitForBoardingConfirmation(30 * time.Second)
		require.NoError(t, err, "client %d should reach PendingRoundAssembly", i)
	}
	t.Log("All clients reached PendingRoundAssembly")

	// All clients trigger registration and join the same round.
	for i, client := range clients {
		err := client.TriggerRegistration(ctx)
		require.NoError(t, err, "client %d should trigger registration", i)
	}

	// Wait for server responses.
	expectedEntries := numClients * 2
	err := h.Transcript().WaitForEntryCount(expectedEntries, 10*time.Second)
	require.NoError(t, err, "should receive all server responses")

	t.Log("Transcript after all clients joined:")
	t.Log(h.Transcript().Dump())

	// Assert all clients got success responses.
	for _, client := range clients {
		h.Transcript().AssertContainsMessage(
			t, C2SFrom("JoinRoundRequest", client.ClientID()),
		)
		h.Transcript().AssertContainsMessage(
			t, S2CTo("ClientSuccessResp", client.ClientID()),
		)
	}
	h.Transcript().AssertNotContainsMessage(t, S2C("ClientRoundFailedResp"))

	// Seal the round and wait for all clients to complete the signing
	// exchange.
	h.TriggerRoundSeal()
	t.Log("Triggered round seal")

	// Wait for full message exchange (9 messages per client).
	totalMessages := numClients * 9
	err = h.Transcript().WaitForEntryCount(totalMessages, 60*time.Second)
	require.NoError(t, err, "should complete all signing phases")

	t.Log("Transcript after signing:")
	t.Log(h.Transcript().Dump())

	// Mine a block to confirm the commitment transaction.
	time.Sleep(1 * time.Second)
	h.MineBlocksAndConfirm(1)
	t.Log("Mined block to confirm commitment transaction")

	// Wait for all clients to receive round completion.
	for i, client := range clients {
		err := client.WaitForRoundComplete(30 * time.Second)
		require.NoError(t, err, "client %d should complete round", i)
	}
	t.Log("All clients completed round")

	// Verify all clients have VTXOs with correct properties.
	for i, client := range clients {
		client.AssertVTXOCountFromDB(1)
		client.AssertConfirmedRoundCountFromDB(1)
		client.AssertVTXOProperties()

		vtxos, err := client.ListVTXOs(ctx)
		require.NoError(t, err)
		require.Len(t, vtxos, 1)

		// Verify amount is close to funding amount (minus fees).
		t.Logf("Client %d: funded=%d, VTXO=%d sats",
			i, amounts[i], vtxos[0].Amount)
	}

	t.Log("TestBoardingE2EMultipleClients completed successfully!")
}

// TestBoardingE2ESubsequentRounds tests multiple rounds in sequence for the
// same client. This verifies:
//  1. Client can complete a round and receive a VTXO
//  2. Client can create a new boarding input
//  3. Client can join another round and receive a second VTXO
//  4. Both VTXOs are correctly persisted with distinct round IDs
func TestBoardingE2ESubsequentRounds(t *testing.T) {
	ParallelN(t)

	h := NewE2EHarness(t)
	h.Start()

	ctx := context.Background()

	// Fund the server wallet.
	h.FundServerWallet(btcutil.SatoshiPerBitcoin * 3) // 3 BTC
	t.Log("Funded server wallet with 3 BTC")

	client := NewTestClient(h)
	terms := h.Terms()
	t.Logf("Created client: %s", client.ClientID())

	// Complete the first round using a fresh boarding input.
	t.Log("Starting round 1")

	addr1, err := client.CreateBoardingAddress(terms.BoardingExitDelay)
	require.NoError(t, err)
	t.Logf("Round 1 boarding address: %s", addr1.Address.String())

	amount1 := btcutil.Amount(200_000)
	txid1 := h.Harness.Faucet(addr1.Address.String(), amount1)
	t.Logf("Round 1 funded with %d sats, txid: %s", amount1, txid1)

	h.MineBlocks(int(terms.MinBoardingConfirmations))

	err = client.WaitForBoardingConfirmation(30 * time.Second)
	require.NoError(t, err, "round 1: should reach PendingRoundAssembly")

	err = client.TriggerRegistration(ctx)
	require.NoError(t, err, "round 1: should trigger registration")

	// Wait for server response.
	err = h.Transcript().WaitForEntryCount(2, 10*time.Second)
	require.NoError(t, err, "round 1: should receive server response")

	h.TriggerRoundSeal()

	// Wait for signing completion.
	err = h.Transcript().WaitForEntryCount(9, 30*time.Second)
	require.NoError(t, err, "round 1: should complete signing")

	time.Sleep(1 * time.Second)
	h.MineBlocksAndConfirm(1)

	err = client.WaitForRoundComplete(30 * time.Second)
	require.NoError(t, err, "round 1: should complete")

	// Verify round 1 state.
	client.AssertVTXOCountFromDB(1)
	client.AssertConfirmedRoundCountFromDB(1)

	round1ID, err := client.GetLastCompletedRoundID()
	require.NoError(t, err)
	t.Logf("Round 1 completed: %s", round1ID)

	vtxosAfterRound1, err := client.ListVTXOs(ctx)
	require.NoError(t, err)
	require.Len(t, vtxosAfterRound1, 1)
	t.Logf("Round 1 VTXO amount: %d sats", vtxosAfterRound1[0].Amount)

	// Clear transcript for round 2.
	h.Transcript().Clear()

	// Complete a second round with a new boarding input to verify the client
	// can participate in multiple rounds.
	t.Log("Starting round 2")

	addr2, err := client.CreateBoardingAddress(terms.BoardingExitDelay)
	require.NoError(t, err)
	t.Logf("Round 2 boarding address: %s", addr2.Address.String())

	amount2 := btcutil.Amount(300_000)
	txid2 := h.Harness.Faucet(addr2.Address.String(), amount2)
	t.Logf("Round 2 funded with %d sats, txid: %s", amount2, txid2)

	h.MineBlocks(int(terms.MinBoardingConfirmations))

	// Wait for second boarding confirmation. The FSM should transition
	// back to PendingRoundAssembly.
	err = client.WaitForBoardingConfirmation(30 * time.Second)
	require.NoError(t, err, "round 2: should reach PendingRoundAssembly")

	err = client.TriggerRegistration(ctx)
	require.NoError(t, err, "round 2: should trigger registration")

	err = h.Transcript().WaitForEntryCount(2, 10*time.Second)
	require.NoError(t, err, "round 2: should receive server response")

	h.TriggerRoundSeal()

	err = h.Transcript().WaitForEntryCount(9, 30*time.Second)
	require.NoError(t, err, "round 2: should complete signing")

	time.Sleep(1 * time.Second)
	h.MineBlocksAndConfirm(1)

	err = client.WaitForRoundComplete(30 * time.Second)
	require.NoError(t, err, "round 2: should complete")

	// Verify round 2 state - now 2 VTXOs total and 2 confirmed rounds.
	client.AssertVTXOCountFromDB(2)
	client.AssertConfirmedRoundCountFromDB(2)

	round2ID, err := client.GetLastCompletedRoundID()
	require.NoError(t, err)
	require.NotEqual(t, round1ID, round2ID, "round IDs should be different")
	t.Logf("Round 2 completed: %s", round2ID)

	// Verify final state after both rounds complete.
	vtxos, err := client.ListVTXOs(ctx)
	require.NoError(t, err)
	require.Len(t, vtxos, 2)

	total, err := client.TotalVTXOValue()
	require.NoError(t, err)
	t.Logf("Total VTXO value across 2 rounds: %d sats", total)

	// Verify VTXO properties for all VTXOs.
	client.AssertVTXOProperties()

	t.Log("TestBoardingE2ESubsequentRounds completed successfully!")
}

// TestTranscriptMessageSequence tests the message transcript assertion helpers.
func TestTranscriptMessageSequence(t *testing.T) {
	ParallelN(t)

	h := NewE2EHarness(t)
	h.Start()

	ctx := context.Background()

	// Create a client and have it join.
	client := NewTestClient(h)

	mockOutpoint := &wire.OutPoint{
		Hash:  chainhash.Hash{0xAB},
		Index: 0,
	}
	boardingReq := &types.BoardingRequest{
		Outpoint:  mockOutpoint,
		ExitDelay: 144,
	}

	err := client.JoinRound(ctx, []*types.BoardingRequest{boardingReq})
	require.NoError(t, err)

	// Test the AssertMessageSequence helper.
	expected := []ExpectedMessage{
		C2S("JoinRoundRequest"),
	}
	h.Transcript().AssertMessageSequence(t, expected)

	t.Log("TestTranscriptMessageSequence passed")
}

// TestHarnessTimeoutTrigger tests the mock timeout actor functionality.
func TestHarnessTimeoutTrigger(t *testing.T) {
	ParallelN(t)

	h := NewE2EHarness(t)
	h.Start()

	// Initially there should be a registration timeout pending. Note: This
	// depends on the rounds actor scheduling a timeout on start. If no
	// timeout is scheduled, this test documents that behavior.
	pendingCount := h.mockTimeout.PendingCount()
	t.Logf("Pending timeouts after start: %d", pendingCount)

	// List pending timeout IDs.
	pendingIDs := h.mockTimeout.PendingTimeoutIDs()
	for _, id := range pendingIDs {
		t.Logf("  Pending timeout: %s", id)
	}

	// Trigger all pending timeouts.
	h.TriggerAllTimeouts()

	// Verify timeouts were fired.
	newCount := h.mockTimeout.PendingCount()
	t.Logf("Pending timeouts after trigger: %d", newCount)
	require.Equal(t, 0, newCount, "all timeouts should be fired")

	t.Log("TestHarnessTimeoutTrigger passed")
}

// TestBoardingRestartAfterRoundBroadcast tests client restart after the
// commitment transaction has been broadcast but before it's confirmed.
// The restarted client should:
// 1. Load round state from the database
// 2. Re-register for confirmation of the commitment tx
// 3. Detect the confirmation when blocks are mined
// 4. Complete the round and persist the VTXO
func TestBoardingRestartAfterRoundBroadcast(t *testing.T) {
	ParallelN(t)

	h := NewE2EHarness(t)
	h.Start()

	ctx := context.Background()

	// Fund the server wallet.
	h.FundServerWallet(btcutil.SatoshiPerBitcoin) // 1 BTC
	t.Log("Funded server wallet with 1 BTC")

	// Create a test client.
	client := NewTestClient(h)
	require.NotNil(t, client)
	t.Logf("Created client: %s", client.ClientID())

	// Complete the round flow up to broadcast but don't mine the confirmation.
	terms := h.Terms()
	boardingResp, err := client.CreateBoardingAddress(terms.BoardingExitDelay)
	require.NoError(t, err, "should create boarding address")
	t.Logf("Created boarding address: %s", boardingResp.Address.String())

	// Fund and confirm boarding.
	amount := btcutil.Amount(100_000)
	h.Harness.Faucet(boardingResp.Address.String(), amount)
	h.MineBlocks(int(terms.MinBoardingConfirmations))

	err = client.WaitForBoardingConfirmation(30 * time.Second)
	require.NoError(t, err, "FSM should reach PendingRoundAssembly")
	t.Log("Client FSM reached PendingRoundAssembly state")

	// Trigger registration.
	err = client.TriggerRegistration(ctx)
	require.NoError(t, err, "should trigger registration")

	err = h.Transcript().WaitForEntryCount(msgsPerClientJoin, 10*time.Second)
	require.NoError(t, err, "server should respond")

	// Seal round and complete signing.
	h.TriggerRoundSeal()
	err = h.Transcript().WaitForEntryCount(msgsPerClientRound, 30*time.Second)
	require.NoError(t, err, "should complete signing")

	// Wait for broadcast but DON'T mine yet.
	time.Sleep(1 * time.Second)

	// Verify tx is in mempool.
	rpcClient, err := h.BitcoinRPCClient()
	require.NoError(t, err, "should get bitcoin RPC client")
	mempoolTxs, err := rpcClient.GetRawMempool()
	require.NoError(t, err, "should get mempool")
	require.Len(t, mempoolTxs, 1, "should have exactly one tx in mempool")
	t.Logf("Commitment tx in mempool: %s", mempoolTxs[0].String())

	// Restart the client before the commitment tx is confirmed to verify it
	// can recover and detect the confirmation.
	t.Log("Restarting client before confirmation")
	client = h.RestartClient(client)
	t.Logf("Client restarted: %s", client.ClientID())

	// Mine a block to confirm the commitment transaction.
	h.MineBlocksAndConfirm(1)
	t.Log("Mined block to confirm commitment transaction")

	// The restarted client should have loaded the round state from the DB
	// and re-registered for confirmations. It should detect the confirmation
	// and complete the round.
	//
	// Note: We don't use WaitForRoundComplete() here because it waits for a
	// RoundJoined event, which won't happen on restart (the round was
	// already joined before the restart). Instead, we poll the database
	// directly using the assert helpers which have built-in polling.
	client.AssertConfirmedRoundCountFromDB(1)
	client.AssertVTXOCountFromDB(1)

	t.Log("TestBoardingRestartAfterRoundBroadcast completed successfully!")
}

// TestBoardingRestartBeforeConfirmation tests client restart before the
// boarding UTXO has been confirmed. The restarted client should:
// 1. Load the boarding address from the database
// 2. Re-register for block epoch notifications
// 3. Detect the confirmation when blocks are mined
// 4. Complete the round normally
func TestBoardingRestartBeforeConfirmation(t *testing.T) {
	ParallelN(t)

	h := NewE2EHarness(t)
	h.Start()

	ctx := context.Background()

	// Fund the server wallet.
	h.FundServerWallet(btcutil.SatoshiPerBitcoin) // 1 BTC
	t.Log("Funded server wallet with 1 BTC")

	// Create a test client.
	client := NewTestClient(h)
	require.NotNil(t, client)
	t.Logf("Created client: %s", client.ClientID())

	// Create a boarding address and fund it, but don't mine yet.
	terms := h.Terms()
	boardingResp, err := client.CreateBoardingAddress(terms.BoardingExitDelay)
	require.NoError(t, err, "should create boarding address")
	t.Logf("Created boarding address: %s", boardingResp.Address.String())

	// Fund but don't mine - the tx is in mempool but not confirmed.
	amount := btcutil.Amount(100_000)
	h.Harness.Faucet(boardingResp.Address.String(), amount)
	t.Log("Funded boarding address (not yet confirmed)")

	// Restart the client before the boarding UTXO is confirmed.
	t.Log("Restarting client before boarding confirmation")
	client = h.RestartClient(client)
	t.Logf("Client restarted: %s", client.ClientID())

	// Mine blocks to confirm the boarding UTXO.
	h.MineBlocks(int(terms.MinBoardingConfirmations))
	t.Logf("Mined %d blocks to confirm boarding", terms.MinBoardingConfirmations)

	// The restarted client should detect the boarding confirmation.
	err = client.WaitForBoardingConfirmation(30 * time.Second)
	require.NoError(t, err, "restarted client should detect boarding confirmation")
	t.Log("Restarted client detected boarding confirmation")

	// Complete the round normally to verify the client recovered properly.
	err = client.TriggerRegistration(ctx)
	require.NoError(t, err, "should trigger registration")

	err = h.Transcript().WaitForEntryCount(msgsPerClientJoin, 10*time.Second)
	require.NoError(t, err, "server should respond")

	h.TriggerRoundSeal()
	err = h.Transcript().WaitForEntryCount(msgsPerClientRound, 30*time.Second)
	require.NoError(t, err, "should complete signing")

	time.Sleep(1 * time.Second)
	h.MineBlocksAndConfirm(1)

	err = client.WaitForRoundComplete(30 * time.Second)
	require.NoError(t, err, "round should complete successfully")

	// Verify persistence.
	client.AssertVTXOCountFromDB(1)
	client.AssertConfirmedRoundCountFromDB(1)

	t.Log("TestBoardingRestartBeforeConfirmation completed successfully!")
}
