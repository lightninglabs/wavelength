package unroll

import (
	"fmt"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightninglabs/darepo-client/vtxo"
)

// A unilateral exit is the trust-minimized escape hatch a client uses to
// reclaim a VTXO entirely on-chain, without the operator's cooperation.
// Driving it to completion is not free: the client must broadcast (and
// fee-bump, via CPFP) every ancestor transaction in the VTXO's proof
// graph, then broadcast a final timeout-path sweep. Those fees come from
// two distinct purses:
//
//   - Wallet-funded: each recovery transaction is a zero-fee / anchor
//     parent driven on-chain by a CPFP child paid from a confirmed
//     on-chain wallet UTXO. This cost never touches the VTXO value.
//
//   - VTXO-funded: the final sweep spends the target output and pays its
//     own fee out of the VTXO value, so the amount that actually lands
//     back in the wallet is `vtxo.Amount - sweepFee`.
//
// AssessExitFeasibility folds both purses into a single up-front verdict
// so the admission path can refuse an exit that can never succeed (the
// swept output would be dust and the sweep tx unrelayable) or that is
// economically irrational (it burns more in fees than the coin is worth)
// before the VTXO is committed to UnilateralExitState. The alternative —
// admitting blindly — strands the VTXO in an exit state after the
// broadcast fails "min relay fee not met", per darepo-client #608.

const (
	// defaultCPFPChildVBytes is a conservative virtual-size estimate
	// for the CPFP child the txconfirm broadcaster builds per recovery
	// transaction: it spends the ephemeral P2A anchor (tiny witness),
	// one confirmed wallet fee input (taproot key-spend), and creates
	// one P2TR change output. Taproot is the dominant path and 155 vB
	// gives comfortable headroom over the exact size.
	defaultCPFPChildVBytes int64 = 155

	// defaultSweepOutputDustSat is the floor the swept output must clear
	// for the sweep transaction to relay. 330 sat is the standard
	// Bitcoin Core / btcwallet dust limit for a P2TR output at the
	// 1 sat/vB default minimum relay fee (the sweep always pays to a
	// P2TR wallet address). Configurable so a tighter or looser relay
	// policy can be tracked, but the default matches stock policy.
	defaultSweepOutputDustSat = btcutil.Amount(330)

	// defaultMaxRecoveryCostFractionBP bounds how much of the VTXO's
	// value the total on-chain recovery cost (CPFP fees + sweep fee)
	// may consume before the exit is judged uneconomical, expressed in
	// basis points of the VTXO value. The default of 10_000 bp (100%)
	// blocks only exits that are outright loss-making — recovering the
	// coin would cost at least as much as the coin holds. It is set at
	// 100% rather than lower on purpose: a unilateral exit is the
	// last-resort path used precisely when the operator is unreachable
	// or uncooperative, so the daemon must not paternalistically refuse
	// a merely-expensive (but still net-positive) recovery. Operators
	// who want a stricter "don't bother below X% efficiency" policy can
	// lower this.
	defaultMaxRecoveryCostFractionBP int64 = 10_000
)

// ExitFeasibilityConfig parameterizes the economic pre-flight. The zero
// value is valid: withDefaults fills every unset field with the
// documented default, so callers that don't care can pass an empty
// struct.
type ExitFeasibilityConfig struct {
	// CPFPChildVBytes is the estimated vsize of the CPFP fee-bump child
	// built per recovery transaction. Zero falls back to
	// defaultCPFPChildVBytes.
	CPFPChildVBytes int64

	// SweepVBytes is the estimated vsize of the timeout-path sweep.
	// Zero falls back to the package-wide estimatedSweepVBytes so the
	// pre-flight stays consistent with the sweep the unroll actor
	// actually builds.
	SweepVBytes int64

	// SweepOutputDustSat is the minimum value the swept output must have
	// to relay. Zero falls back to defaultSweepOutputDustSat.
	SweepOutputDustSat btcutil.Amount

	// MaxRecoveryCostFractionBP bounds the total recovery cost as a
	// fraction (in basis points) of the VTXO value. Zero falls back to
	// defaultMaxRecoveryCostFractionBP.
	MaxRecoveryCostFractionBP int64
}

// withDefaults returns a copy of the config with every unset field
// replaced by its documented default.
func (c ExitFeasibilityConfig) withDefaults() ExitFeasibilityConfig {
	if c.CPFPChildVBytes <= 0 {
		c.CPFPChildVBytes = defaultCPFPChildVBytes
	}
	if c.SweepVBytes <= 0 {
		c.SweepVBytes = estimatedSweepVBytes
	}
	if c.SweepOutputDustSat <= 0 {
		c.SweepOutputDustSat = defaultSweepOutputDustSat
	}
	if c.MaxRecoveryCostFractionBP <= 0 {
		c.MaxRecoveryCostFractionBP = defaultMaxRecoveryCostFractionBP
	}

	return c
}

// ExitInfeasibilityReason enumerates why a unilateral exit was judged
// infeasible. ExitFeasible is the zero value (no problem).
type ExitInfeasibilityReason uint8

