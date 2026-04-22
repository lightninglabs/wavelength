package harness

import (
	"github.com/lightninglabs/darepo"
)

// DefaultItestFeeSchedule returns the canonical non-zero fee
// schedule every itest and systest runs against unless the test
// explicitly opts out via WithZeroFeeSchedule or supplies a
// custom schedule via WithFeesSchedule.
//
// The values are chosen so that:
//
//  1. Typical 100_000-sat refreshes are not dusted by the
//     MinViableVTXOPct=30 policy.
//  2. Every branch of rounds.validateOperatorFee can be driven
//     by adversarial test inputs (BaseMarginSat is small enough
//     that a mis-sized refresh reliably undershoots TotalFeeSat,
//     and AnnualRate combined with MinRefreshDeltaBlocks=10
//     produces non-zero liquidity fees at reasonable amounts).
//  3. The itest schedule mirrors production semantics (same
//     units, same curve shape) at reduced magnitudes so the
//     fee-aware code paths are exercised under load identical
//     in shape to mainnet.
//
// MinRefreshDeltaBlocks is set well below the production default
// of 144 so tests don't need to either mine huge numbers of
// blocks or refresh massive VTXO amounts to see a non-zero
// liquidity-fee leg.
func DefaultItestFeeSchedule() *darepo.FeesConfig {
	return &darepo.FeesConfig{
		AnnualRate:                 0.05,
		BaseMarginSat:              100,
		UtilizationThresholdBPS:    7000,
		UtilizationSpreadDelta0BPS: 200,
		UtilizationSpreadDelta1BPS: 1000,
		MinViableVTXOPolicy:        "reject",
		MinViableVTXOPct:           30,
		MinRefreshDeltaBlocks:      10,
	}
}

// ZeroFeeSchedule returns a FeesConfig that disables every fee
// component. Equivalent to the pre-#263 itest default. Used by
// TestFeesDisabledGreenPath and by any test that needs to drive
// a pure zero-fee code path.
func ZeroFeeSchedule() *darepo.FeesConfig {
	return &darepo.FeesConfig{
		MinViableVTXOPolicy: "reject",
	}
}

// WithFeesSchedule returns an OperatorConfigMutator that installs
// the given fee schedule on the operator's config before arkd
// starts. Use in itests to opt into a fee schedule different
// from DefaultItestFeeSchedule().
//
// Passing nil installs the zero schedule (equivalent to
// WithZeroFeeSchedule). Passing DefaultItestFeeSchedule() is a
// no-op because that is the harness default.
func WithFeesSchedule(cfg *darepo.FeesConfig) func(*darepo.Config) {
	return func(c *darepo.Config) {
		if cfg == nil {
			c.Fees = ZeroFeeSchedule()
			return
		}
		c.Fees = cfg
	}
}

// WithZeroFeeSchedule returns an OperatorConfigMutator that
// installs the zero schedule. Useful for regression tests that
// verify the fee-disabled code path still works post-#263.
func WithZeroFeeSchedule() func(*darepo.Config) {
	return func(c *darepo.Config) {
		c.Fees = ZeroFeeSchedule()
	}
}
