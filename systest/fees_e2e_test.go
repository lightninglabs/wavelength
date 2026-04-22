//go:build systest

package systest

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/darepo/fees"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

// TestFeesE2EScheduleRoundTrips is a smoke test that exercises
// the full fees subsystem from systest harness startup through
// fee-aware schedule lookup. Under the harness default (issue
// #263 forcing function) the E2EHarness fee calculator is
// seeded with the canonical non-zero systest schedule; this
// test verifies (a) the calculator is wired, (b) the schedule
// reflects the harness default, (c) a fee quote for a typical
// refresh amount produces a non-zero total with the expected
// shape.
//
// A deeper boarding->refresh->sweep->directed-send cycle with
// AssertLedgerDelta-style per-leg invariants is tracked in a
// follow-up (issue #263 E.10 full scope) since it requires
// wiring the client-side dynamic-fee quoting through the
// systest TestClient's admission flow. This smoke test
// validates the scaffolding so the deeper test can be written
// without infra questions.
func TestFeesE2EScheduleRoundTrips(t *testing.T) {
	ParallelN(t)

	h := NewE2EHarness(t)
	h.Start()

	h.FundServerWallet(btcutil.SatoshiPerBitcoin)

	// The fee calculator must be non-nil after the harness
	// starts, and its schedule must match the harness default.
	require.NotNil(
		t, h.feeCalculator,
		"fee calculator must be wired at harness start",
	)

	sched := h.feeCalculator.Schedule()
	require.NotNil(t, sched)

	// Default schedule: non-zero annual rate, reject dust
	// policy, non-zero margin. A test that calls DisableFees()
	// would see a zero schedule.
	require.Positive(
		t, sched.AnnualRate,
		"default systest schedule must carry a non-zero rate",
	)
	require.Equal(
		t, fees.DustPolicyReject, sched.MinViableVTXOPolicy,
	)

	// Fee quote for a typical refresh: all three legs present.
	quote := h.feeCalculator.ComputeForfeitFee(
		100_000, 128, 10, defaultSystestFeeRate(), 0.0,
	)
	require.NotNil(t, quote)
	require.Greater(
		t, quote.TotalFeeSat, int64(0),
		"non-zero schedule must produce a non-zero fee",
	)
	require.Equal(
		t,
		quote.LiquidityFeeSat+quote.OnChainShareSat+quote.MarginSat,
		quote.TotalFeeSat,
		"TotalFeeSat must equal the sum of components",
	)

	// Give the actor system a beat to settle before teardown
	// so any outbound ledger events have a chance to land.
	time.Sleep(100 * time.Millisecond)
}

// defaultSystestFeeRate is the rate the harness's fee estimator
// reports; kept local so the test is self-contained.
func defaultSystestFeeRate() chainfee.SatPerKWeight {
	return chainfee.FeePerKwFloor
}
