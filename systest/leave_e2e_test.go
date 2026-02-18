//go:build systest

package systest

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/stretchr/testify/require"
)

// TestVTXOLeaveE2E tests the complete VTXO leave (offboard) flow where:
//  1. Client boards funds and creates a VTXO in round 1
//  2. Client triggers leave of that VTXO to an on-chain address
//  3. Old VTXO is forfeited, leave output created in batch tx
//  4. After confirmation, client's on-chain wallet has the funds
//
// The leave flow involves:
//   - Client requests leave via round actor (LeaveVTXORequest)
//   - Round actor coordinates the forfeit signing (same as refresh)
//   - Server builds new round with connector for forfeit tx
//   - Server includes leave output in batch transaction
//   - Client signs forfeit tx linking old VTXO to new round
//   - New round confirms, old VTXO marked forfeited
//   - Leave output spendable by client's on-chain wallet
func TestVTXOLeaveE2E(t *testing.T) {
	ParallelN(t)

	h := NewE2EHarness(t)
	h.Start()

	ctx := t.Context()

	// Fund the server wallet with enough for multiple rounds.
	h.FundServerWallet(btcutil.SatoshiPerBitcoin * 3)
	t.Log("Funded server wallet with 3 BTC")

	client := NewTestClient(h)
	terms := h.Terms()
	t.Logf("Created client: %s", client.ClientID())

	// === PHASE 1: Board funds and create VTXO in Round 1 ===
	t.Log("=== Phase 1: Initial boarding and VTXO creation ===")

	// Create a boarding address.
	boardingResp, err := client.CreateBoardingAddress(
		terms.BoardingExitDelay,
	)
	require.NoError(t, err, "should create boarding address")
	t.Logf("Created boarding address: %s", boardingResp.Address.String())

	// Fund the boarding address.
	boardingAmount := btcutil.Amount(200_000)
	txidStr := h.Harness.Faucet(
		boardingResp.Address.String(), boardingAmount,
	)
	t.Logf("Funded boarding address with %d sats, txid: %s",
		boardingAmount, txidStr)

	// Mine blocks to confirm the funding.
	h.MineBlocks(int(terms.MinBoardingConfirmations))
	t.Logf("Mined %d blocks to confirm funding",
		terms.MinBoardingConfirmations)

	// Wait for boarding confirmation.
	err = client.WaitForBoardingConfirmation(30 * time.Second)
	require.NoError(t, err, "FSM should reach PendingRoundAssembly")
	t.Log("Client FSM reached PendingRoundAssembly state")

	// Register VTXO requests.
	vtxoAmount := boardingAmount - 5000
	err = client.RegisterVTXORequests(ctx, []btcutil.Amount{vtxoAmount})
	require.NoError(t, err, "should register VTXO requests")
	t.Logf("Registered VTXO request for %d sats", vtxoAmount)

	// Trigger registration.
	err = client.TriggerRegistration(ctx)
	require.NoError(t, err, "should trigger registration")

	// Wait for server response.
	err = h.Transcript().WaitForEntryCount(
		msgsPerClientJoin, 10*time.Second,
	)
	require.NoError(t, err, "server should respond")

	// Seal round 1.
	h.TriggerRoundSeal()
	t.Log("Round 1 sealed")

	// Wait for signing completion.
	err = h.Transcript().WaitForEntryCount(
		msgsPerClientRound, 30*time.Second,
	)
	require.NoError(t, err, "round 1: should complete signing")

	// Wait for broadcast and mine to confirm.
	time.Sleep(1 * time.Second)
	h.MineBlocksAndConfirm(1)
	t.Log("Mined block to confirm round 1 commitment transaction")

	// Wait for round completion.
	err = client.WaitForRoundComplete(30 * time.Second)
	require.NoError(t, err, "round 1: should complete")

	// Verify round 1 state.
	client.AssertVTXOCountFromDB(1)
	client.AssertConfirmedRoundCountFromDB(1)

	round1ID, err := client.GetLastCompletedRoundID()
	require.NoError(t, err)
	t.Logf("Round 1 completed: %s", round1ID)

	// Get the VTXO from round 1.
	vtxosAfterRound1, err := client.ListVTXOs(ctx)
	require.NoError(t, err)
	require.Len(t, vtxosAfterRound1, 1)
	vtxo1Outpoint := vtxosAfterRound1[0].Outpoint
	vtxo1Amount := vtxosAfterRound1[0].Amount
	t.Logf("Round 1 VTXO: outpoint=%s, amount=%d sats",
		vtxo1Outpoint, vtxo1Amount)

	// Verify initial VTXO status is Live.
	vtxo1Desc, err := client.GetVTXODescriptor(vtxo1Outpoint)
	require.NoError(t, err)
	require.Equal(t, vtxo.VTXOStatusLive, vtxo1Desc.Status,
		"VTXO should be in Live status after round 1")
	t.Log("VTXO status verified: Live")

	// Record initial on-chain balance.
	initialBalance, err := client.GetOnChainBalance(ctx)
	require.NoError(t, err)
	t.Logf("Initial on-chain balance: %d sats", initialBalance)

	// Clear transcript for round 2.
	h.Transcript().Clear()

	// === PHASE 2: Trigger VTXO Leave ===
	t.Log("=== Phase 2: Trigger VTXO leave ===")

	// Get destination address from client's LND wallet.
	destAddr, err := client.GetNewAddress(ctx)
	require.NoError(t, err)
	t.Logf("Leave destination: %s", destAddr.String())

	// Trigger leave of the VTXO.
	err = client.TriggerVTXOLeave(
		ctx, []wire.OutPoint{vtxo1Outpoint}, destAddr,
	)
	require.NoError(t, err, "should trigger VTXO leave")
	t.Logf("Triggered leave for VTXO %s", vtxo1Outpoint)

	// Wait for the leave request to be processed and round FSM to be
	// created. The async message path is: wallet -> round -> vtxo -> round.
	time.Sleep(500 * time.Millisecond)

	// Trigger registration to send the leave request to the server.
	err = client.TriggerRegistration(ctx)
	require.NoError(t, err, "should trigger registration for leave")
	t.Log("Triggered registration for leave round")

	// Wait for server response to leave request (join round).
	err = h.Transcript().WaitForEntryCount(
		msgsPerClientJoin, 10*time.Second,
	)
	require.NoError(t, err, "server should respond to leave registration")
	t.Log("Server responded to leave request")

	// Seal round 2 to trigger forfeit request back to VTXO.
	h.TriggerRoundSeal()
	t.Log("Round 2 sealed")

	// Now wait for VTXO status to transition to Forfeiting. The VTXO only
	// reaches Forfeiting after receiving ForfeitRequestEvent from the round
	// actor, which is sent after the round is sealed.
	err = client.WaitForVTXOStatus(
		vtxo1Outpoint, vtxo.VTXOStatusForfeiting, 15*time.Second,
	)
	require.NoError(t, err, "VTXO should reach Forfeiting status")
	t.Log("VTXO status: Forfeiting")

	// Wait for signing completion. Leave-only rounds have fewer messages
	// since they skip nonce/partial sig exchange (no VTXO trees to sign).
	// Messages: JoinRound, CommitmentTx, InputSigs (forfeit).
	msgsPerLeaveRound := 5
	err = h.Transcript().WaitForEntryCount(
		msgsPerLeaveRound, 30*time.Second,
	)
	require.NoError(t, err, "round 2: should complete signing")

	t.Log("Round 2 signing completed")

	// Wait for broadcast and mine to confirm.
	time.Sleep(1 * time.Second)
	h.MineBlocksAndConfirm(1)
	t.Log("Mined block to confirm round 2 commitment transaction")

	// === PHASE 3: Verify Final State ===
	t.Log("=== Phase 3: Verify final state ===")

	// Verify old VTXO is now Forfeited.
	err = client.WaitForVTXOStatus(
		vtxo1Outpoint, vtxo.VTXOStatusForfeited, 10*time.Second,
	)
	require.NoError(t, err, "VTXO should reach Forfeited status")
	t.Logf("Verified: Old VTXO %s is Forfeited", vtxo1Outpoint)

	// Verify no live VTXOs remain (leave consumes the VTXO without creating
	// a new one, unlike refresh).
	liveVTXOs, err := client.ListLiveVTXODescriptors()
	require.NoError(t, err)
	require.Len(t, liveVTXOs, 0, "should have no live VTXOs after leave")
	t.Log("Verified: No live VTXOs (all funds exited to on-chain)")

	// Mine a few more blocks to ensure the leave output is mature.
	h.MineBlocks(6)

	// Verify on-chain balance increased by approximately the VTXO amount.
	// The exact amount will be slightly less due to fees.
	expectedMinBalance := initialBalance + btcutil.Amount(
		float64(vtxo1Amount)*0.9,
	)
	err = client.WaitForOnChainBalance(
		ctx, expectedMinBalance, 30*time.Second,
	)
	require.NoError(t, err, "on-chain balance should increase")

	finalBalance, err := client.GetOnChainBalance(ctx)
	require.NoError(t, err)
	t.Logf("Final on-chain balance: %d sats (increase: %d sats)",
		finalBalance, finalBalance-initialBalance)

	// Verify the balance increase is close to the VTXO amount (minus fees).
	balanceIncrease := finalBalance - initialBalance
	require.InDelta(t, float64(vtxo1Amount), float64(balanceIncrease),
		float64(50_000), // Allow up to 50k sats for fees.
		"balance increase should be close to VTXO amount")

	t.Log("TestVTXOLeaveE2E completed successfully!")
	t.Log("Demonstrated: VTXO leave lifecycle - Live -> Forfeiting -> " +
		"Forfeited + on-chain balance credited")
}
