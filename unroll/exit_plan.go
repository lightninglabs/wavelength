package unroll

import (
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightninglabs/wavelength/txconfirm"
	"github.com/lightninglabs/wavelength/vtxo"
)

const (
	// RequiredFeeInputConfirmations is the number of confirmations a
	// backing-wallet UTXO must have before it can fund unroll CPFP.
	RequiredFeeInputConfirmations uint32 = 1

	// DefaultFeeInputMinAmountSat is the soft floor for counting a
	// confirmed wallet UTXO as an independently usable CPFP fee input.
	DefaultFeeInputMinAmountSat = btcutil.Amount(10_000)
)

// ExitFundingSnapshot is the wallet state needed to plan unilateral-exit
// funding.
type ExitFundingSnapshot struct {
	WalletConfirmedSat btcutil.Amount
	WalletUsableInputs int
}

// ExitFundingPlan is the user-facing funding projection derived from the
// unroll feasibility model.
type ExitFundingPlan struct {
	Feasibility ExitFeasibility

	RequiredConfirmations      uint32
	RecommendedUTXOAmountSat   btcutil.Amount
	RecommendedTotalFundingSat btcutil.Amount
	FundingShortfallSat        btcutil.Amount
}

// PlanExitFunding computes the backing-wallet funding plan for a unilateral
// exit using the same recovery counts and economic model as unroll admission.
//
// mat is the resolved lineage material for the target and may be nil. When
// supplied it lets the estimate size the OOR checkpoint/ark transactions
// exactly; when nil the OOR chain is approximated from the descriptor's
// ChainDepth scalar, so a caller that cannot (or need not) resolve artifacts
// still gets a sound descriptor-only plan.
func PlanExitFunding(desc *vtxo.Descriptor, mat *LineageMaterial,
	feeRate btcutil.Amount, wallet ExitFundingSnapshot) ExitFundingPlan {

	numTxs, numPaths, vbytes := recoveryEstimate(desc, mat)
	var amount btcutil.Amount
	if desc != nil {
		amount = desc.Amount
	}

	feasibility := AssessExitFeasibility(ExitFeasibilityInput{
		NumRecoveryTxs:     numTxs,
		RecoveryTxVBytes:   vbytes,
		NumAncestryPaths:   numPaths,
		VTXOAmountSat:      amount,
		FeeRateSatPerVByte: feeRate,
		WalletConfirmedSat: wallet.WalletConfirmedSat,
		WalletUsableInputs: wallet.WalletUsableInputs,
	})

	recommended := RecommendedExitFeeInputAmount(feasibility)

	return ExitFundingPlan{
		Feasibility:              feasibility,
		RequiredConfirmations:    RequiredFeeInputConfirmations,
		RecommendedUTXOAmountSat: recommended,
		RecommendedTotalFundingSat: totalExitFunding(
			feasibility, recommended,
		),
		FundingShortfallSat: exitFundingShortfall(
			feasibility, recommended,
		),
	}
}

// RecommendedExitFeeInputAmount derives the per-funding-output suggestion from
// the full unroll feasibility cost breakdown.
func RecommendedExitFeeInputAmount(verdict ExitFeasibility) btcutil.Amount {
	if verdict.RequiredWalletInputs <= 0 {
		return 0
	}

	perInputFee := ceilDivAmount(
		verdict.CPFPFeeTotalSat,
		btcutil.Amount(verdict.RequiredWalletInputs),
	)
	recommended := perInputFee + txconfirm.DustLimit
	if recommended < DefaultFeeInputMinAmountSat {
		return DefaultFeeInputMinAmountSat
	}

	return recommended
}

func totalExitFunding(verdict ExitFeasibility,
	recommended btcutil.Amount) btcutil.Amount {

	return btcutil.Amount(verdict.RequiredWalletInputs) * recommended
}

func exitFundingShortfall(verdict ExitFeasibility,
	recommended btcutil.Amount) btcutil.Amount {

	required := verdict.RequiredWalletInputs
	usable := verdict.WalletUsableInputs

	missingInputs := 0
	if usable < required {
		missingInputs = required - usable
	}

	inputShortfall := btcutil.Amount(missingInputs) * recommended

	var balanceShortfall btcutil.Amount
	if verdict.WalletConfirmedSat < verdict.CPFPFeeTotalSat {
		balanceShortfall = verdict.CPFPFeeTotalSat -
			verdict.WalletConfirmedSat
	}

	if inputShortfall > balanceShortfall {
		return inputShortfall
	}

	return balanceShortfall
}

func ceilDivAmount(a, b btcutil.Amount) btcutil.Amount {
	if b <= 0 {
		return 0
	}

	return (a + b - 1) / b
}
