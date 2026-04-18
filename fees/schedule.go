package fees

import (
	"fmt"
	"math"
)

// DustPolicy controls how the operator handles VTXOs whose post-fee
// value falls below the economic viability threshold.
type DustPolicy int

const (
	// DustPolicyReject rejects join requests where any output
	// VTXO would be uneconomical after fees.
	DustPolicyReject DustPolicy = iota

	// DustPolicyWarn accepts the request but flags the fee
	// estimate response with a below-dust warning.
	DustPolicyWarn
)

// String returns a human-readable representation of the dust
// policy.
func (d DustPolicy) String() string {
	switch d {
	case DustPolicyReject:
		return "reject"
	case DustPolicyWarn:
		return "warn"
	default:
		return fmt.Sprintf("DustPolicy(%d)", int(d))
	}
}

// ParseDustPolicy converts a string to a DustPolicy.
func ParseDustPolicy(s string) (DustPolicy, error) {
	switch s {
	case "reject", "":
		return DustPolicyReject, nil
	case "warn":
		return DustPolicyWarn, nil
	default:
		return 0, fmt.Errorf("unknown dust policy %q, "+
			"expected 'reject' or 'warn'", s)
	}
}

// Schedule contains all operator-configurable fee parameters. It
// is immutable once created; updates produce a new Schedule that
// is swapped atomically via the Calculator.
type Schedule struct {
	// AnnualRate is the annualized BTC-denominated cost of
	// capital (e.g. 0.05 for 5%). This is r in the fee
	// formula F = A*(delta/365)*r.
	AnnualRate float64

	// BaseMarginSat is the fixed operator margin in satoshis
	// added to every liquidity-requiring fee computation.
	// This is epsilon (ε) in the formula.
	BaseMarginSat int64

	// UtilizationThresholdBPS is the treasury utilization
	// level (in basis points, e.g. 7000 = 70%) above which
	// the congestion spread activates. This is u* in the
	// pricing formula.
	UtilizationThresholdBPS uint32

	// UtilizationSpreadDelta0BPS is the base spread in basis
	// points added to AnnualRate when utilization exceeds the
	// threshold. Converted to a rate by dividing by 10000.
	UtilizationSpreadDelta0BPS uint32

	// UtilizationSpreadDelta1BPS is the slope of the linear
	// congestion spread. The value is in basis points and
	// converted to a rate fraction by dividing by 10000.
	// The rate addition for a given excess utilization u-u*
	// is: Delta1BPS/10000 * (u - u*). For example, with
	// Delta1BPS=500 and 10% excess, the addition is
	// 0.05 * 0.10 = 0.005 (50 BPS added to r_eff).
	UtilizationSpreadDelta1BPS uint32

	// MinViableVTXOPolicy controls whether VTXOs below the
	// economic dust threshold are rejected or warned.
	MinViableVTXOPolicy DustPolicy

	// MinViableVTXOPct is the maximum fee-to-amount ratio (as
	// a percentage, e.g. 50 means fee must be < 50% of amount)
	// that defines "economically viable." VTXOs where the fee
	// exceeds this fraction trigger the dust policy.
	MinViableVTXOPct uint32

	// MinRefreshDeltaBlocks is the fee floor for refresh
	// operations, expressed in blocks. When the forfeited
	// VTXO's remaining lifetime δ is below this floor, the
	// liquidity fee is computed using δ_min instead of the
	// actual δ. This prevents a "lazy refresh" bypass where
	// users wait until δ ≈ 0 for near-zero fees. Default
	// is 144 blocks (~1 day). This is a pricing floor, not
	// an admission rule — refreshes at δ < δ_min are still
	// accepted.
	MinRefreshDeltaBlocks uint32
}

