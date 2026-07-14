package unroll

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/txconfirm"
	"github.com/lightninglabs/wavelength/vtxo"
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
		testExitPlanDescriptor(1_000_000, 2), nil, 1,
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
		testExitPlanDescriptor(1_000_000, 2), nil, 1,
		ExitFundingSnapshot{
			WalletConfirmedSat: 100,
			WalletUsableInputs: 2,
		},
	)

	require.False(t, plan.Feasibility.Feasible)
	require.Equal(t, ExitWalletUnderfunded, plan.Feasibility.Reason)

	// Two recovery txs at 1 sat/vB. The CPFP budget covers both the
	// children (2 * 155) and the zero-fee recovery txs themselves, which
	// have no extracted path here so they fall back to
	// defaultRecoveryTxVBytes (2 * 200): total 710 sat. Minus the 100 sat
	// confirmed balance, the shortfall is 610.
	require.EqualValues(t, 610, plan.FundingShortfallSat)
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