const (
	// ExitFeasible means the exit passed every check.
	ExitFeasible ExitInfeasibilityReason = iota

	// ExitSweepBelowDust means the swept output, after deducting the
	// sweep fee from the VTXO value, would fall at or below the dust
	// limit. The sweep transaction could never be relayed, so the exit
	// is impossible — no amount of wallet funding fixes it.
	ExitSweepBelowDust

	// ExitUneconomical means the total on-chain cost to recover the
	// VTXO (wallet-funded CPFP fees plus the VTXO-funded sweep fee)
	// exceeds the configured fraction of the VTXO value. The exit could
	// technically complete but burns more than it returns.
	ExitUneconomical

	// ExitWalletUnderfunded means the confirmed on-chain wallet balance
	// is too small to cover the CPFP fees for the recovery
	// transactions. Funding the wallet and retrying resolves it.
	ExitWalletUnderfunded

	// ExitWalletTooFewInputs means the wallet has fewer usable confirmed
	// UTXOs than the VTXO has independent ancestry paths. Each path is a
	// distinct CPFP parent needing its own fee input at child-
	// construction time, so distinct inputs — not just total balance —
	// are required.
	ExitWalletTooFewInputs
)

// String renders the reason for logs and error messages.
func (r ExitInfeasibilityReason) String() string {
	switch r {
	case ExitFeasible:
		return "feasible"

	case ExitSweepBelowDust:
		return "sweep_below_dust"

	case ExitUneconomical:
		return "uneconomical"

	case ExitWalletUnderfunded:
		return "wallet_underfunded"

	case ExitWalletTooFewInputs:
		return "wallet_too_few_inputs"

	default:
		return fmt.Sprintf("unknown(%d)", uint8(r))
	}
}

// Impossible reports whether the reason describes an exit that can never
// succeed regardless of wallet state or user override (the swept output
// is unrelayable). Callers that offer a force-override should still
// honor an Impossible verdict.
func (r ExitInfeasibilityReason) Impossible() bool {
	return r == ExitSweepBelowDust
}

// ExitFeasibilityInput carries everything the pure assessment needs. The
// caller resolves these from the VTXO descriptor (RecoveryTxCount), the
// chain fee estimator, and the wallet's confirmed UTXO set.
type ExitFeasibilityInput struct {
	// NumRecoveryTxs is the count of ancestor transactions that must be
	// broadcast (and CPFP-bumped) to materialize the target output.
	NumRecoveryTxs int

	// NumAncestryPaths is the number of independent commitment-tree
	// roots in the VTXO's ancestry — one concurrent CPFP parent each.
	NumAncestryPaths int

	// VTXOAmountSat is the value of the VTXO being exited.
	VTXOAmountSat btcutil.Amount

	// FeeRateSatPerVByte is the current estimated fee rate.
	FeeRateSatPerVByte btcutil.Amount

	// WalletConfirmedSat is the total confirmed on-chain wallet balance
	// available to fund CPFP children.
	WalletConfirmedSat btcutil.Amount

	// WalletUsableInputs is the number of confirmed wallet UTXOs large
	// enough to plausibly fund a CPFP child on their own.
	WalletUsableInputs int

	// Config tunes the cost model. The zero value uses all defaults.
	Config ExitFeasibilityConfig
}

// ExitFeasibility is the structured verdict plus the full cost breakdown
// that produced it, so callers can build actionable error messages and
// UX previews without recomputing anything.
type ExitFeasibility struct {
	// Feasible is true when every check passed.
	Feasible bool

	// Reason is ExitFeasible when Feasible, else the first failing
	// check.
	Reason ExitInfeasibilityReason

	// NumRecoveryTxs echoes the input recovery-transaction count.
	NumRecoveryTxs int

	// FeeRateSatPerVByte echoes the fee rate the estimate used.
	FeeRateSatPerVByte btcutil.Amount

	// CPFPFeeTotalSat is the wallet-funded cost: the summed CPFP child
	// fees across every recovery transaction.
	CPFPFeeTotalSat btcutil.Amount

	// SweepFeeSat is the VTXO-funded cost: the fee the final sweep pays
	// out of the VTXO value.
	SweepFeeSat btcutil.Amount

	// TotalRecoveryCostSat is CPFPFeeTotalSat + SweepFeeSat — the whole
	// on-chain cost to recover the coin.
	TotalRecoveryCostSat btcutil.Amount

	// NetRecoveredSat is VTXOAmountSat - SweepFeeSat: the value that
	// actually lands back in the wallet from the sweep.
	NetRecoveredSat btcutil.Amount

	// VTXOAmountSat echoes the input VTXO value.
	VTXOAmountSat btcutil.Amount

	// DustLimitSat is the resolved sweep-output dust floor used.
	DustLimitSat btcutil.Amount

	// RequiredWalletInputs is the number of distinct confirmed wallet
	// UTXOs the exit needs (one per ancestry path).
	RequiredWalletInputs int

	// WalletConfirmedSat / WalletUsableInputs echo the wallet inputs.
	WalletConfirmedSat btcutil.Amount
	WalletUsableInputs int
}