// DefaultSchedule returns a Schedule with sensible defaults for a
// regtest/development environment.
func DefaultSchedule() *Schedule {
	return &Schedule{
		AnnualRate:                 0.05,
		BaseMarginSat:              100,
		UtilizationThresholdBPS:    7000,
		UtilizationSpreadDelta0BPS: 100,
		UtilizationSpreadDelta1BPS: 500,
		MinViableVTXOPolicy:        DustPolicyReject,
		MinViableVTXOPct:           50,
		MinRefreshDeltaBlocks:      144,
	}
}

// Validate returns a non-nil error if any field is outside the
// bounds defined in docs/fee-model.md. Schedules are the admin
// surface for hot-reload, so operators can configure them at
// runtime; this check rejects obvious footguns (negative rates,
// out-of-range percentages) before they poison fee quotes or the
// congestion pricing curve.
func (s *Schedule) Validate() error {
	if s == nil {
		return fmt.Errorf("schedule is nil")
	}

	if s.AnnualRate < 0 {
		return fmt.Errorf("annual rate must be >= 0, got %v",
			s.AnnualRate)
	}
	if math.IsNaN(s.AnnualRate) || math.IsInf(s.AnnualRate, 0) {
		return fmt.Errorf("annual rate must be finite, got %v",
			s.AnnualRate)
	}

	if s.BaseMarginSat < 0 {
		return fmt.Errorf("base margin must be >= 0, got %d",
			s.BaseMarginSat)
	}

	if s.UtilizationThresholdBPS > 10_000 {
		return fmt.Errorf("utilization threshold must be "+
			"<= 10000 bps (100%%), got %d",
			s.UtilizationThresholdBPS)
	}

	if s.MinViableVTXOPct > 100 {
		return fmt.Errorf("min viable VTXO pct must be <= 100, "+
			"got %d", s.MinViableVTXOPct)
	}

	switch s.MinViableVTXOPolicy {
	case DustPolicyReject, DustPolicyWarn:
	default:
		return fmt.Errorf("unknown dust policy %d",
			s.MinViableVTXOPolicy)
	}

	return nil
}

// EffectiveRate returns the annual rate including the congestion
// spread at the given utilization level. The utilization is a
// ratio between 0.0 and 1.0.
//
// The congestion spread is a hockey-stick curve:
//
//	r_eff = r                       if u <= u*
//	r_eff = r + Δ₀ + Δ₁·(u - u*)    if u >  u*
//
// where u* = UtilizationThresholdBPS / 10000, Δ₀ =
// UtilizationSpreadDelta0BPS / 10000, and Δ₁ =
// UtilizationSpreadDelta1BPS / 10000.
//
// IMPORTANT — unit-mixing subtlety: Δ₁ is expressed in basis
// points OF RATE (per 10000), while (u - u*) is a UNIT RATIO of
// excess utilization. The two multiply into a rate delta. With
// the default `UtilizationSpreadDelta1BPS = 500` and excess
// utilization of 0.10 (u = u* + 0.10), the Δ₁ term contributes
//
//	500 / 10000 * 0.10 = 0.005  (= 50 bps added to r_eff)
//
// Misreading Δ₁ as "basis points per basis point of excess" would
// miscalculate the spread by a factor of 10000. See
// `UtilizationSpreadDelta1BPS` field docs for more.
//
// There is an intentional discontinuous step Δ₀ at u = u*
// (see docs/fee-model.md for the rationale: a clear "in
// congestion" signal that dominates utilization noise). Operators
// who want a smooth curve can set `UtilizationSpreadDelta0BPS =
// 0`, in which case Δ(u) is a continuous linear ramp rooted at
// the threshold.
func (s *Schedule) EffectiveRate(utilization float64) float64 {
	threshold := float64(s.UtilizationThresholdBPS) / 10000.0

	if utilization <= threshold {
		return s.AnnualRate
	}

	excess := utilization - threshold

	delta0 := float64(s.UtilizationSpreadDelta0BPS) / 10000.0
	delta1 := float64(s.UtilizationSpreadDelta1BPS) / 10000.0

	return s.AnnualRate + delta0 + delta1*excess
}
