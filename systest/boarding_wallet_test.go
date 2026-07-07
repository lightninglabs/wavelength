//go:build systest

package systest

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/wallet"
	"github.com/stretchr/testify/require"
)

// TestBoardingWalletIntentPersistence tests that when a boarding address
// receives funds, the BoardingIntent is persisted with full fidelity. This
// verifies address creation, funding detection, and complete data integrity.
func TestBoardingWalletIntentPersistence(t *testing.T) {
	ParallelN(t)

	f := NewBoardingWalletFixture(t)

	// Create address and verify it's stored correctly.
	addrResp := f.CreateBoardingAddress(144)
	addresses := f.GetActiveAddresses()
	require.Len(t, addresses, 1)

	storedAddr := addresses[addrResp.Address.String()]
	require.NotNil(t, storedAddr)
	f.AssertAddressStored(storedAddr)

	// Fund it and wait for detection.
	fundAmount := btcutil.Amount(btcutil.SatoshiPerBitcoin / 4)
	f.FundAddress(addrResp.Address.String(), fundAmount)
	f.WaitForBalance(30 * time.Second)

	// Verify intent is stored with full fidelity.
	intent := f.AssertIntentStored(storedAddr, fundAmount)

	// The persisted row must carry a populated TxProof so a subsequent
	// daemon restart can replay it to the round actor without rebuilding
	// from chain. Without migration-000010 this assertion would fail —
	// the column wouldn't exist and the load path would always return
	// None.
	require.True(
		t, intent.ChainInfo.TxProof.IsSome(),
		"persisted intent must carry TxProof for restart safety",
	)
	persistedProof := intent.ChainInfo.TxProof.UnsafeFromSome()
	require.Equal(
		t, uint32(intent.ChainInfo.ConfHeight),
		persistedProof.BlockHeight,
	)
	require.Equal(t, intent.Outpoint, persistedProof.ClaimedOutPoint)

	t.Logf(
		"Intent stored: outpoint=%s, height=%d, amount=%d, txproof=%v",
		intent.Outpoint.String(), intent.ChainInfo.ConfHeight,
		intent.ChainInfo.Amount, intent.ChainInfo.TxProof.IsSome(),
	)
}

// TestBoardingWalletBacklogNotifications tests both backlog delivery (for
// historical UTXOs) and real-time notifications (for new UTXOs). This
// verifies the BacklogHeight parameter works correctly.
func TestBoardingWalletBacklogNotifications(t *testing.T) {
	ParallelN(t)

	f := NewBoardingWalletFixture(t)
	ctx := f.Context()

	// First, create and fund address BEFORE registering any notifier.
	addr1 := f.CreateBoardingAddress(144)
	amount1 := btcutil.Amount(btcutil.SatoshiPerBitcoin / 2)
	f.FundAddress(addr1.Address.String(), amount1)
	f.WaitForBalance(30 * time.Second)

	// With the address created, verify that it actually exists.
	intents, err := f.Store().FetchBoardingIntentsByStatus(
		ctx, wallet.BoardingStatusConfirmed,
	)
	require.NoError(t, err)
	require.Len(t, intents, 1)
	backlogHeight := intents[0].ChainInfo.ConfHeight

	// Next, register with BacklogHeight to receive historical event.
	// This is the standalone-restart simulation: when the daemon
	// restarts, the round actor re-registers as a confirmation notifier
	// and asks the wallet for backlog from the height it last observed.
	// The backlog event MUST carry a populated TxProof; otherwise the
	// server rejects the join request with "TxProof is required when
	// server has no chain source".
	notifyCh := f.RegisterNotifierWithBacklog("backlog-test", backlogHeight)

	// We should receive backlog notification for already-confirmed UTXO.
	select {
	case event := <-notifyCh:
		t.Logf(
			"Backlog notification: height=%d, addr=%s, txproof=%v",
			event.ChainInfo.ConfHeight,
			event.Address.Address.String(),
			event.ChainInfo.TxProof.IsSome(),
		)

		require.Equal(
			t, addr1.Address.String(),
			event.Address.Address.String(),
		)
		require.Equal(t, backlogHeight, event.ChainInfo.ConfHeight)
		require.True(
			t, event.ChainInfo.TxProof.IsSome(),
			"backlog event must carry TxProof for post-restart "+
				"join-round registration",
		)
		backlogProof := event.ChainInfo.TxProof.UnsafeFromSome()
		require.Equal(
			t, uint32(backlogHeight), backlogProof.BlockHeight,
		)
		require.Equal(
			t, event.Outpoint, backlogProof.ClaimedOutPoint,
		)

	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for backlog notification")
	}

	// Now we'll create second address and fund it, this should result in
	// us getting yet another notification.
	addr2 := f.CreateBoardingAddress(288)
	amount2 := btcutil.Amount(btcutil.SatoshiPerBitcoin / 4)
	f.FundAddress(addr2.Address.String(), amount2)

	// Should receive real-time notification for new UTXO.
	select {
	case event := <-notifyCh:
		hasProof := event.ChainInfo.TxProof.IsSome()
		t.Logf(
			"Real-time notification: height=%d addr=%s txproof=%v",
			event.ChainInfo.ConfHeight,
			event.Address.Address.String(), hasProof,
		)

		require.Equal(
			t, addr2.Address.String(),
			event.Address.Address.String(),
		)
		require.True(
			t, event.ChainInfo.TxProof.IsSome(),
			"live event must carry TxProof",
		)

	case <-time.After(30 * time.Second):
		t.Fatal("timeout waiting for real-time notification")
	}

	// Verify both intents exist with correct details.
	balance := f.GetBalance()
	require.Equal(t, 2, balance.UtxoCount)

	f.UnregisterNotifier("backlog-test")
}

