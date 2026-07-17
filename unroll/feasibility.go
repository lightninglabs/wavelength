package unroll

import (
	"fmt"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/lightninglabs/wavelength/lib/recovery"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/txconfirm"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightningnetwork/lnd/input"
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
// broadcast fails "min relay fee not met", per wavelength #608.

const (
	// defaultCPFPChildVBytes is a conservative virtual-size estimate
	// for the CPFP child the txconfirm broadcaster builds per recovery
	// transaction: it spends the ephemeral P2A anchor (tiny witness),
	// one confirmed wallet fee input (taproot key-spend), and creates
	// one P2TR change output. Taproot is the dominant path and 155 vB
	// gives comfortable headroom over the exact size.
	defaultCPFPChildVBytes int64 = 155

	// defaultRecoveryTxVBytes is the fallback virtual size charged for a
	// single recovery (branch / checkpoint) transaction when its actual
	// size can't be measured — a pruned ancestry fragment that persisted
	// only a depth, or an OOR checkpoint hop. When the extracted tree
	// path is present we sum the real per-node sizes instead (see
	// RecoveryTxVBytes), which is exact across any tree radix. The
	// constant is deliberately generous so the fallback does not
	// under-fund an exit.
	defaultRecoveryTxVBytes int64 = 200

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

	// RecoveryTxVBytes is the summed virtual size of those recovery
	// transactions themselves. Each recovery tx is zero-fee, so its CPFP
	// child pays the ancestor-package fee over parent+child weight; the
	// wallet-funded cost therefore includes these parent vBytes, not just
	// the children. Resolve it with RecoveryTxVBytes(desc).
	RecoveryTxVBytes int64

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

	// The wallet funds, per recovery tx, a CPFP child AND the ancestor
	// package fee for the zero-fee recovery tx itself. So the CPFP budget
	// is the fee over (sum of child vBytes) + (sum of recovery-tx vBytes),
	// matching the ancestor-aware package fee the txconfirm broadcaster
	// actually pays. Omitting RecoveryTxVBytes here (the old behavior)
	// under-counted the exit cost and under-recommended funding.
	cpfpVBytes := cfg.CPFPChildVBytes*int64(in.NumRecoveryTxs) +
		in.RecoveryTxVBytes
	cpfpTotal := btcutil.Amount(
		int64(in.FeeRateSatPerVByte) * cpfpVBytes,
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
//
// This is the descriptor-only estimate: it approximates the OOR chain from
// the ChainDepth scalar. Pass resolved lineage material to recoveryEstimate
// (via PlanExitFunding) to size the OOR checkpoint/ark transactions exactly.
func RecoveryTxCount(desc *vtxo.Descriptor) (int, int) {
	numTxs, numPaths, _ := recoveryEstimate(desc, nil)

	return numTxs, numPaths
}

// RecoveryTxVBytes estimates the summed virtual size of every recovery
// transaction that must be broadcast to materialize the target output. It is
// the parent-weight companion to RecoveryTxCount: the CPFP child that drives
// each zero-fee recovery tx on chain pays the ancestor-package fee over
// parent+child, so the exit's wallet-funded cost depends on these sizes.
//
// For a fragment that still carries its extracted tree path we sum the actual
// per-node transaction size, which is exact for any tree radix (a wider branch
// simply has more child outputs). For a pruned fragment (depth only) or an OOR
// checkpoint hop, where the real transactions aren't on hand, we fall back to
// defaultRecoveryTxVBytes per transaction. A nil descriptor yields 0.
//
// This is the descriptor-only estimate; see recoveryEstimate for the
// material-aware path that sizes OOR transactions exactly.
func RecoveryTxVBytes(desc *vtxo.Descriptor) int64 {
	_, _, vbytes := recoveryEstimate(desc, nil)

	return vbytes
}

// recoveryEstimate derives, in one pass, the three quantities the exit-cost
// model needs from a VTXO's lineage: the recovery-transaction count (one CPFP
// child each), the number of independent commitment-tree roots (one concurrent
// CPFP parent each), and the summed virtual size of the recovery transactions
// themselves (the zero-fee parents the CPFP children must pay for at ancestor
// feerate). Computing all three together guarantees the count and the vBytes
// stay in lockstep — a fragment that contributes a tx to the count always
// contributes a parent size, and vice versa.
//
// The commitment-tree ancestry is always sized from the descriptor's extracted
// paths, which is exact for any radix; fragments that overlap (same-commitment
// multi-leaf ancestry shares prefix nodes and the root CPFP parent) are
// deduplicated by txid so the estimate matches the post-dedup broadcast set.
// The OOR chain is sized from the resolved lineage material when it is
// supplied (each finalized checkpoint/ark tx measured directly), and otherwise
// falls back to the ChainDepth scalar at the default per-tx size. A nil
// descriptor yields (0, 0, 0).
func recoveryEstimate(desc *vtxo.Descriptor, mat *LineageMaterial) (int, int,
	int64) {

	if desc == nil {
		return 0, 0, 0
	}

	var (
		numTxs   int
		numPaths int
		vbytes   int64
	)

	// Same-commitment multi-leaf fragments legitimately overlap: they
	// share their prefix nodes AND their root CPFP parent (both roots
	// spend the same batch outpoint). The proof assembler dedupes
	// overlapping byte-identical nodes, so counting and sizing every
	// fragment independently would over-require concurrent wallet
	// inputs and over-reserve fees — with up to MaxAncestryPaths
	// fragments, enough for AssessExitFeasibility to refuse an exit
	// that is actually affordable. Dedupe cross-fragment by node txid
	// (and CPFP parents by root txid) so the estimate matches what the
	// broadcaster will actually publish. Within one fragment nodes are
	// distinct by construction, so dedup only applies across
	// fragments.
	seenTxids := make(map[chainhash.Hash]struct{})
	seenRoots := make(map[chainhash.Hash]struct{})
	for _, a := range desc.Ancestry {
		pathTxs := 0
		var pathVBytes int64
		newPath := true
		switch {
		// Require a non-nil Root, not just a non-nil TreePath:
		// Tree.NumTx dereferences Root without a guard, so a
		// TreePath with a nil Root would panic here. Falling through
		// to the depth or malformed-floor arms instead keeps the
		// count and the parent weight in lockstep and preserves the
		// "degrade, never block the exit" contract.
		case a.TreePath != nil && a.TreePath.Root != nil:
			// Count only nodes not already contributed by an
			// earlier fragment. Nodes whose txid cannot be
			// computed are counted unconditionally — a
			// conservative overcount, matching the pre-dedup
			// behavior for degenerate trees.
			fragTxids := make([]chainhash.Hash, 0)
			_ = a.TreePath.Root.ForEach(func(n *tree.Node) error {
				if n == nil {
					return nil
				}

				txid, err := n.TXID()
				if err == nil {
					if _, dup := seenTxids[txid]; dup {
						return nil
					}
					fragTxids = append(fragTxids, txid)
				}

				pathTxs++
				pathVBytes += nodeTxVBytes(n)

				return nil
			})
			for _, txid := range fragTxids {
				seenTxids[txid] = struct{}{}
			}

			// A fragment whose root tx is already funded by an
			// earlier fragment shares that fragment's CPFP
			// parent; it must not demand another concurrent
			// wallet input.
			rootTxid, err := a.TreePath.Root.TXID()
			if err == nil {
				if _, dup := seenRoots[rootTxid]; dup {
					newPath = false
				} else {
					seenRoots[rootTxid] = struct{}{}
				}
			}

		case a.TreeDepth > 0:
			// Tree pruned but depth persisted: use it as a rough
			// lower bound on the transactions in this fragment.
			pathTxs = int(a.TreeDepth)
			pathVBytes = int64(a.TreeDepth) *
				defaultRecoveryTxVBytes
		}

		if newPath {
			numPaths++

			// An ancestry path that exists always has at least
			// one commitment transaction. Floor both the count
			// and the parent weight so a malformed fragment (nil
			// TreePath and zero TreeDepth) still budgets a CPFP
			// child AND its parent, rather than silently costing
			// nothing and letting a wallet-underfunded exit slip
			// through. A deduped fragment (shared root) is
			// exempt: its transactions are already budgeted by
			// the fragment that introduced them.
			if pathTxs < 1 {
				pathTxs = 1
			}
			if pathVBytes == 0 {
				pathVBytes = defaultRecoveryTxVBytes
			}
		}

		numTxs += pathTxs
		vbytes += pathVBytes
	}

	oorTxs, oorVBytes := oorRecovery(desc, mat)
	numTxs += oorTxs
	vbytes += oorVBytes

	return numTxs, numPaths, vbytes
}

// oorRecovery returns the (txCount, vBytes) the OOR checkpoint chain adds to
// the recovery estimate. When resolved lineage material carrying the finalized
// checkpoint/ark transactions is supplied it counts and sizes each one exactly
// — a single OOR hop can carry several checkpoint PSBTs plus one ark tx, which
// the ChainDepth scalar alone cannot capture. Without material it falls back to
// one default-sized transaction per ChainDepth hop, matching RecoveryTxCount's
// long-standing approximation.
func oorRecovery(desc *vtxo.Descriptor, mat *LineageMaterial) (int, int64) {
	if mat != nil && len(mat.ExtraNodes) > 0 {
		var vbytes int64
		for _, n := range mat.ExtraNodes {
			vbytes += extraNodeVBytes(n)
		}

		return len(mat.ExtraNodes), vbytes
	}

	return desc.ChainDepth, int64(desc.ChainDepth) * defaultRecoveryTxVBytes
}

// extraNodeVBytes measures the virtual size of a finalized OOR recovery
// transaction (a checkpoint or ark tx). These carry real witnesses, so unlike
// the modeled tree nodes we size them directly with the same weight estimator
// the txconfirm broadcaster uses, keeping the funding recommendation aligned
// with the fee the broadcaster actually pays. A nil node or tx falls back to
// the default per-tx size so it still contributes a conservative parent weight.
func extraNodeVBytes(n *recovery.Node) int64 {
	if n == nil || n.Tx == nil {
		return defaultRecoveryTxVBytes
	}

	return (txconfirm.EstimateWeight(n.Tx) + 3) / 4
}

// nodeTxVBytes estimates the virtual size of the transaction a tree node
// represents: a single pre-signed MuSig2 key-spend input plus the node's own
// outputs (its child outputs and P2A anchor for a branch, or the VTXO output
// and anchor for a leaf). Extraction preserves each node's full output set, so
// this matches the transaction the broadcaster fee-bumps, radix and all.
func nodeTxVBytes(n *tree.Node) int64 {
	// ForEach can hand us a nil node if the tree carries a nil child, and
	// a node's Outputs slice can hold nil entries; guard both so a
	// well-formed-but-empty node can't panic the estimate. (A nil entry
	// in a node's Children map would still panic inside ForEach itself, on
	// the nil-receiver traversal — but built and deserialized trees never
	// carry nil children, so that path is unreachable in practice.)
	if n == nil {
		return 0
	}

	var est input.TxWeightEstimator
	est.AddTaprootKeySpendInput(txscript.SigHashDefault)
	for _, out := range n.Outputs {
		if out == nil {
			continue
		}
		est.AddTxOutput(out)
	}

	return int64(est.Weight().ToVB())
}
