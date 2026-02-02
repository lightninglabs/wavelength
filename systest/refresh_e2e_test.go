//go:build systest

package systest

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/stretchr/testify/require"
)

// TestVTXORefreshE2E tests the complete VTXO refresh flow where:
//  1. Client boards funds and creates a VTXO in round 1
//  2. Client triggers refresh of that VTXO
//  3. Old VTXO is forfeited, new VTXO is created in round 2
//  4. All database persistence reflects this correctly
//
// The refresh flow involves:
//   - Client requests refresh via wallet actor
//   - Round actor coordinates the forfeit signing
//   - Server builds new round with connector for forfeit tx
//   - Client signs forfeit tx linking old VTXO to new round
//   - New round confirms, old VTXO marked forfeited
func TestVTXORefreshE2E(t *testing.T) {
	ParallelN(t)

	h := NewE2EHarness(t)
	h.Start()

	ctx := context.Background()

	// Fund the server wallet with enough for multiple rounds.
	h.FundServerWallet(btcutil.SatoshiPerBitcoin * 3) // 3 BTC
	t.Log("Funded server wallet with 3 BTC")

	client := NewTestClient(h)
	terms := h.Terms()
	t.Logf("Created client: %s", client.ClientID())

	// === PHASE 1: Board funds and create VTXO in Round 1 ===
	t.Log("=== Phase 1: Initial boarding and VTXO creation ===")

	// Create a boarding address.
	boardingResp, err := client.CreateBoardingAddress(terms.BoardingExitDelay)
	require.NoError(t, err, "should create boarding address")
	t.Logf("Created boarding address: %s", boardingResp.Address.String())

	// Fund the boarding address.
	boardingAmount := btcutil.Amount(200_000)
	txidStr := h.Harness.Faucet(boardingResp.Address.String(), boardingAmount)
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
	err = h.Transcript().WaitForEntryCount(msgsPerClientJoin, 10*time.Second)
	require.NoError(t, err, "server should respond")

	// Seal round 1.
	h.TriggerRoundSeal()
	t.Log("Round 1 sealed")

	// Wait for signing completion.
	err = h.Transcript().WaitForEntryCount(msgsPerClientRound, 30*time.Second)
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
	t.Logf("Round 1 VTXO: outpoint=%s, amount=%d sats",
		vtxo1Outpoint, vtxosAfterRound1[0].Amount)

	// Verify initial VTXO status is Live.
	vtxo1Desc, err := client.GetVTXODescriptor(vtxo1Outpoint)
	require.NoError(t, err)
	require.Equal(t, vtxo.VTXOStatusLive, vtxo1Desc.Status,
		"VTXO should be in Live status after round 1")
	t.Log("VTXO status verified: Live")

	// Clear transcript for round 2.
	h.Transcript().Clear()

	// === PHASE 2: Trigger VTXO Refresh ===
	t.Log("=== Phase 2: Trigger VTXO refresh ===")

	// Trigger refresh of the VTXO.
	err = client.TriggerVTXORefresh(ctx, []wire.OutPoint{vtxo1Outpoint})
	require.NoError(t, err, "should trigger VTXO refresh")
	t.Logf("Triggered refresh for VTXO %s", vtxo1Outpoint)

	// Wait for VTXO status to transition to RefreshRequested.
	err = client.WaitForVTXOStatus(
		vtxo1Outpoint, vtxo.VTXOStatusRefreshRequested, 10*time.Second,
	)
	require.NoError(t, err, "VTXO should reach RefreshRequested status")
	t.Log("VTXO status: RefreshRequested")

	// Now trigger registration to send the JoinRoundRequest to the server.
	// The round FSM has accumulated the refresh request in PendingRoundAssembly,
	// but needs RegistrationRequested to actually send the join message.
	err = client.TriggerRegistration(ctx)
	require.NoError(t, err, "should trigger registration for refresh")
	t.Log("Triggered registration for refresh round")

	// Wait for server response.
	err = h.Transcript().WaitForEntryCount(msgsPerClientJoin, 10*time.Second)
	require.NoError(t, err, "server should respond to refresh registration")

	// Seal round 2.
	h.TriggerRoundSeal()
	t.Log("Round 2 sealed")

	// Wait for VTXO status to transition to Forfeiting. This happens when
	// the client receives the batch info and starts the forfeit signing flow.
	err = client.WaitForVTXOStatus(
		vtxo1Outpoint, vtxo.VTXOStatusForfeiting, 15*time.Second,
	)
	require.NoError(t, err, "VTXO should reach Forfeiting status")
	t.Log("VTXO status: Forfeiting")

	// Wait for signing completion. For refresh rounds, the message count
	// includes forfeit signature submission.
	err = h.Transcript().WaitForEntryCount(msgsPerClientRound, 30*time.Second)
	require.NoError(t, err, "round 2: should complete signing")

	t.Log("Round 2 signing completed")
	t.Log(h.Transcript().Dump())

	// Wait for broadcast and mine to confirm.
	time.Sleep(1 * time.Second)
	h.MineBlocksAndConfirm(1)
	t.Log("Mined block to confirm round 2 commitment transaction")

	// Wait for round 2 completion.
	err = client.WaitForRoundComplete(30 * time.Second)
	require.NoError(t, err, "round 2: should complete")

	// === PHASE 3: Verify Final State ===
	t.Log("=== Phase 3: Verify final state ===")

	// Verify we have 2 confirmed rounds.
	client.AssertConfirmedRoundCountFromDB(2)
	t.Log("Verified: 2 confirmed rounds in database")

	// Get round 2 ID.
	round2ID, err := client.GetLastCompletedRoundID()
	require.NoError(t, err)
	require.NotEqual(t, round1ID, round2ID, "round IDs should be different")
	t.Logf("Round 2 completed: %s", round2ID)

	// Verify old VTXO is now Forfeited.
	client.AssertVTXOStatus(vtxo1Outpoint, vtxo.VTXOStatusForfeited)
	t.Logf("Verified: Old VTXO %s is Forfeited", vtxo1Outpoint)

	// Find the new VTXO from round 2.
	vtxo2Desc, err := client.GetVTXOByRoundID(round2ID.String())
	require.NoError(t, err, "should find VTXO from round 2")
	require.NotEqual(t, vtxo1Outpoint, vtxo2Desc.Outpoint,
		"new VTXO should have different outpoint")
	t.Logf("Round 2 VTXO: outpoint=%s, amount=%d sats",
		vtxo2Desc.Outpoint, vtxo2Desc.Amount)

	// Verify new VTXO is Live.
	require.Equal(t, vtxo.VTXOStatusLive, vtxo2Desc.Status,
		"new VTXO should be Live")
	t.Log("Verified: New VTXO is Live")

	// Verify replacement relationship (this also checks amounts are similar).
	client.AssertVTXOReplacement(vtxo1Outpoint, vtxo2Desc.Outpoint)
	t.Log("Verified: VTXO replacement relationship and value preservation")

	// Verify we have exactly 1 live VTXO (the new one).
	liveVTXOs, err := client.ListLiveVTXODescriptors()
	require.NoError(t, err)
	require.Len(t, liveVTXOs, 1, "should have exactly 1 live VTXO")
	require.Equal(t, vtxo2Desc.Outpoint, liveVTXOs[0].Outpoint,
		"live VTXO should be the new one")
	t.Log("Verified: Exactly 1 live VTXO (the refreshed one)")

	t.Log("TestVTXORefreshE2E completed successfully!")
	t.Log("Demonstrated: VTXO refresh lifecycle - Live -> RefreshRequested -> " +
		"Forfeiting -> Forfeited (old) + Live (new)")
}