// TestBoardingWalletAddressReuse tests that multiple credits to the SAME
// boarding address are correctly tracked as separate intents.
func TestBoardingWalletAddressReuse(t *testing.T) {
	ParallelN(t)

	f := NewBoardingWalletFixture(t)
	ctx := f.Context()

	// Register notifier to capture all events.
	notifyCh := f.RegisterNotifier("reuse-notifier")

	// Now, create a single boarding address.
	addrResp := f.CreateBoardingAddress(144)
	addr := addrResp.Address.String()
	addresses := f.GetActiveAddresses()
	storedAddr := addresses[addr]
	f.AssertAddressStored(storedAddr)

	// Send some funds to it.
	amount1 := btcutil.Amount(btcutil.SatoshiPerBitcoin / 4)
	f.FundAddress(addr, amount1)

	var firstOutpoint wire.OutPoint
	select {
	case event := <-notifyCh:
		t.Logf(
			"First UTXO: %s, amount=%d", event.Outpoint.String(),
			event.ChainInfo.Amount,
		)

		require.Equal(t, addr, event.Address.Address.String())
		firstOutpoint = event.Outpoint

	case <-time.After(30 * time.Second):
		t.Fatal("timeout waiting for first notification")
	}

	// Send second credit to SAME address.
	amount2 := btcutil.Amount(btcutil.SatoshiPerBitcoin / 2)
	f.FundAddress(addr, amount2)

	select {
	case event := <-notifyCh:
		t.Logf(
			"Second UTXO: %s, amount=%d", event.Outpoint.String(),
			event.ChainInfo.Amount,
		)
		require.Equal(t, addr, event.Address.Address.String())
		require.NotEqual(
			t, firstOutpoint, event.Outpoint,
			"should be different outpoint",
		)

	case <-time.After(30 * time.Second):
		t.Fatal("timeout waiting for second notification")
	}

	// Verify two separate intents exist for the same address.
	intents, err := f.Store().FetchBoardingIntentsByStatus(
		ctx, wallet.BoardingStatusConfirmed,
	)
	require.NoError(t, err)
	require.Len(t, intents, 2, "should have 2 intents for reused address")

	// Both reference the same address but have different outpoints.
	require.Equal(
		t, intents[0].Address.Address.String(),
		intents[1].Address.Address.String(),
	)
	require.NotEqual(t, intents[0].Outpoint, intents[1].Outpoint)

	// Balance should be sum of both.
	balance := f.GetBalance()
	require.InDelta(
		t, int64(amount1+amount2), int64(balance.TotalBalance), 20000,
	)
	require.Equal(t, 2, balance.UtxoCount)

	f.UnregisterNotifier("reuse-notifier")
}

// TestBoardingWalletMultipleAddresses tests creating and funding multiple
// addresses with different exit delays, verifying each is stored correctly.
func TestBoardingWalletMultipleAddresses(t *testing.T) {
	ParallelN(t)

	f := NewBoardingWalletFixture(t)
	ctx := f.Context()

	// Create 3 addresses with different exit delays.
	addr1 := f.CreateBoardingAddress(144)
	addr2 := f.CreateBoardingAddress(288)
	addr3 := f.CreateBoardingAddress(432)

	// Verify all addresses are stored with correct details.
	addresses := f.GetActiveAddresses()
	require.Len(t, addresses, 3)
	for _, addr := range addresses {
		f.AssertAddressStored(addr)
	}

	// Fund each address with different amounts.
	amounts := []btcutil.Amount{
		btcutil.Amount(btcutil.SatoshiPerBitcoin / 4),
		btcutil.Amount(btcutil.SatoshiPerBitcoin / 2),
		btcutil.Amount(btcutil.SatoshiPerBitcoin / 8),
	}

	f.FundAddress(addr1.Address.String(), amounts[0])
	f.FundAddress(addr2.Address.String(), amounts[1])
	f.FundAddress(addr3.Address.String(), amounts[2])

	// Wait for all UTXOs to be detected.
	require.Eventually(t, func() bool {
		return f.GetBalance().UtxoCount == 3
	}, 60*time.Second, 500*time.Millisecond)

	// Verify all intents are persisted with correct exit delays.
	intents, err := f.Store().FetchBoardingIntentsByStatus(
		ctx, wallet.BoardingStatusConfirmed,
	)
	require.NoError(t, err)
	require.Len(t, intents, 3)

	exitDelays := make(map[string]uint32)
	for _, intent := range intents {
		addrStr := intent.Address.Address.String()
		exitDelays[addrStr] = intent.Address.ExitDelay
	}

	require.Equal(t, uint32(144), exitDelays[addr1.Address.String()])
	require.Equal(t, uint32(288), exitDelays[addr2.Address.String()])
	require.Equal(t, uint32(432), exitDelays[addr3.Address.String()])

	// Verify total balance.
	balance := f.GetBalance()
	expectedTotal := amounts[0] + amounts[1] + amounts[2]
	require.InDelta(
		t, int64(expectedTotal), int64(balance.TotalBalance), 30000,
	)
}
