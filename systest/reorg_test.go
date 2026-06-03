//go:build systest

package systest

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/stretchr/testify/require"
)

// TestReorgStressActionConvergesAndWalletRemainsUsable verifies that the
// systest reorg action can be injected into a live actor stack and that
// boarding wallet chain notifications continue to work afterward.
func TestReorgStressActionConvergesAndWalletRemainsUsable(t *testing.T) {
	ParallelN(t)

	f := NewBoardingWalletFixture(t)
	h := f.Harness

	oldTip := h.Harness.BestBlockHeader()
	result := h.ApplyReorgAction(ReorgAction{Depth: 2})
	newTip := h.Harness.BestBlockHeader()

	require.Equal(t, oldTip.Hash, result.OldTip.Hash)
	require.Equal(t, oldTip.Height-2, result.ForkPoint.Height)
	require.Len(t, result.Disconnected, 2)
	require.Len(t, result.Connected, 3)
	require.Equal(
		t, result.Connected[len(result.Connected)-1].Hash, newTip.Hash,
	)
	require.Equal(t, oldTip.Height+1, newTip.Height)
	require.NotEqual(t, oldTip.Hash, newTip.Hash)

	addrResp := f.CreateBoardingAddress(144)
	amount := btcutil.Amount(btcutil.SatoshiPerBitcoin / 10)
	f.FundAddress(addrResp.Address.String(), amount)
	f.WaitForBalance(30 * time.Second)

	addresses := f.GetActiveAddresses()
	storedAddr := addresses[addrResp.Address.String()]
	require.NotNil(t, storedAddr)

	f.AssertIntentStored(storedAddr, amount)

	balance := f.GetBalance()
	require.Equal(t, 1, balance.UtxoCount)
	require.InDelta(
		t, int64(amount), int64(balance.TotalBalance), 10000,
	)
}
