package wavewalletdk

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightninglabs/wavelength/rpc/swapclientrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestMaximumInvoiceAmount checks zero-change drains, fee rounding, credit
// rounding, and a legacy exact fit against the actual shared coin selector.
func TestMaximumInvoiceAmount(t *testing.T) {
	minimumError := status.Error(
		codes.InvalidArgument, swapclientrpc.PayBelowMinimumHint,
	)
	for _, test := range []struct {
		name   string
		coins  []btcutil.Amount
		floor  btcutil.Amount
		fee    func(uint64) uint64
		credit uint64
		cap    uint64
		topup  uint64
		want   uint64
	}{
		{name: "exact drain", coins: []btcutil.Amount{
			1500,
		},
			floor: 1000, fee: func(uint64) uint64 { return 6 },
			want: 1494},
		{name: "in Ark", coins: []btcutil.Amount{
			1500,
			2000,
		},
			floor: 1000, fee: func(uint64) uint64 { return 0 },
			want: 3500},
		{name: "fee step leaves dust", coins: []btcutil.Amount{
			2524,
		},
			floor: 1000, fee: func(a uint64) uint64 {
				return a / 100
			}, want: 1509},
		{name: "legacy exact prefix", coins: []btcutil.Amount{
			2474,
			50,
		},
			floor: 1000, fee: func(a uint64) uint64 {
				return a / 100
			}, want: 2450},
		{name: "credit contributes", coins: []btcutil.Amount{
			1500,
		},
			floor: 1000, fee: func(uint64) uint64 { return 6 },
			credit: 500, cap: 500, want: 1994},
		{name: "credit cap", coins: []btcutil.Amount{
			1500,
		},
			floor: 1000, fee: func(uint64) uint64 { return 6 },
			credit: 500, cap: 100, want: 1594},
		{name: "credit only", floor: 1000,
			fee:    func(uint64) uint64 { return 6 },
			credit: 500, cap: 1000, want: 494},
		{name: "credit only variable fee", floor: 1000,
			fee: func(a uint64) uint64 {
				return 2 + (a+199)/200
			},
			credit: 500, cap: 1000, want: 495},
		{name: "credit only variable fee at cap", floor: 1000,
			fee: func(a uint64) uint64 {
				return 2 + (a+199)/200
			},
			credit: 500, cap: 500, want: 495},
		{name: "rounded credit topup", coins: []btcutil.Amount{
			1000,
		},
			floor: 1000, fee: func(uint64) uint64 { return 6 },
			cap: 1000, topup: 1000, want: 994},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := PrepareSendRequest{
				MaxCreditSat:      test.cap,
				MaxCreditTopupSat: test.topup,
			}
			quote := func(_ context.Context, a uint64) (
				*swapclientrpc.QuotePayResponse, error) {

				if test.cap == 0 && a < uint64(test.floor) {
					return nil, minimumError
				}
				fee := test.fee(a)
				debit := a + fee
				applied := min(
					debit, min(test.credit, test.cap),
				)
				debit -= applied
				q := &swapclientrpc.QuotePayResponse{
					InvoiceAmountSat: a,
					FeeSat:           fee,
					AmountSat:        debit,
					CreditQuote: &swapclientrpc.CreditQuote{
						CreditAppliedSat: applied,
					},
				}
				if test.cap > 0 && debit > 0 && debit < uint64(
					test.floor,
				) {

					q.AmountSat = 0
					q.CreditQuote.CreditShortfallSat = debit
					q.CreditQuote.CreditTopupSat = max(
						debit, uint64(test.floor),
					)
				}

				cp := q.CreditQuote
				required := applied + cp.CreditShortfallSat
				if required > test.cap {
					return nil, fmt.Errorf("%s %d",
						swapclientrpc.PayCreditCapHint,
						test.cap)
				}

				return q, nil
			}
			amount, q, err := maximumInvoiceAmount(
				t.Context(), test.coins, test.floor, req, quote,
			)
			require.NoError(t, err)
			require.Equal(t, test.want, amount)
			require.True(
				t, maxQuoteFundable(
					test.coins, test.floor, req, q,
				),
			)
			// Exhaustive oracle proves this is the largest
			// affordable, selectable amount, including the invalid
			// dust interval.
			var total uint64
			for _, coin := range test.coins {
				total += uint64(coin)
			}
			upper := total + test.cap
			for larger := amount + 1; larger <= upper; larger++ {
				other, err := quote(t.Context(), larger)
				if err != nil && maxAmountPolicyRejection(err) {
					continue
				}
				require.NoError(t, err)
				require.False(
					t, maxQuoteFundable(
						test.coins, test.floor, req,
						other,
					),
					"larger amount %d is fundable",
					larger,
				)
			}
		})
	}
}

// TestMaximumInvoiceAmountFailsClosed ensures a missing quote or cancellation
// never becomes a guessed payment amount or a funding operation.
func TestMaximumInvoiceAmountFailsClosed(t *testing.T) {
	lookupErr := errors.New("service unavailable")
	_, _, err := maximumInvoiceAmount(
		t.Context(), []btcutil.Amount{1500}, 1000, PrepareSendRequest{},
		func(context.Context, uint64) (*swapclientrpc.QuotePayResponse,
			error) {

			return nil, lookupErr
		},
	)
	require.ErrorIs(t, err, lookupErr)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err = maximumInvoiceAmount(ctx, []btcutil.Amount{1500}, 1000,
		PrepareSendRequest{}, func(context.Context, uint64) (
			*swapclientrpc.QuotePayResponse, error) {

			panic("unexpected RPC")
		})
	require.ErrorIs(t, err, context.Canceled)
}
