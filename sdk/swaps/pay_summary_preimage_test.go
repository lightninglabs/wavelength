package swaps

import (
	"testing"

	swapsqlc "github.com/lightninglabs/wavelength/sdk/swaps/sqlc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// TestPaySummaryFromRowSurfacesPreimage verifies a completed pay row exposes
// its persisted preimage on the summary, the proof of payment for the paid
// invoice.
func TestPaySummaryFromRowSurfacesPreimage(t *testing.T) {
	t.Parallel()

	var preimage lntypes.Preimage
	for i := range preimage {
		preimage[i] = byte(i + 1)
	}
	paymentHash := preimage.Hash()

	row := swapsqlc.PaySwap{
		PaymentHash: paymentHash[:],
		State:       PayStateCompleted.String(),
		AmountSat:   10_000,
		Preimage:    preimage[:],
	}

	summary, err := paySummaryFromRow(row)
	require.NoError(t, err)
	require.NotNil(t, summary.Preimage)
	require.Equal(t, preimage, *summary.Preimage)
}

// TestPaySummaryFromRowNilPreimageWhilePending verifies a pay row that has not
// yet revealed a preimage leaves the summary preimage nil rather than zero.
func TestPaySummaryFromRowNilPreimageWhilePending(t *testing.T) {
	t.Parallel()

	var paymentHash lntypes.Hash
	for i := range paymentHash {
		paymentHash[i] = 0xab
	}

	row := swapsqlc.PaySwap{
		PaymentHash: paymentHash[:],
		State:       PayStateWaitingForClaim.String(),
		AmountSat:   10_000,
	}

	summary, err := paySummaryFromRow(row)
	require.NoError(t, err)
	require.Nil(t, summary.Preimage)
}

// TestPaySummaryFromLegacyRowReconcilesFeeTerms verifies pre-migration rows
// expose a conservative split instead of an irreconcilable zero breakdown.
func TestPaySummaryFromLegacyRowReconcilesFeeTerms(t *testing.T) {
	t.Parallel()

	var paymentHash lntypes.Hash
	paymentHash[0] = 0xcd

	summary, err := paySummaryFromRow(swapsqlc.PaySwap{
		PaymentHash: paymentHash[:],
		State:       PayStateCompleted.String(),
		AmountSat:   10_100,
		FeeSat:      100,
	})
	require.NoError(t, err)
	require.Zero(t, summary.ServerFeeSat)
	require.Equal(t, uint64(100), summary.RoutingFeeBudgetSat)
	require.Equal(
		t, summary.FeeSat,
		summary.ServerFeeSat+summary.RoutingFeeBudgetSat,
	)
}
