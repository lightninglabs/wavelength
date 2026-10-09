//go:build wavewalletrpc && swapruntime

package swapwallet

import (
	"math"
	"math/bits"
	"testing"
	"time"

	"github.com/lightninglabs/wavelength/rpc/swapclientrpc"
	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// TestPrepareSendCreditCapsDefaultClosed verifies an invoice prepare cannot
// turn the zero-value request into a credit reservation or Ark top-up.
func TestPrepareSendCreditCapsDefaultClosed(t *testing.T) {
	t.Parallel()

	r, swap, _ := newRouterFixture(t)
	invoice, paymentHash := testPreparedInvoice(t, 500, "tiny")
	swap.quotePayResp = creditOnlyQuote(paymentHash, 500, 0, 500, 1_000)

	_, err := r.PrepareSend(
		t.Context(), &wavewalletrpc.PrepareSendRequest{
			Destination: &wavewalletrpc.PrepareSendRequest_Invoice{
				Invoice: invoice,
			},
		},
	)
	require.ErrorContains(t, err, "exceeds max_credit_sat 0")
	require.Zero(t, swap.quotePayLastReq.GetMaxCreditSat())
	require.Zero(t, preparedIntentCount(r))
}

// TestPrepareSendAllowsBoundedRoundedTopup verifies the server may round a
// real shortfall up to its minimum Ark output only when the caller explicitly
// authorizes both the payment's credit requirement and the rounded top-up.
func TestPrepareSendAllowsBoundedRoundedTopup(t *testing.T) {
	t.Parallel()

	r, swap, _ := newRouterFixture(t)
	reg := &fakeCreditRegistry{}
	r.deps.CreditRegistry = reg

	invoice, paymentHash := testPreparedInvoice(t, 500, "tiny")
	swap.quotePayResp = creditOnlyQuote(
		paymentHash, 500, 267, 233, 1_000,
	)

	resp, err := r.PrepareSend(
		t.Context(), &wavewalletrpc.PrepareSendRequest{
			Destination: &wavewalletrpc.PrepareSendRequest_Invoice{
				Invoice: invoice,
			},
			MaxCreditSat:      500,
			MaxCreditTopupSat: 1_000,
		},
	)
	require.NoError(t, err)
	require.Equal(t, uint64(500), swap.quotePayLastReq.GetMaxCreditSat())
	require.Equal(t, int64(1_000), resp.GetExpectedTotalOutflowSat())
	require.Equal(
		t, uint64(1_000), resp.GetCreditPreview().GetCreditTopupSat(),
	)

	_, err = sendPrepared(t, r, resp)
	require.NoError(t, err)
	require.Equal(t, 1, reg.payCalls)
	require.Equal(t, uint64(500), reg.lastPay.MaxCreditSat)
	require.Equal(t, uint64(1_000), reg.lastPay.TopupSat)
}

// TestPrepareSendEarmarksExactCreditRequirement verifies a generous caller
// cap does not suppress auto-redemption beyond the amount the accepted quote
// will actually reserve.
func TestPrepareSendEarmarksExactCreditRequirement(t *testing.T) {
	t.Parallel()

	r, swap, _ := newRouterFixture(t)
	invoice, paymentHash := testPreparedInvoice(t, 500, "tiny")
	swap.quotePayResp = creditOnlyQuote(
		paymentHash, 500, 267, 233, 1_000,
	)

	_, err := r.PrepareSend(
		t.Context(), &wavewalletrpc.PrepareSendRequest{
			Destination: &wavewalletrpc.PrepareSendRequest_Invoice{
				Invoice: invoice,
			},
			MaxCreditSat:      100_000,
			MaxCreditTopupSat: 1_000,
		},
	)
	require.NoError(t, err)
	require.Equal(t, uint64(500), r.intents.earmarkedCreditSat())
}

// TestSendCreditInvoiceIntentRevalidatesCreditCaps verifies the durable
// dispatch boundary rejects a prepared plan that no longer satisfies the
// caller's limits before it can reach the credit registry.
func TestSendCreditInvoiceIntentRevalidatesCreditCaps(t *testing.T) {
	t.Parallel()

	r, swap, _ := newRouterFixture(t)
	reg := &fakeCreditRegistry{}
	r.deps.CreditRegistry = reg

	invoice, _ := testPreparedInvoice(t, 500, "tiny")
	intentID, err := r.intents.put(&preparedSendIntent{
		kind:              preparedSendInvoice,
		invoice:           invoice,
		amountSat:         500,
		maxCreditSat:      500,
		maxCreditTopupSat: 1_000,
		creditPreview: &wavewalletrpc.CreditPreview{
			CreditAppliedSat:   267,
			CreditShortfallSat: 233,
			CreditTopupSat:     1_001,
		},
	})
	require.NoError(t, err)

	_, err = r.Send(
		t.Context(), &wavewalletrpc.SendRequest{
			SendIntentId: intentID,
		},
	)
	require.ErrorContains(t, err, "exceeds max_credit_topup_sat 1000")
	require.Zero(t, reg.payCalls)
	require.Zero(t, swap.startPayCalls)
}

// TestValidateCreditPlanRejectsMalformedOrOverCapPlans pins the independent
// reservation and top-up limits around the server-controlled quote fields.
func TestValidateCreditPlanRejectsMalformedOrOverCapPlans(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		plan         *wavewalletrpc.CreditPreview
		maxCredit    uint64
		maxTopup     uint64
		errorPattern string
	}{
		{
			name: "credit requirement exceeds cap",
			plan: &wavewalletrpc.CreditPreview{
				CreditAppliedSat:   300,
				CreditShortfallSat: 201,
				CreditTopupSat:     201,
			},
			maxCredit:    500,
			maxTopup:     201,
			errorPattern: "exceeds max_credit_sat",
		},
		{
			name: "top-up exceeds cap",
			plan: &wavewalletrpc.CreditPreview{
				CreditShortfallSat: 200,
				CreditTopupSat:     1_000,
			},
			maxCredit:    200,
			maxTopup:     999,
			errorPattern: "exceeds max_credit_topup_sat",
		},
		{
			name: "top-up below shortfall",
			plan: &wavewalletrpc.CreditPreview{
				CreditShortfallSat: 200,
				CreditTopupSat:     199,
			},
			maxCredit:    200,
			maxTopup:     200,
			errorPattern: "below shortfall",
		},
		{
			name: "top-up without shortfall",
			plan: &wavewalletrpc.CreditPreview{
				CreditAppliedSat: 100,
				CreditTopupSat:   1_000,
			},
			maxCredit:    100,
			maxTopup:     1_000,
			errorPattern: "requires a shortfall",
		},
		{
			name: "credit requirement overflows",
			plan: &wavewalletrpc.CreditPreview{
				CreditAppliedSat:   math.MaxUint64,
				CreditShortfallSat: 1,
				CreditTopupSat:     1,
			},
			maxCredit:    math.MaxUint64,
			maxTopup:     1,
			errorPattern: "overflows uint64",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := validateCreditPlan(
				test.plan, test.maxCredit, test.maxTopup,
			)
			require.ErrorContains(t, err, test.errorPattern)
		})
	}
}

