//go:build swapruntime

package swapclientserver

import (
	"errors"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/lightninglabs/wavelength/rpc/swapclientrpc"
	"github.com/lightninglabs/wavelength/sdk/swaps"
	"github.com/stretchr/testify/require"
)

// TestDefaultInSwapMaxFeeSat verifies the proportional ~1% cap with its
// absolute floor for representative swap sizes.
func TestDefaultInSwapMaxFeeSat(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		amountSat uint64
		want      uint64
	}{
		{
			// The exact case from the bug report: a 5_000 sat swap
			// quoted 1 sat. 1% is 50 sat, comfortably above the 1
			// sat quote, so the payment now clears where the old
			// 0-cap default rejected it.
			name:      "bug report swap clears",
			amountSat: 5_000,
			want:      50,
		},
		{
			// Below 1_000 sat the 1% cap rounds beneath the floor,
			// so the absolute floor takes over and keeps the tiny
			// payment routable.
			name:      "sub-thousand swap floored",
			amountSat: 500,
			want:      defaultInSwapMaxFeeFloorSat,
		},
		{
			// 1% of an intermediate sub-1_000_000 swap. This case
			// guards the multiply-before-divide ordering: integer
			// division first would truncate the cap to the floor.
			name:      "proportional fee below one million",
			amountSat: 500_000,
			want:      5_000,
		},
		{
			name:      "one percent above floor",
			amountSat: 1_000_000,
			want:      10_000,
		},
		{
			name:      "one percent of large swap",
			amountSat: 50_000_000,
			want:      500_000,
		},
		{
			name:      "zero amount floored",
			amountSat: 0,
			want:      defaultInSwapMaxFeeFloorSat,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(
				t, tc.want,
				defaultInSwapMaxFeeSat(tc.amountSat),
			)
		})
	}
}

// TestEffectiveMaxFeeSatHonoursCaller verifies a caller-supplied non-zero cap
// is forwarded verbatim and never overwritten by the proportional default.
func TestEffectiveMaxFeeSatHonoursCaller(t *testing.T) {
	t.Parallel()

	s := &swapClientService{chainParams: &chaincfg.RegressionNetParams}

	got := s.effectiveMaxFeeSat(42, "lnbc1invalid")
	require.Equal(t, uint64(42), got)
}

// TestEffectiveMaxFeeSatDefaultsFromInvoice verifies a zero cap is replaced
// with the ~1% default derived from the decoded invoice amount.
func TestEffectiveMaxFeeSatDefaultsFromInvoice(t *testing.T) {
	t.Parallel()

	s := &swapClientService{chainParams: &chaincfg.RegressionNetParams}

	invoice := testStartPayInvoice(t, testHash(7), 5_000)
	got := s.effectiveMaxFeeSat(0, invoice)
	require.Equal(t, uint64(50), got)

	invoice = testStartPayInvoice(t, testHash(8), 1_000_000)
	got = s.effectiveMaxFeeSat(0, invoice)
	require.Equal(t, uint64(10_000), got)
}

// TestEffectiveMaxFeeSatPreservesZeroOnUndecodable verifies an undecodable
// invoice leaves the caller's zero intact so the existing downstream validation
// surfaces the original decode error rather than a derived cap.
func TestEffectiveMaxFeeSatPreservesZeroOnUndecodable(t *testing.T) {
	t.Parallel()

	s := &swapClientService{chainParams: &chaincfg.RegressionNetParams}

	require.Equal(t, uint64(0), s.effectiveMaxFeeSat(0, "not-an-invoice"))
}

