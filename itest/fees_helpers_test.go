//go:build itest

package itest

import (
	"context"
	"testing"

	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo-client/daemonrpc"
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

// defaultItestBatchSize is the on-chain cost divisor used by
// fee-asserting itests. Under #270 the binding fee is decided at
// seal time against the real round occupancy; the EstimateFee RPC
// surface (which these helpers assert against) previews at
// batch=1, matching this constant.
const defaultItestBatchSize = 1

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
func feeQuoteForBoarding(t *testing.T, grossSat int64, batchSize int) int64 {
	t.Helper()

	calc := newDefaultCalculator(t)
	b := calc.ComputeBoardingFee(
		grossSat, batchSize, defaultTestFeeRate,
	)

	return b.TotalFeeSat
}

// expectedNetAfterBoarding returns the VTXO balance a client
// should observe after a boarding of grossSat into a round with
// batchSize participants.
func expectedNetAfterBoarding(t *testing.T, grossSat int64,
	batchSize int) int64 {

	t.Helper()

	return grossSat - feeQuoteForBoarding(t, grossSat, batchSize)
}

// expectedNetAfterRefresh returns the VTXO balance a client
// should observe after a refresh of a live VTXO. Under the
// seal-time fee handshake (#270) the server computes the
// authoritative operator fee at seal time and stamps the residual
// onto the client's change output; the client no longer pre-quotes
// the fee into the intent. Callers that previously expected the
// amount to carry through unchanged must switch to this helper.
//
// The expected fee is read straight from the server's EstimateFee
// RPC so the test stays in lock-step with whatever fee the seal-
// time quote builder will charge for this VTXO: the RPC and the
// builder share the same ComputeForfeitFee path. We mirror the
// client-side remaining-blocks computation here by resolving the
// current chain tip from the harness's bitcoind, so the test's
// expected amount and the server's final quote read from the same
// source of truth.
//
// remainingBlocks clamps to zero when BatchExpiry is already at
// or behind currentHeight; the server's EstimateFee then falls
// back to SweepDelay, matching the fee builder's δ_min floor.
func expectedNetAfterRefresh(t *testing.T, h *harness.ArkHarness,
	vtxo *daemonrpc.VTXO) int64 {

	t.Helper()

	currentHeight := int32(h.Harness.BlockCount())

	var remainingBlocks uint32
	if vtxo.BatchExpiry > currentHeight {
		remainingBlocks = uint32(vtxo.BatchExpiry - currentHeight)
	}

	quote := operatorEstimateFee(
		t, h, vtxo.AmountSat, false, /* isBoarding */
		remainingBlocks,
	)

	return vtxo.AmountSat - quote.TotalFeeSat
}

// operatorEstimateFee queries the operator's client-facing
// EstimateFee RPC directly (not via the mailbox) and returns the
// total fee. Used by tests that need the server's exact quote at
// a specific moment (for example, post-boarding when utilization
// has moved). Bypassing the mailbox keeps the helper independent
// of client-daemon state.
func operatorEstimateFee(t *testing.T, h *harness.ArkHarness, grossSat int64,
	boarding bool, remainingBlocks uint32) *arkrpc.EstimateFeeResponse {

	t.Helper()

	conn, err := grpc.Dial(
		h.ArkRPCAddr,
		grpc.WithTransportCredentials(
			insecure.NewCredentials(),
		),
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
