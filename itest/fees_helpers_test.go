//go:build itest

package itest

import (
	"context"
	"testing"

	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo/fees"
	"github.com/lightninglabs/darepo/harness"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// defaultTestFeeRate is the fee rate every itest uses for
// on-chain cost estimation. It matches chainfee.FeePerKwFloor so
// the client-side expected-fee computation aligns with what the
// operator's static fee estimator produces in-process.
const defaultTestFeeRate = chainfee.FeePerKwFloor

// defaultBatchSizeForBoarding is the tree size the server's
// validateOperatorFee pins for ComputeBoardingFee (the full
// theoretical MaxVTXOsPerTree, not the live round occupancy).
// Mirrors config.DefaultRoundsConfig.MaxVTXOsPerTree so itest
// assertions compute the same per-input on-chain share the
// server uses to validate an incoming boarding.
const defaultBatchSizeForBoarding = 128

// newDefaultCalculator constructs a *fees.Calculator over the
// canonical DefaultItestFeeSchedule. Tests use it to reproduce
// the operator's fee computation in-process without needing to
// call the EstimateFee RPC.
func newDefaultCalculator(t *testing.T) *fees.Calculator {
	t.Helper()

	cfg := harness.DefaultItestFeeSchedule()

	policy, err := fees.ParseDustPolicy(cfg.MinViableVTXOPolicy)
	require.NoError(t, err, "parse default dust policy")

	sched := &fees.Schedule{
		AnnualRate:                 cfg.AnnualRate,
		BaseMarginSat:              cfg.BaseMarginSat,
		UtilizationThresholdBPS:    cfg.UtilizationThresholdBPS,
		UtilizationSpreadDelta0BPS: cfg.UtilizationSpreadDelta0BPS,
		UtilizationSpreadDelta1BPS: cfg.UtilizationSpreadDelta1BPS,
		MinViableVTXOPolicy:        policy,
		MinViableVTXOPct:           cfg.MinViableVTXOPct,
		MinRefreshDeltaBlocks:      cfg.MinRefreshDeltaBlocks,
	}

	calc, err := fees.NewCalculator(sched)
	require.NoError(t, err, "build default test calculator")

	return calc
}

// feeQuoteForBoarding returns the operator fee (sat) the server
// is expected to charge for a boarding input of grossSat, given
// a batch of batchSize participants at the default test fee
// rate. Utilization is not a factor for boarding (boarding does
// not deploy operator capital), so this quote is stable across
// treasury state.
func feeQuoteForBoarding(t *testing.T,
	grossSat int64, batchSize int) int64 {

	t.Helper()

	calc := newDefaultCalculator(t)
	b := calc.ComputeBoardingFee(
		grossSat, batchSize, defaultTestFeeRate,
	)

	return b.TotalFeeSat
}

// feeQuoteForRefresh returns the operator fee (sat) the server
// is expected to charge for a forfeit/refresh of grossSat with
// remainingBlocks of time left, at the given utilization. Pass
// utilization=0 for the first-round case.
func feeQuoteForRefresh(t *testing.T, grossSat int64, batchSize int,
	remainingBlocks uint32, utilization float64) int64 {

	t.Helper()

	calc := newDefaultCalculator(t)
	b := calc.ComputeForfeitFee(
		grossSat, batchSize, remainingBlocks,
		defaultTestFeeRate, utilization,
	)

	return b.TotalFeeSat
}

// expectedNetAfterBoarding returns the VTXO balance a client
// should observe after a boarding of grossSat into a round with
// batchSize participants.
func expectedNetAfterBoarding(t *testing.T,
	grossSat int64, batchSize int) int64 {

	t.Helper()

	return grossSat - feeQuoteForBoarding(t, grossSat, batchSize)
}

// operatorEstimateFee queries the operator's client-facing
// EstimateFee RPC directly (not via the mailbox) and returns the
// total fee. Used by tests that need the server's exact quote at
// a specific moment (for example, post-boarding when utilization
// has moved). Bypassing the mailbox keeps the helper independent
// of client-daemon state.
func operatorEstimateFee(t *testing.T,
	h *harness.ArkHarness, grossSat int64, boarding bool,
	remainingBlocks uint32) *arkrpc.EstimateFeeResponse {

	t.Helper()

	conn, err := grpc.Dial(
		h.ArkRPCAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err, "dial client-facing ark RPC")
	defer conn.Close()

	client := arkrpc.NewArkServiceClient(conn)

	ctx, cancel := context.WithTimeout(
		t.Context(), defaultSmallTimeout,
	)
	defer cancel()

	resp, err := client.EstimateFee(ctx, &arkrpc.EstimateFeeRequest{
		AmountSat:       grossSat,
		IsBoarding:      boarding,
		RemainingBlocks: remainingBlocks,
	})
	require.NoError(t, err, "EstimateFee RPC")

	return resp
}
