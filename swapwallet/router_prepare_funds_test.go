//go:build wavewalletrpc && swapruntime

package swapwallet

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightninglabs/wavelength/coinselect"
	"github.com/lightninglabs/wavelength/rpc/swapclientrpc"
	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fundableTerms returns operator terms whose minimum-change floor is 1000
// sat, matching the dispatch-side minimum VTXO amount.
func fundableTerms() *waverpc.GetInfoResponse {
	return &waverpc.GetInfoResponse{
		BlockHeight: 1000,
		ServerInfo: &waverpc.ServerInfo{
			DustLimit:        354,
			MinVtxoAmountSat: 1_000,
		},
	}
}

func oneLiveVTXO(amount int64) *waverpc.ListVTXOsResponse {
	return &waverpc.ListVTXOsResponse{
		Vtxos: []*waverpc.VTXO{{
			Outpoint:  "funding:0",
			AmountSat: amount,
		}},
	}
}

func prepareInvoice(t *testing.T, r *router, invoice string) (
	*wavewalletrpc.PrepareSendResponse, error) {

	t.Helper()

	return r.PrepareSend(t.Context(), &wavewalletrpc.PrepareSendRequest{
		Destination: &wavewalletrpc.PrepareSendRequest_Invoice{
			Invoice: invoice,
		},
		MaxFeeSat: 25,
	})
}

// TestRouterPrepareInvoiceInsufficientFunds reproduces issue #1631: a
// fixed-amount invoice whose quoted outflow exceeds the live VTXO balance
// must be rejected at prepare, not after confirmation at dispatch.
func TestRouterPrepareInvoiceInsufficientFunds(t *testing.T) {
	t.Parallel()

	r, swap, rpc := newRouterFixture(t)
	invoice, paymentHash := testPreparedInvoice(t, 2_500, "too big")
	swap.quotePayResp = &swapclientrpc.QuotePayResponse{
		PaymentHash:      paymentHash,
		InvoiceAmountSat: 2_500,
		AmountSat:        2_508,
		FeeSat:           8,
		SettlementType: swapclientrpc.
			SwapSettlementType_SWAP_SETTLEMENT_TYPE_LIGHTNING,
		ExpiresAtUnix: time.Now().Add(time.Minute).Unix(),
	}
	rpc.getInfoResp = fundableTerms()
	rpc.listVTXOsResp = oneLiveVTXO(2_500)

	_, err := prepareInvoice(t, r, invoice)
	require.ErrorIs(t, err, ErrInsufficientFunds)
	require.Contains(t, err.Error(), "2508 sat")
	require.Contains(t, err.Error(), "2500 sat")
}

// TestRouterPrepareInvoiceExactCoverSucceeds is the issue's control row:
// 2492 + 8 = 2500 exactly covers the wallet's one live VTXO.
func TestRouterPrepareInvoiceExactCoverSucceeds(t *testing.T) {
	t.Parallel()

	r, swap, rpc := newRouterFixture(t)
	invoice, paymentHash := testPreparedInvoice(t, 2_492, "exact")
	swap.quotePayResp = &swapclientrpc.QuotePayResponse{
		PaymentHash:      paymentHash,
		InvoiceAmountSat: 2_492,
		AmountSat:        2_500,
		FeeSat:           8,
		SettlementType: swapclientrpc.
			SwapSettlementType_SWAP_SETTLEMENT_TYPE_LIGHTNING,
		ExpiresAtUnix: time.Now().Add(time.Minute).Unix(),
	}
	rpc.getInfoResp = fundableTerms()
	rpc.listVTXOsResp = oneLiveVTXO(2_500)

	resp, err := prepareInvoice(t, r, invoice)
	require.NoError(t, err)
	require.NotEmpty(t, resp.GetSendIntentId())
}

// TestRouterPrepareInvoiceSubMinimumChange covers the issue's third row: a
// covering selection whose 492 sat change is below the 1000 sat minimum
// must also be rejected at prepare (the #1448 dispatch rule, one step
// earlier).
func TestRouterPrepareInvoiceSubMinimumChange(t *testing.T) {
	t.Parallel()

	r, swap, rpc := newRouterFixture(t)
	invoice, paymentHash := testPreparedInvoice(t, 2_000, "dust change")
	swap.quotePayResp = &swapclientrpc.QuotePayResponse{
		PaymentHash:      paymentHash,
		InvoiceAmountSat: 2_000,
		AmountSat:        2_008,
		FeeSat:           8,
		SettlementType: swapclientrpc.
			SwapSettlementType_SWAP_SETTLEMENT_TYPE_LIGHTNING,
		ExpiresAtUnix: time.Now().Add(time.Minute).Unix(),
	}
	rpc.getInfoResp = fundableTerms()
	rpc.listVTXOsResp = oneLiveVTXO(2_500)

	_, err := prepareInvoice(t, r, invoice)
	require.ErrorIs(t, err, ErrInsufficientFunds)
}