// AssessExitFeasibility runs the pure economic model over the supplied
// inputs and returns a verdict. It performs no IO and is the single
// source of truth for whether a unilateral exit should be admitted.
//
// Checks are evaluated most-fundamental first, and the first failure is
// reported:
//
//  1. ExitSweepBelowDust — the exit is impossible (unrelayable sweep).
//  2. ExitUneconomical — the exit is not worth doing.
//  3. ExitWalletUnderfunded — the wallet can't pay the CPFP fees.
//  4. ExitWalletTooFewInputs — the wallet lacks distinct fee inputs.
func AssessExitFeasibility(in ExitFeasibilityInput) ExitFeasibility {
	cfg := in.Config.withDefaults()

	sweepFee := btcutil.Amount(
		int64(in.FeeRateSatPerVByte) * cfg.SweepVBytes,
	)
	cpfpTotal := btcutil.Amount(
		int64(in.FeeRateSatPerVByte) * cfg.CPFPChildVBytes *
			int64(in.NumRecoveryTxs),
	)
	totalCost := cpfpTotal + sweepFee
	net := in.VTXOAmountSat - sweepFee

	f := ExitFeasibility{
		Feasible:             true,
		Reason:               ExitFeasible,
		NumRecoveryTxs:       in.NumRecoveryTxs,
		FeeRateSatPerVByte:   in.FeeRateSatPerVByte,
		CPFPFeeTotalSat:      cpfpTotal,
		SweepFeeSat:          sweepFee,
		TotalRecoveryCostSat: totalCost,
		NetRecoveredSat:      net,
		VTXOAmountSat:        in.VTXOAmountSat,
		DustLimitSat:         cfg.SweepOutputDustSat,
		RequiredWalletInputs: in.NumAncestryPaths,
		WalletConfirmedSat:   in.WalletConfirmedSat,
		WalletUsableInputs:   in.WalletUsableInputs,
	}

	// 1. Impossible: after the sweep pays its own fee, the output left
	//    for the wallet is below the dust limit and the sweep tx cannot
	//    be relayed. An output exactly at the dust limit is relayable
	//    (standard dust semantics reject only strictly-below), so the
	//    comparison is strict.
	if net < cfg.SweepOutputDustSat {
		f.Feasible = false
		f.Reason = ExitSweepBelowDust

		return f
	}

	// 2. Uneconomical: the whole-picture cost to recover the coin
	//    (wallet-funded CPFP + VTXO-funded sweep) exceeds the configured
	//    fraction of the coin's value. float64 keeps the basis-point
	//    math overflow-free; sat-level precision is irrelevant for a
	//    policy gate.
	maxCost := float64(in.VTXOAmountSat) *
		float64(cfg.MaxRecoveryCostFractionBP) / 10_000.0
	if float64(totalCost) >= maxCost {
		f.Feasible = false
		f.Reason = ExitUneconomical

		return f
	}

	// 3. Wallet can't fund the CPFP children at all.
	if in.WalletConfirmedSat < cpfpTotal {
		f.Feasible = false
		f.Reason = ExitWalletUnderfunded

		return f
	}

	// 4. Wallet has the balance but not enough distinct inputs: each
	//    independent ancestry path is a separate CPFP parent that needs
	//    its own confirmed fee input at child-construction time.
	if in.WalletUsableInputs < in.NumAncestryPaths {
		f.Feasible = false
		f.Reason = ExitWalletTooFewInputs

		return f
	}

	return f
}

// RecoveryTxCount derives, from a VTXO descriptor, two counts: the number
// of ancestor transactions that must be broadcast to materialize the
// target output, and the number of independent commitment-tree roots in
// its ancestry (returned in that order). Each ancestry fragment
// contributes its tree-path transactions; OOR VTXOs add one checkpoint
// transaction per hop in the OOR chain (ChainDepth). A nil descriptor
// yields (0, 0).
func RecoveryTxCount(desc *vtxo.Descriptor) (int, int) {
	if desc == nil {
		return 0, 0
	}

	var numTxs, numPaths int
	for _, a := range desc.Ancestry {
		numPaths++

		pathTxs := 0
		switch {
		case a.TreePath != nil:
			pathTxs = a.TreePath.NumTx()

		case a.TreeDepth > 0:
			// Tree pruned but depth persisted: use it as a rough
			// lower bound on the transactions in this fragment.
			pathTxs = int(a.TreeDepth)
		}

		// An ancestry path that exists always has at least one
		// commitment transaction. Floor at 1 so a malformed fragment
		// (nil TreePath and zero TreeDepth) still contributes to the
		// CPFP-fee estimate rather than silently costing nothing,
		// which would let a wallet-underfunded exit slip through.
		if pathTxs < 1 {
			pathTxs = 1
		}

		numTxs += pathTxs
	}

	numTxs += desc.ChainDepth

	return numTxs, numPaths
}