// TestCheckedOutflowSatRejectsOverflow verifies a separately bounded top-up
// cannot wrap the headline total or overflow the wallet RPC's signed amount.
func TestCheckedOutflowSatRejectsOverflow(t *testing.T) {
	t.Parallel()

	_, err := checkedOutflowSat(math.MaxInt64, 1)
	require.ErrorContains(t, err, "total outflow exceeds int64 range")

	_, err = checkedOutflowSat(math.MaxUint64, 1)
	require.ErrorContains(t, err, "total outflow exceeds int64 range")
}

// TestPropertyCreditPlanCaps verifies every accepted server plan satisfies
// the caller's reservation and top-up bounds across the uint64 domain.
func TestPropertyCreditPlanCaps(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		appliedSat := rapid.Uint64().Draw(rt, "applied_sat")
		shortfallSat := rapid.Uint64().Draw(rt, "shortfall_sat")
		topupSat := rapid.Uint64().Draw(rt, "topup_sat")
		maxCreditSat := rapid.Uint64().Draw(rt, "max_credit_sat")
		maxTopupSat := rapid.Uint64().Draw(rt, "max_topup_sat")

		requiredCreditSat, carry := bits.Add64(
			appliedSat, shortfallSat, 0,
		)
		addOK := carry == 0
		shapeOK := shortfallSat == 0 && topupSat == 0 ||
			shortfallSat > 0 && topupSat >= shortfallSat
		wantValid := addOK && requiredCreditSat <= maxCreditSat &&
			topupSat <= maxTopupSat && shapeOK

		err := validateCreditPlan(&wavewalletrpc.CreditPreview{
			CreditAppliedSat:   appliedSat,
			CreditShortfallSat: shortfallSat,
			CreditTopupSat:     topupSat,
		}, maxCreditSat, maxTopupSat)
		require.Equal(rt, wantValid, err == nil)
	})
}

// TestPropertyCheckedOutflowSat verifies total wallet outflow is returned
// exactly when it fits the signed RPC field and rejected otherwise.
func TestPropertyCheckedOutflowSat(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		arkFundingSat := rapid.Uint64().Draw(rt, "ark_funding_sat")
		creditTopupSat := rapid.Uint64().Draw(rt, "credit_topup_sat")

		wantOK := arkFundingSat <= math.MaxInt64 &&
			creditTopupSat <= math.MaxInt64-arkFundingSat
		totalSat, err := checkedOutflowSat(
			arkFundingSat, creditTopupSat,
		)
		require.Equal(rt, wantOK, err == nil)
		if wantOK {
			require.Equal(
				rt, int64(arkFundingSat+creditTopupSat),
				totalSat,
			)
		}
	})
}

// creditOnlyQuote builds a credit-only quote with the supplied plan.
func creditOnlyQuote(paymentHash string, invoiceSat, appliedSat, shortfallSat,
	topupSat uint64) *swapclientrpc.QuotePayResponse {

	return &swapclientrpc.QuotePayResponse{
		PaymentHash:      paymentHash,
		InvoiceAmountSat: invoiceSat,
		SettlementType: swapclientrpc.
			SwapSettlementType_SWAP_SETTLEMENT_TYPE_CREDIT,
		CreditQuote: &swapclientrpc.CreditQuote{
			MustUseCredit:      true,
			CreditAppliedSat:   appliedSat,
			CreditShortfallSat: shortfallSat,
			CreditTopupSat:     topupSat,
		},
		ExpiresAtUnix: time.Now().Add(time.Minute).Unix(),
	}
}

// preparedIntentCount returns the number of live prepared intents.
func preparedIntentCount(r *router) int {
	r.intents.mu.Lock()
	defer r.intents.mu.Unlock()

	return len(r.intents.intents)
}