// TestRouterPrepareInvoiceCreditOnlySkipsFundsCheck confirms a fully
// credit-funded quote has no wallet legs and prepares fine with an empty
// wallet.
func TestRouterPrepareInvoiceCreditOnlySkipsFundsCheck(t *testing.T) {
	t.Parallel()

	r, swap, rpc := newRouterFixture(t)
	invoice, paymentHash := testPreparedInvoice(t, 500, "credit only")
	swap.quotePayResp = &swapclientrpc.QuotePayResponse{
		PaymentHash:      paymentHash,
		InvoiceAmountSat: 500,
		AmountSat:        0,
		SettlementType: swapclientrpc.
			SwapSettlementType_SWAP_SETTLEMENT_TYPE_CREDIT,
		CreditQuote: &swapclientrpc.CreditQuote{
			MustUseCredit:    true,
			CreditAppliedSat: 500,
		},
		ExpiresAtUnix: time.Now().Add(time.Minute).Unix(),
	}
	rpc.getInfoResp = fundableTerms()

	resp, err := r.PrepareSend(t.Context(),
		&wavewalletrpc.PrepareSendRequest{
			Destination: &wavewalletrpc.PrepareSendRequest_Invoice{
				Invoice: invoice,
			},
			MaxFeeSat:    25,
			MaxCreditSat: 500,
		},
	)
	require.NoError(t, err)
	require.NotEmpty(t, resp.GetSendIntentId())
}

// TestRouterPrepareInvoiceMixedCreditTopup verifies the sequential-leg
// semantics: the credit top-up's change funds the Ark leg, so 300 + 2200
// against one 2500 sat VTXO is fundable.
func TestRouterPrepareInvoiceMixedCreditTopup(t *testing.T) {
	t.Parallel()

	r, swap, rpc := newRouterFixture(t)
	invoice, paymentHash := testPreparedInvoice(t, 2_500, "mixed")
	swap.quotePayResp = &swapclientrpc.QuotePayResponse{
		PaymentHash:      paymentHash,
		InvoiceAmountSat: 2_500,
		AmountSat:        2_200,
		FeeSat:           8,
		SettlementType: swapclientrpc.
			SwapSettlementType_SWAP_SETTLEMENT_TYPE_MIXED,
		CreditQuote: &swapclientrpc.CreditQuote{
			CreditAppliedSat:   308,
			CreditShortfallSat: 300,
			CreditTopupSat:     300,
		},
		ExpiresAtUnix: time.Now().Add(time.Minute).Unix(),
	}
	rpc.getInfoResp = fundableTerms()
	rpc.listVTXOsResp = oneLiveVTXO(2_500)

	resp, err := r.PrepareSend(t.Context(),
		&wavewalletrpc.PrepareSendRequest{
			Destination: &wavewalletrpc.PrepareSendRequest_Invoice{
				Invoice: invoice,
			},
			MaxFeeSat:         25,
			MaxCreditSat:      608,
			MaxCreditTopupSat: 300,
		},
	)
	require.NoError(t, err)
	require.NotEmpty(t, resp.GetSendIntentId())
}

// TestRouterPrepareInvoiceLocalOnlySkipsFundsCheck confirms that when the
// operator quote is unavailable the local fallback preview still prepares
// with a warning; the fundability of an unsized outflow cannot be checked.
func TestRouterPrepareInvoiceLocalOnlySkipsFundsCheck(t *testing.T) {
	t.Parallel()

	r, swap, rpc := newRouterFixture(t)
	invoice, _ := testPreparedInvoice(t, 2_500, "offline")
	swap.quotePayErr = status.Error(codes.Unimplemented, "no quote")
	rpc.getInfoResp = fundableTerms()

	resp, err := prepareInvoice(t, r, invoice)
	require.NoError(t, err)
	require.NotEmpty(t, resp.GetSendIntentId())
	require.Equal(
		t, wavewalletrpc.SendQuoteStatus_SEND_QUOTE_STATUS_LOCAL_ONLY,
		resp.GetQuoteStatus(),
	)
	require.NotEmpty(t, resp.GetWarning())
}

// TestSelectSequentialLegs pins the shared helper's change-carryover and
// minimum-change semantics independently of the router.
func TestSelectSequentialLegs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		coins     []int64
		minChange int64
		targets   []int64
		wantErr   error
	}{
		{
			name:    "no coins is a shortfall",
			coins:   nil,
			targets: []int64{1_000},
			wantErr: coinselect.ErrNoCandidates,
		},
		{
			name:    "single leg exact fit",
			coins:   []int64{2_500},
			targets: []int64{2_500},
		},
		{
			name:    "single leg shortfall",
			coins:   []int64{2_500},
			targets: []int64{2_508},
			wantErr: coinselect.ErrSelectionShortfall,
		},
		{
			name:      "change below minimum rejected",
			coins:     []int64{2_500},
			minChange: 1_000,
			targets:   []int64{2_008},
			wantErr:   coinselect.ErrChangeBelowMin,
		},
		{
			name:      "change at minimum accepted",
			coins:     []int64{3_000},
			minChange: 1_000,
			targets:   []int64{2_000},
		},
		{
			name:      "top-up change funds the pay leg",
			coins:     []int64{2_500},
			minChange: 1_000,
			targets:   []int64{300, 2_200},
		},
		{
			name:      "second leg shortfall",
			coins:     []int64{2_500},
			minChange: 1_000,
			targets:   []int64{300, 2_300},
			wantErr:   coinselect.ErrSelectionShortfall,
		},
		{
			name:    "zero targets skipped",
			coins:   nil,
			targets: []int64{0, 0},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			coins := make([]btcutil.Amount, len(tc.coins))
			for i, c := range tc.coins {
				coins[i] = btcutil.Amount(c)
			}
			targets := make([]btcutil.Amount, len(tc.targets))
			for i, tgt := range tc.targets {
				targets[i] = btcutil.Amount(tgt)
			}

			err := coinselect.SelectSequential(
				coins, btcutil.Amount(tc.minChange), targets...,
			)
			if tc.wantErr == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, tc.wantErr)
			}
		})
	}
}
