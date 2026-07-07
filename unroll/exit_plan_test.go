package unroll

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/darepo-client/txconfirm"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/stretchr/testify/require"
)

// TestRecommendedExitFeeInputAmountUsesFeasibilityCosts verifies that the
// suggested per-UTXO funding amount follows the feasibility cost breakdown.
func TestRecommendedExitFeeInputAmountUsesFeasibilityCosts(t *testing.T) {
	t.Parallel()

	verdict := ExitFeasibility{
		CPFPFeeTotalSat:      50_000,
		RequiredWalletInputs: 2,
	}

	require.EqualValues(
		t, 25_000+txconfirm.DustLimit,
		RecommendedExitFeeInputAmount(verdict),
	)

	verdict.CPFPFeeTotalSat = 1
	require.Equal(
		t, DefaultFeeInputMinAmountSat,
		RecommendedExitFeeInputAmount(verdict),
	)
}

// TestPlanExitFundingShortfallCoversMissingInputs verifies that the plan tells
// callers to create distinct funding UTXOs when balance alone is insufficient.
func TestPlanExitFundingShortfallCoversMissingInputs(t *testing.T) {
	t.Parallel()

	plan := PlanExitFunding(
		testExitPlanDescriptor(1_000_000, 2), 1,
		ExitFundingSnapshot{
			WalletConfirmedSat: 100_000,
			WalletUsableInputs: 1,
		},
	)

	require.False(t, plan.Feasibility.Feasible)
	require.Equal(t, ExitWalletTooFewInputs, plan.Feasibility.Reason)
	require.Equal(t, DefaultFeeInputMinAmountSat, plan.FundingShortfallSat)
}

// TestPlanExitFundingShortfallCoversBalance verifies that the plan reports the
// missing CPFP balance when the wallet has enough distinct inputs.
func TestPlanExitFundingShortfallCoversBalance(t *testing.T) {
	t.Parallel()

	plan := PlanExitFunding(
		testExitPlanDescriptor(1_000_000, 2), 1,
		ExitFundingSnapshot{
			WalletConfirmedSat: 100,
			WalletUsableInputs: 2,
		},
	)

	require.False(t, plan.Feasibility.Feasible)
	require.Equal(t, ExitWalletUnderfunded, plan.Feasibility.Reason)
	require.EqualValues(t, 210, plan.FundingShortfallSat)
}

// TestExitFundingAddressBookReusesCachedAddress verifies polling a plan for
// the same target does not advance the underlying wallet address index.
func TestExitFundingAddressBookReusesCachedAddress(t *testing.T) {
	t.Parallel()

	var book ExitFundingAddressBook
	calls := 0
	newAddress := func(context.Context) (string, error) {
		calls++

		return "bcrt1preallocated", nil
	}

	address, err := book.Address(t.Context(), "txid:0", newAddress)
	require.NoError(t, err)
	require.Equal(t, "bcrt1preallocated", address)

	address, err = book.Address(t.Context(), "txid:0", newAddress)
	require.NoError(t, err)
	require.Equal(t, "bcrt1preallocated", address)
	require.Equal(t, 1, calls)
}

func testExitPlanDescriptor(amount btcutil.Amount, paths int) *vtxo.Descriptor {
	desc := &vtxo.Descriptor{
		Amount: amount,
	}
	for i := 0; i < paths; i++ {
		desc.Ancestry = append(desc.Ancestry, vtxo.Ancestry{
			CommitmentTxID: chainhash.Hash{byte(i + 1)},
		})
	}

	return desc
}
