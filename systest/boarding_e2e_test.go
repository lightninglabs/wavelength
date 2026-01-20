//go:build systest

package systest

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/google/uuid"
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

	// Register VTXO requests with desired amounts. Amount should be
	// boarding amount minus estimated fees (5000 sats for simplicity).
	vtxoAmount := amount - 5000
	err = client.RegisterVTXORequests(ctx, []btcutil.Amount{vtxoAmount})
	require.NoError(t, err, "should register VTXO requests")
	t.Logf("Registered VTXO request for %d sats", vtxoAmount)

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

	// Get block height before mining for BatchWatcher assertions.
	blockCountBeforeConfirm, err := rpcClient.GetBlockCount()
	require.NoError(t, err, "should get block count")

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

	// Get the round ID for BatchWatcher verification.
	roundID, err := client.GetLastCompletedRoundID()
	require.NoError(t, err, "should get last completed round ID")

	// Verify the batch was registered with the BatchWatcher. The
	// confirmation height is the block after pre-confirm count.
	confirmationHeight := uint32(blockCountBeforeConfirm) + 1
	h.AssertBatchRegistered(uuid.UUID(roundID), confirmationHeight, 1)
	t.Log("Verified batch registered with BatchWatcher")

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

// TestBoardingE2EMultipleVTXOs tests that a client can board with a single
// input of size N and receive multiple VTXOs. This demonstrates the decoupled
// VTXO request model where VTXOs are explicitly registered independent of
// boarding inputs.
func TestBoardingE2EMultipleVTXOs(t *testing.T) {
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

	// Create a boarding address.
	terms := h.Terms()
	boardingResp, err := client.CreateBoardingAddress(terms.BoardingExitDelay)
	require.NoError(t, err, "should create boarding address")
	t.Logf("Created boarding address: %s", boardingResp.Address.String())

	// Fund with 200,000 sats (N).
	boardingAmount := btcutil.Amount(200_000)
	txidStr := h.Harness.Faucet(boardingResp.Address.String(), boardingAmount)
	t.Logf("Funded boarding address with %d sats, txid: %s", boardingAmount, txidStr)

	// Mine blocks to confirm the funding.
	h.MineBlocks(int(terms.MinBoardingConfirmations))
	t.Logf("Mined %d blocks to confirm funding", terms.MinBoardingConfirmations)

	// Wait for boarding confirmation.
	err = client.WaitForBoardingConfirmation(30 * time.Second)
	require.NoError(t, err, "FSM should reach PendingRoundAssembly")
	t.Log("Client FSM reached PendingRoundAssembly state")

	// Register TWO VTXO requests of ~N/2 each (minus fees).
	// Estimate 5000 sats total operator fee, so each VTXO gets ~97,500 sats.
	vtxoAmount := (boardingAmount - 5000) / 2
	err = client.RegisterVTXORequests(ctx, []btcutil.Amount{
		vtxoAmount,
		vtxoAmount,
	})
	require.NoError(t, err, "should register VTXO requests")
	t.Logf("Registered 2 VTXO requests for %d sats each", vtxoAmount)

	// Trigger registration.
	err = client.TriggerRegistration(ctx)
	require.NoError(t, err, "should trigger registration")

	// Wait for server response.
	err = h.Transcript().WaitForEntryCount(2, 5*time.Second)
	require.NoError(t, err, "server should respond within timeout")

	t.Log("Transcript after JoinRound:")
	t.Log(h.Transcript().Dump())

	// Assert success response.
	h.Transcript().AssertContainsMessage(t, C2S("JoinRoundRequest"))
	h.Transcript().AssertContainsMessage(t, S2C("ClientSuccessResp"))
	h.Transcript().AssertNotContainsMessage(t, S2C("ClientRoundFailedResp"))
	h.Transcript().AssertNotContainsMessage(t, S2C("ClientErrorResp"))

	// Seal the round.
	h.TriggerRoundSeal()
	t.Log("Triggered round seal")

	// Wait for full signing exchange.
	err = h.Transcript().WaitForEntryCount(9, 30*time.Second)
	require.NoError(t, err, "should complete VTXO and input signing phases")

	t.Log("Transcript after signing:")
	t.Log(h.Transcript().Dump())

	// Wait for broadcast.
	time.Sleep(1 * time.Second)

	// Verify tx in mempool.
	rpcClient, err := h.BitcoinRPCClient()
	require.NoError(t, err, "should get bitcoin RPC client")
	mempoolTxs, err := rpcClient.GetRawMempool()
	require.NoError(t, err, "should get mempool")
	require.Len(t, mempoolTxs, 1, "should have exactly one tx in mempool")
	t.Logf("Commitment tx in mempool: %s", mempoolTxs[0].String())

	// Get block height before mining for BatchWatcher assertions.
	blockCountBeforeConfirm, err := rpcClient.GetBlockCount()
	require.NoError(t, err, "should get block count")

	// Mine to confirm.
	h.MineBlocksAndConfirm(1)
	t.Log("Mined block to confirm commitment transaction")

	// Wait for round completion.
	err = client.WaitForRoundComplete(30 * time.Second)
	require.NoError(t, err, "round should complete successfully")

	// Verify the batch was registered with the BatchWatcher.
	roundID, err := client.GetLastCompletedRoundID()
	require.NoError(t, err, "should get last completed round ID")
	confirmationHeight := uint32(blockCountBeforeConfirm) + 1
	h.AssertBatchRegistered(uuid.UUID(roundID), confirmationHeight, 1)
	t.Log("Verified batch registered with BatchWatcher")

	// Verify TWO VTXOs in database.
	client.AssertVTXOCountFromDB(2)
	t.Log("Two VTXOs persisted to client database")

	// Verify VTXO amounts.
	vtxos, err := client.ListVTXOs(ctx)
	require.NoError(t, err, "should list VTXOs")
	require.Len(t, vtxos, 2, "should have exactly two VTXOs")

	for i, vtxo := range vtxos {
		t.Logf("VTXO %d: outpoint=%s, amount=%d sats",
			i+1, vtxo.Outpoint, vtxo.Amount)
		// Each should be close to vtxoAmount (allow for small variance).
		require.InDelta(t, int64(vtxoAmount), int64(vtxo.Amount), 1000,
			"VTXO amount should be approximately %d", vtxoAmount)
	}

	t.Log("TestBoardingE2EMultipleVTXOs completed successfully!")
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

	// Register VTXO requests for all clients. Each client requests a VTXO
	// with their boarding amount minus estimated fees.
	for i, client := range clients {
		vtxoAmount := amounts[i] - 5000
		err := client.RegisterVTXORequests(ctx, []btcutil.Amount{vtxoAmount})
		require.NoError(t, err, "client %d should register VTXO requests", i)
	}
	t.Log("All clients registered VTXO requests")

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

	// Get block height before mining for BatchWatcher assertions.
	rpcClient, err := h.BitcoinRPCClient()
	require.NoError(t, err, "should get bitcoin RPC client")
	blockCountBeforeConfirm, err := rpcClient.GetBlockCount()
	require.NoError(t, err, "should get block count")

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

	// Verify the batch was registered with the BatchWatcher. All clients
	// share the same round, so we just need to get the round ID from one.
	roundID, err := clients[0].GetLastCompletedRoundID()
	require.NoError(t, err, "should get last completed round ID")
	confirmationHeight := uint32(blockCountBeforeConfirm) + 1
	h.AssertBatchRegistered(uuid.UUID(roundID), confirmationHeight, 1)
	t.Log("Verified batch registered with BatchWatcher")

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

	vtxoAmount1 := amount1 - 5000
	err = client.RegisterVTXORequests(ctx, []btcutil.Amount{vtxoAmount1})
	require.NoError(t, err, "round 1: should register VTXO requests")

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

	vtxoAmount2 := amount2 - 5000
	err = client.RegisterVTXORequests(ctx, []btcutil.Amount{vtxoAmount2})
	require.NoError(t, err, "round 2: should register VTXO requests")

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

	// Register VTXO requests.
	vtxoAmount := amount - 5000
	err = client.RegisterVTXORequests(ctx, []btcutil.Amount{vtxoAmount})
	require.NoError(t, err, "should register VTXO requests")

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

// TestBoardingE2EConcurrentRounds tests two clients participating in two
// different rounds that are actively driven simultaneously. This demonstrates
// true concurrency where both rounds are in their signing phases at the same
// time, with interleaved message processing.
//
// Timeline:
//
//	Setup:    Client1 boarding confirmed    Client2 boarding confirmed
//	          ─────────────────────────────────────────────────────────
//	Round1:   Client1 joins → Seal → BatchInfo → Signing... → Finalized
//	Round2:                          Client2 joins → Seal → BatchInfo → Signing... → Finalized
//	                                 ─────────────────────────────────────────────────
//	                                 ↑ CONCURRENT WINDOW: Both rounds actively signing ↑
//	          ────────────────────────────────────────────────────────────────────────
//	Mine:                                                          [Block confirms both]
func TestBoardingE2EConcurrentRounds(t *testing.T) {
	ParallelN(t)

	h := NewE2EHarness(t)
	h.Start()

	ctx := context.Background()

	// Fund the server wallet with MULTIPLE UTXOs for concurrent rounds.
	// Each round needs its own UTXO for the commitment transaction, so we
	// fund with separate transactions to create distinct UTXOs.
	h.FundServerWallet(btcutil.SatoshiPerBitcoin * 2)
	h.FundServerWallet(btcutil.SatoshiPerBitcoin * 2)
	t.Log("Funded server wallet with 2 separate UTXOs (2 BTC each)")

	terms := h.Terms()

	// === Phase 1: Setup Both Clients Upfront ===
	// Create two clients and confirm both boarding inputs before any round
	// participation. This ensures we can drive both rounds concurrently.

	client1 := NewTestClient(h)
	t.Logf("Created client1: %s", client1.ClientID())

	client2 := NewTestClient(h)
	t.Logf("Created client2: %s", client2.ClientID())

	// Create boarding addresses for both clients.
	addr1, err := client1.CreateBoardingAddress(terms.BoardingExitDelay)
	require.NoError(t, err)
	t.Logf("Client1 boarding address: %s", addr1.Address.String())

	addr2, err := client2.CreateBoardingAddress(terms.BoardingExitDelay)
	require.NoError(t, err)
	t.Logf("Client2 boarding address: %s", addr2.Address.String())

	// Fund both boarding addresses.
	amount1 := btcutil.Amount(200_000)
	amount2 := btcutil.Amount(300_000)
	txid1 := h.Harness.Faucet(addr1.Address.String(), amount1)
	t.Logf("Client1 funded with %d sats, txid: %s", amount1, txid1)
	txid2 := h.Harness.Faucet(addr2.Address.String(), amount2)
	t.Logf("Client2 funded with %d sats, txid: %s", amount2, txid2)

	// Mine blocks to confirm BOTH boarding inputs.
	h.MineBlocks(int(terms.MinBoardingConfirmations))
	t.Log("Mined blocks to confirm both boarding inputs")

	// Wait for both clients to reach PendingRoundAssembly state.
	err = client1.WaitForBoardingConfirmation(30 * time.Second)
	require.NoError(t, err, "client1: should reach PendingRoundAssembly")
	t.Log("Client1 boarding confirmed")

	err = client2.WaitForBoardingConfirmation(30 * time.Second)
	require.NoError(t, err, "client2: should reach PendingRoundAssembly")
	t.Log("Client2 boarding confirmed")

	// Register VTXO requests for both clients.
	vtxoAmount1 := amount1 - 5000
	err = client1.RegisterVTXORequests(ctx, []btcutil.Amount{vtxoAmount1})
	require.NoError(t, err, "client1: should register VTXO requests")

	vtxoAmount2 := amount2 - 5000
	err = client2.RegisterVTXORequests(ctx, []btcutil.Amount{vtxoAmount2})
	require.NoError(t, err, "client2: should register VTXO requests")
	t.Log("Both clients registered VTXO requests")

	// === Phase 2: Client 1 Joins Round 1 ===
	err = client1.TriggerRegistration(ctx)
	require.NoError(t, err, "client1: should trigger registration")

	// Wait for Client 1 to receive join response.
	err = h.Transcript().WaitForEntryCount(msgsPerClientJoin, 10*time.Second)
	require.NoError(t, err, "client1: should receive join response")
	t.Log("Client1 joined Round 1")

	// === Enable S→C buffering BEFORE sealing ===
	// We buffer server messages so that signing-phase messages (BatchInfo,
	// nonces, sigs) accumulate while we set up both rounds. This ensures
	// both rounds enter signing before either client starts processing.
	h.Bridge().SetBuffered(true)
	t.Log("Enabled S→C message buffering")

	// Seal Round 1 - this creates Round 2 and starts Round 1's batch
	// building. The ClientBatchInfo message for Client 1 is buffered.
	h.TriggerRoundSeal()
	t.Log("Round 1 sealed (Round 2 created, BatchInfo for C1 buffered)")

	// === Phase 3: Client 2 Joins Round 2 ===
	err = client2.TriggerRegistration(ctx)
	require.NoError(t, err, "client2: should trigger registration")

	// Give the server time to process the join and queue ClientSuccessResp.
	time.Sleep(500 * time.Millisecond)
	t.Log("Client2 join request sent (response buffered)")

	// Seal Round 2 - starts Round 2's batch building. Both rounds now have
	// their signing-phase messages queued in the buffer.
	h.TriggerRoundSeal()
	t.Log("Round 2 sealed (BatchInfo for C2 buffered)")

	// Give server time to build batches for both rounds.
	time.Sleep(500 * time.Millisecond)

	// Check buffered message counts.
	buffered1 := h.Bridge().PendingCountFor(client1.ClientID())
	buffered2 := h.Bridge().PendingCountFor(client2.ClientID())
	t.Logf("Buffered messages - Client1: %d, Client2: %d", buffered1, buffered2)

	// === Phase 4: Release All Messages - Both Rounds Sign Concurrently ===
	// Disable buffering and flush. Both clients receive their messages and
	// start their signing phases simultaneously.
	h.Bridge().SetBuffered(false)
	h.Bridge().FlushAllFor(client1.ClientID())
	h.Bridge().FlushAllFor(client2.ClientID())
	t.Log("Flushed all buffered messages - both rounds now signing")

	// Both rounds are now in their signing phases concurrently.
	// Total messages: 2 clients * 9 messages each = 18 messages
	totalExpectedMsgs := msgsPerClientRound * 2
	err = h.Transcript().WaitForEntryCount(totalExpectedMsgs, 60*time.Second)
	require.NoError(t, err, "both rounds should complete signing")

	t.Log("Both rounds completed signing phase")
	t.Log(h.Transcript().Dump())

	// Give server time to broadcast both commitment transactions.
	time.Sleep(1 * time.Second)

	// === Verify: Both commitment transactions in mempool simultaneously ===
	rpcClient, err := h.BitcoinRPCClient()
	require.NoError(t, err, "should get bitcoin RPC client")
	mempoolTxs, err := rpcClient.GetRawMempool()
	require.NoError(t, err, "should get mempool")
	require.Len(t, mempoolTxs, 2, "BOTH commitment txs should be in mempool")
	t.Logf("Round 1 commitment tx: %s", mempoolTxs[0].String())
	t.Logf("Round 2 commitment tx: %s", mempoolTxs[1].String())

	// === Phase 5: Confirm Both Rounds ===
	h.MineBlocksAndConfirm(1)
	t.Log("Mined block to confirm both commitment transactions")

	// Wait for both clients to receive round completion.
	err = client1.WaitForRoundComplete(30 * time.Second)
	require.NoError(t, err, "client1: round should complete")

	err = client2.WaitForRoundComplete(30 * time.Second)
	require.NoError(t, err, "client2: round should complete")

	// === Verify Final State ===
	// Each client should have exactly 1 VTXO from their round.
	client1.AssertVTXOCountFromDB(1)
	client1.AssertConfirmedRoundCountFromDB(1)

	client2.AssertVTXOCountFromDB(1)
	client2.AssertConfirmedRoundCountFromDB(1)

	vtxos1, err := client1.ListVTXOs(ctx)
	require.NoError(t, err)
	require.Len(t, vtxos1, 1, "client1 should have 1 VTXO")
	t.Logf("Client1 VTXO: outpoint=%s, amount=%d sats",
		vtxos1[0].Outpoint, vtxos1[0].Amount)

	vtxos2, err := client2.ListVTXOs(ctx)
	require.NoError(t, err)
	require.Len(t, vtxos2, 1, "client2 should have 1 VTXO")
	t.Logf("Client2 VTXO: outpoint=%s, amount=%d sats",
		vtxos2[0].Outpoint, vtxos2[0].Amount)

	// Verify VTXO properties for both clients.
	client1.AssertVTXOProperties()
	client2.AssertVTXOProperties()

	t.Log("TestBoardingE2EConcurrentRounds completed successfully!")
	t.Log("Verified: Two rounds driven concurrently with interleaved signing")
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

	// Register VTXO requests.
	vtxoAmount := amount - 5000
	err = client.RegisterVTXORequests(ctx, []btcutil.Amount{vtxoAmount})
	require.NoError(t, err, "should register VTXO requests")

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

// TestBatchExpiryNotification verifies that the BatchWatcher correctly sends
// expiry notifications to the BatchSweeper when a batch reaches its expiry
// height. The test uses a smaller sweep delay (150 blocks) for faster
// execution. Note: sweep delay must be > VTXO exit delay (144).
func TestBatchExpiryNotification(t *testing.T) {
	ParallelN(t)

	// Use a smaller sweep delay for this test to avoid mining 1000+ blocks.
	// Must be greater than defaultVTXOExitDelay (144).
	sweepDelay := uint32(150)
	h := NewE2EHarness(t, WithSweepDelay(sweepDelay))
	h.Start()

	ctx := context.Background()

	// Fund the server wallet.
	h.FundServerWallet(btcutil.SatoshiPerBitcoin)
	t.Log("Funded server wallet with 1 BTC")

	// Create a test client.
	client := NewTestClient(h)
	require.NotNil(t, client)
	t.Logf("Created client: %s", client.ClientID())

	// Complete the boarding flow.
	terms := h.Terms()
	require.Equal(t, sweepDelay, terms.SweepDelay,
		"sweep delay should be configured to %d", sweepDelay)

	boardingResp, err := client.CreateBoardingAddress(terms.BoardingExitDelay)
	require.NoError(t, err, "should create boarding address")
	t.Logf("Created boarding address: %s", boardingResp.Address.String())

	// Fund and confirm the boarding input.
	amount := btcutil.Amount(100_000)
	h.Harness.Faucet(boardingResp.Address.String(), amount)
	h.MineBlocks(int(terms.MinBoardingConfirmations))

	err = client.WaitForBoardingConfirmation(30 * time.Second)
	require.NoError(t, err, "FSM should reach PendingRoundAssembly")

	// Register VTXO requests.
	vtxoAmount := amount - 5000
	err = client.RegisterVTXORequests(ctx, []btcutil.Amount{vtxoAmount})
	require.NoError(t, err, "should register VTXO requests")

	// Trigger registration and complete signing.
	err = client.TriggerRegistration(ctx)
	require.NoError(t, err, "should trigger registration")

	err = h.Transcript().WaitForEntryCount(msgsPerClientJoin, 10*time.Second)
	require.NoError(t, err, "server should respond")

	h.TriggerRoundSeal()
	err = h.Transcript().WaitForEntryCount(msgsPerClientRound, 30*time.Second)
	require.NoError(t, err, "should complete signing")

	// Wait for broadcast.
	time.Sleep(1 * time.Second)

	// Get block height before mining the confirmation block.
	rpcClient, err := h.BitcoinRPCClient()
	require.NoError(t, err, "should get bitcoin RPC client")
	blockCountBeforeConfirm, err := rpcClient.GetBlockCount()
	require.NoError(t, err, "should get block count")

	// Mine to confirm the commitment transaction.
	h.MineBlocksAndConfirm(1)
	t.Log("Mined block to confirm commitment transaction")

	// Wait for round completion.
	err = client.WaitForRoundComplete(30 * time.Second)
	require.NoError(t, err, "round should complete successfully")

	// Get the round ID and compute the batch ID.
	roundID, err := client.GetLastCompletedRoundID()
	require.NoError(t, err, "should get last completed round ID")
	batchID := ComputeBatchID(uuid.UUID(roundID), 0)
	t.Logf("Batch ID: %s", batchID)

	// Calculate the expiry height.
	confirmationHeight := uint32(blockCountBeforeConfirm) + 1
	expiryHeight := confirmationHeight + sweepDelay
	t.Logf("Confirmation height: %d, Expiry height: %d",
		confirmationHeight, expiryHeight)

	// Verify batch is registered but not yet expired.
	h.AssertBatchRegistered(uuid.UUID(roundID), confirmationHeight, 1)
	require.False(t, h.MockBatchSweeper().HasExpiryNotification(batchID),
		"batch should not be expired yet")
	t.Log("Verified batch registered, not yet expired")

	// Get current block height.
	currentHeight, err := rpcClient.GetBlockCount()
	require.NoError(t, err, "should get current block count")

	// Mine blocks until we reach the expiry height. The BatchWatcher checks
	// for expiry when it receives NewBlockReceived messages.
	blocksToMine := int(expiryHeight) - int(currentHeight)
	require.Greater(t, blocksToMine, 0, "should have blocks to mine")
	t.Logf("Mining %d blocks to reach expiry height %d", blocksToMine, expiryHeight)

	h.MineBlocks(blocksToMine)

	// Give the BatchWatcher time to process the block and send the expiry
	// notification.
	time.Sleep(500 * time.Millisecond)

	// Verify the BatchSweeper received the expiry notification.
	require.True(t, h.MockBatchSweeper().HasExpiryNotification(batchID),
		"batch should have received expiry notification")

	notification := h.MockBatchSweeper().GetExpiryNotification(batchID)
	require.NotNil(t, notification, "expiry notification should not be nil")
	require.Equal(t, expiryHeight, notification.ExpiryHeight,
		"expiry notification height should match expected")

	t.Logf("Verified batch %s expired at height %d",
		batchID, notification.ExpiryHeight)

	t.Log("TestBatchExpiryNotification completed successfully!")
}