// TestStartPayDefaultsMaxFeeWhenUnset verifies StartPay forwards the ~1%
// default to sdk/swaps when the caller does not pin a max fee, so a routine
// payment is not rejected by a 0 sat hard cap.
func TestStartPayDefaultsMaxFeeWhenUnset(t *testing.T) {
	t.Parallel()

	payHash := testHash(91)
	invoice := testStartPayInvoice(t, payHash, 1_000_000)

	// The seeded summary is terminal so the pending-invoice dedup misses
	// and StartPay proceeds to StartPayViaLightning, where the forwarded
	// max fee is recorded. summaryByHash still resolves it afterwards.
	fakeClient := newFakeSwapRuntime(
		swaps.SwapSummary{
			Direction:   swaps.SwapDirectionPay,
			PaymentHash: payHash,
			State:       "completed",
			Pending:     false,
			AmountSat:   1_000_000,
		},
	)
	fakeClient.startPaySession = &fakePaySession{hash: payHash}
	service := newTestSwapClientService(fakeClient)
	service.chainParams = &chaincfg.RegressionNetParams
	defer service.cancel()

	_, err := service.StartPay(
		t.Context(), &swapclientrpc.StartPayRequest{
			Invoice:   invoice,
			MaxFeeSat: 0,
		},
	)
	require.NoError(t, err)
	require.Equal(t, uint64(10_000), fakeClient.startPayMaxFeeSat)
}

// TestQuotePayDefaultsMaxFeeWhenUnset verifies QuotePay derives the same ~1%
// default for the non-mutating preview path.
func TestQuotePayDefaultsMaxFeeWhenUnset(t *testing.T) {
	t.Parallel()

	payHash := testHash(92)
	invoice := testStartPayInvoice(t, payHash, 1_000_000)
	fakeClient := newFakeSwapRuntime()
	fakeClient.quotePayResp = &swaps.InSwapQuote{
		PaymentHash:      payHash,
		InvoiceAmountSat: 1_000_000,
		AmountSat:        1_000_210,
		FeeSat:           210,
		SettlementType:   swaps.SettlementTypeLightning,
	}
	service := newTestSwapClientService(fakeClient)
	service.chainParams = &chaincfg.RegressionNetParams
	defer service.cancel()

	_, err := service.QuotePay(
		t.Context(), &swapclientrpc.QuotePayRequest{
			Invoice:   invoice,
			MaxFeeSat: 0,
		},
	)
	require.NoError(t, err)
	require.Equal(t, uint64(10_000), fakeClient.quotePayMaxFeeSat)
}

// TestQuotePayWrapsMaxFeeError verifies quote previews surface the same
// actionable fee-cap error as the mutating send path.
func TestQuotePayWrapsMaxFeeError(t *testing.T) {
	t.Parallel()

	payHash := testHash(93)
	invoice := testStartPayInvoice(t, payHash, 1_000_000)
	feeErr := fmt.Errorf("quote in-swap: invalid in-swap request: " +
		"in-swap fee 250 sat exceeds max fee 100 sat")
	fakeClient := newFakeSwapRuntime()
	fakeClient.quotePayErr = feeErr
	service := newTestSwapClientService(fakeClient)
	service.chainParams = &chaincfg.RegressionNetParams
	defer service.cancel()

	_, err := service.QuotePay(
		t.Context(), &swapclientrpc.QuotePayRequest{
			Invoice:   invoice,
			MaxFeeSat: 100,
		},
	)
	require.ErrorContains(t, err, "max-fee cap of 100 sat")
	require.ErrorContains(t, err, "--max_fee")
	require.ErrorIs(t, err, feeErr)
}

// TestWrapInSwapFeeError verifies the fee-cap rejection is rewritten into an
// actionable message while unrelated errors pass through untouched.
func TestWrapInSwapFeeError(t *testing.T) {
	t.Parallel()

	feeErr := fmt.Errorf("create in-swap: invalid in-swap request: " +
		"in-swap fee 1 sat exceeds max fee 0 sat")
	wrapped := wrapInSwapFeeError(feeErr, 0)
	require.ErrorContains(t, wrapped, "max-fee cap of 0 sat")
	require.ErrorContains(t, wrapped, "--max_fee")
	require.ErrorIs(t, wrapped, feeErr)

	other := errors.New("boom")
	require.Equal(t, other, wrapInSwapFeeError(other, 5))

	require.NoError(t, wrapInSwapFeeError(nil, 5))
}
