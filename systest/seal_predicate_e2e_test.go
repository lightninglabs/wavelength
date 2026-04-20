//go:build systest

package systest

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/darepo/rounds"
	"github.com/stretchr/testify/require"
)

// TestSealPredicateMaxClients verifies that a MaxClients seal predicate
// seals the round automatically when enough clients join, without the
// registration timeout firing. The test uses a 10-minute registration
// timeout to prove the predicate sealed the round, not the timer.
//
// Flow:
//  1. Create harness with MaxClients(2) and a very long registration
//     timeout.
//  2. Two clients create boarding addresses, fund them, and join.
//  3. After the second client joins, the predicate fires and the round
//     seals immediately.
//  4. The round completes (signing, broadcast, confirmation) without
//     any explicit timeout trigger.
func TestSealPredicateMaxClients(t *testing.T) {
	ParallelN(t)

	h := NewE2EHarness(t,
		WithShouldSeal(rounds.MaxClients(2)),
		WithRegistrationTimeout(10*time.Minute),
	)
	h.Start()

	ctx := t.Context()

	// Fund the server wallet.
	h.FundServerWallet(btcutil.SatoshiPerBitcoin)
	t.Log("Funded server wallet with 1 BTC")

	terms := h.Terms()

	// Create two clients, each with their own boarding address.
	client1 := NewTestClient(h)
	client2 := NewTestClient(h)
	t.Logf("Created clients: %s, %s",
		client1.ClientID(), client2.ClientID())

	boardingResp1, err := client1.CreateBoardingAddress(
		terms.BoardingExitDelay,
	)
	require.NoError(t, err)

	boardingResp2, err := client2.CreateBoardingAddress(
		terms.BoardingExitDelay,
	)
	require.NoError(t, err)

	// Fund both boarding addresses.
	amount := btcutil.Amount(100_000)
	h.Harness.Faucet(boardingResp1.Address.String(), amount)
	h.Harness.Faucet(boardingResp2.Address.String(), amount)

	// Mine to confirm funding.
	h.MineBlocks(int(terms.MinBoardingConfirmations))

	// Wait for both clients to detect boarding confirmation.
	err = client1.WaitForBoardingConfirmation(30 * time.Second)
	require.NoError(t, err, "client1 should confirm boarding")

	err = client2.WaitForBoardingConfirmation(30 * time.Second)
	require.NoError(t, err, "client2 should confirm boarding")

	t.Log("Both clients reached PendingRoundAssembly")

	// Register VTXO requests for both clients.
	vtxoAmount := amount - 5000
	err = client1.RegisterVTXORequests(
		ctx, []btcutil.Amount{vtxoAmount},
	)
	require.NoError(t, err)

	err = client2.RegisterVTXORequests(
		ctx, []btcutil.Amount{vtxoAmount},
	)
	require.NoError(t, err)

	// Both clients trigger registration.
	err = client1.TriggerRegistration(ctx)
	require.NoError(t, err)

	err = client2.TriggerRegistration(ctx)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		entries := h.Transcript().Entries()

		var (
			client1Joined bool
			client2Joined bool
			client1Acked  bool
			client2Acked  bool
		)
		for _, entry := range entries {
			switch {
			case entry.Direction == ClientToServer &&
				entry.ClientID == client1.ClientID() &&
				entry.MsgType == "JoinRoundRequest":

				client1Joined = true

			case entry.Direction == ClientToServer &&
				entry.ClientID == client2.ClientID() &&
				entry.MsgType == "JoinRoundRequest":

				client2Joined = true

			case entry.Direction == ServerToClient &&
				entry.ClientID == client1.ClientID() &&
				entry.MsgType == "ClientSuccessResp":

				client1Acked = true

			case entry.Direction == ServerToClient &&
				entry.ClientID == client2.ClientID() &&
				entry.MsgType == "ClientSuccessResp":

				client2Acked = true
			}
		}

		return client1Joined && client2Joined &&
			client1Acked && client2Acked
	}, 10*time.Second, 50*time.Millisecond,
		"both clients should complete the join handshake\n%s",
		h.Transcript().Dump(),
	)

	t.Log("Transcript after both clients joined:")
	t.Log(h.Transcript().Dump())

	// DO NOT call h.TriggerRoundSeal(). The MaxClients(2) predicate
	// should have already sealed the round after the second join.

	// Wait for the full signing exchange to complete. Each client
	// exchanges 9 messages with the server during a round with VTXOs.
	totalMessages := 2 * msgsPerClientRound
	err = h.Transcript().WaitForEntryCount(
		totalMessages, 60*time.Second,
	)
	require.NoError(
		t, err,
		"round should complete without explicit seal trigger",
	)

	t.Log("Transcript after signing (sealed by predicate):")
	t.Log(h.Transcript().Dump())

	// Wait for broadcast and mine to confirm.
	time.Sleep(1 * time.Second)
	h.MineBlocksAndConfirm(1)
	t.Log("Mined block to confirm commitment transaction")

	// Both clients should see round completion.
	err = client1.WaitForRoundComplete(30 * time.Second)
	require.NoError(t, err, "client1 round should complete")

	err = client2.WaitForRoundComplete(30 * time.Second)
	require.NoError(t, err, "client2 round should complete")

	t.Log("Both clients completed round (sealed by predicate)")
}
