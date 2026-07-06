package vtxo

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/stretchr/testify/require"
)

// TestSumSpendableBalance verifies that SumSpendableBalance counts only
// Live VTXOs, excluding the other non-terminal states that ListLiveVTXOs
// returns for actor recovery (PendingForfeit, Forfeiting, Spending). This
// is the regression behind darepo-client #584, where a stuck Spending
// VTXO and a PendingForfeit VTXO were summed into vtxo_balance_sat and
// made the spendable balance look non-zero when it was actually zero.
func TestSumSpendableBalance(t *testing.T) {
	t.Parallel()

	descs := []*Descriptor{
		{
			Amount: 1_000,
			Status: VTXOStatusLive,
		},
		{
			Amount: 2_000,
			Status: VTXOStatusLive,
		},
		{
			Amount: 5_000,
			Status: VTXOStatusPendingForfeit,
		},
		{
			Amount: 7_000,
			Status: VTXOStatusForfeiting,
		},
		{
			Amount: 4_834_745,
			Status: VTXOStatusSpending,
		},
	}

	// Only the two Live VTXOs are spendable.
	require.Equal(
		t, btcutil.Amount(3_000), SumSpendableBalance(descs),
	)

	// SumBalance over the same set sums everything, demonstrating why
	// GetBalance must not use it for the spendable figure.
	require.Equal(
		t, btcutil.Amount(4_849_745), SumBalance(descs),
	)
}

// TestSumSpendableBalanceAllNonSpendable confirms the issue's pathological
// case: a set containing no Live VTXOs (only a Spending + PendingForfeit
// reservation) reports zero spendable balance, not the inflated total.
func TestSumSpendableBalanceAllNonSpendable(t *testing.T) {
	t.Parallel()

	descs := []*Descriptor{
		{
			Amount: 4_834_745,
			Status: VTXOStatusSpending,
		},
		{
			Amount: 5_000,
			Status: VTXOStatusPendingForfeit,
		},
	}

	require.Zero(t, SumSpendableBalance(descs))
	require.Equal(t, btcutil.Amount(4_839_745), SumBalance(descs))
}

// TestSumSpendableBalanceEmpty guards the empty-input case.
func TestSumSpendableBalanceEmpty(t *testing.T) {
	t.Parallel()

	require.Zero(t, SumSpendableBalance(nil))
	require.Zero(t, SumSpendableBalance([]*Descriptor{}))
}
