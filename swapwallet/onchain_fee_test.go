//go:build wavewalletrpc && swapruntime

package swapwallet

import (
	"errors"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestRouterOnchainSweepQuotesEveryInput prevents a multi-input sweep from
// presenting a single-input fee as the cost of the entire leave.
func TestRouterOnchainSweepQuotesEveryInput(t *testing.T) {
	t.Parallel()

	r, _, rpc := newRouterFixture(t)
	rpc.getInfoResp = &waverpc.GetInfoResponse{BlockHeight: 1000}
	rpc.listVTXOsResp = &waverpc.ListVTXOsResponse{}
	for i := range 6 {
		rpc.listVTXOsResp.Vtxos = append(
			rpc.listVTXOsResp.Vtxos, &waverpc.VTXO{
				Outpoint:    fmt.Sprintf("tx%d:0", i),
				AmountSat:   46000,
				BatchExpiry: 1200,
			},
		)
	}
	rpc.listVTXOsResp.Vtxos[0].AmountSat = 49681
	rpc.listVTXOsResp.Vtxos[0].BatchExpiry = 1300
	rpc.estimateFeeFn = func(req *waverpc.EstimateFeeRequest) (
		*waverpc.EstimateFeeResponse, error) {

		require.False(t, req.GetIsBoarding())
		if req.GetAmountSat() == 49681 {
			require.Equal(t, uint32(300), req.GetRemainingBlocks())

			return &waverpc.EstimateFeeResponse{
				TotalFeeSat: 382,
			}, nil
		}

		require.Equal(t, int64(46000), req.GetAmountSat())
		require.Equal(t, uint32(200), req.GetRemainingBlocks())

		return &waverpc.EstimateFeeResponse{TotalFeeSat: 300}, nil
	}

	resp, err := r.PrepareSend(
		t.Context(), &wavewalletrpc.PrepareSendRequest{
			Destination: &wavewalletrpc.
				PrepareSendRequest_OnchainAddress{
				OnchainAddress: "bcrt1qaddr",
			},
			SweepAll: true,
		},
	)
	require.NoError(t, err)
	require.Len(t, resp.GetSelectedOutpoints(), 6)
	require.Equal(t, int64(279681), resp.GetExpectedTotalOutflowSat())
	require.Equal(t, int64(1882), resp.GetExpectedFeeSat())
	require.True(t, resp.GetFeeKnown())
	require.Equal(
		t, wavewalletrpc.SendQuoteStatus_SEND_QUOTE_STATUS_COMPLETE,
		resp.GetQuoteStatus(),
	)
	require.Empty(t, resp.GetWarning())

	// Identical inputs reuse a request, but each contributes its fee.
	require.Equal(t, 2, rpc.estimateFeeCalls)
}

// TestRouterOnchainRejectsUnfundedInputFees ensures a bounded preview checks
// affordability against all input fees before creating a send intent.
func TestRouterOnchainRejectsUnfundedInputFees(t *testing.T) {
	t.Parallel()

	r, _, rpc := newRouterFixture(t)
	rpc.getInfoResp = &waverpc.GetInfoResponse{BlockHeight: 1000}
	rpc.listVTXOsResp = &waverpc.ListVTXOsResponse{
		Vtxos: []*waverpc.VTXO{
			{
				Outpoint:    "tx1:0",
				AmountSat:   6000,
				BatchExpiry: 1200,
			},
			{
				Outpoint:    "tx2:0",
				AmountSat:   5000,
				BatchExpiry: 1200,
			},
		},
	}
	rpc.estimateFeeResp = &waverpc.EstimateFeeResponse{TotalFeeSat: 600}

	resp, err := r.PrepareSend(
		t.Context(), &wavewalletrpc.PrepareSendRequest{
			Destination: &wavewalletrpc.
				PrepareSendRequest_OnchainAddress{
				OnchainAddress: "bcrt1qaddr",
			},
			AmtSat: 10000,
		},
	)
	require.ErrorIs(t, err, ErrAmountRequired)
	require.Nil(t, resp)
	require.Equal(t, 2, rpc.estimateFeeCalls)
}

// TestOnchainFeeQuoteCompleteness checks lifetime-sensitive quote reuse,
// zero-fee schedules, and all-or-nothing fallback for incomplete estimates.
func TestOnchainFeeQuoteCompleteness(t *testing.T) {
	t.Parallel()
	complete := wavewalletrpc.SendQuoteStatus_SEND_QUOTE_STATUS_COMPLETE

	tests := []struct {
		name          string
		height        uint32
		expiries      []int32
		fees          []int64
		nilReply      bool
		failLast      bool
		wantCalls     int
		wantRemaining []uint32
		wantFee       int64
		wantKnown     bool
	}{
		{
			name:   "different lifetimes are quoted separately",
			height: 1000,
			expiries: []int32{
				1200,
				1300,
			},
			fees: []int64{
				200,
				300,
			},
			wantCalls: 2,
			wantRemaining: []uint32{
				200,
				300,
			},
			wantFee:   500,
			wantKnown: true,
		},
		{
			name:   "expired inputs never request default lifetime",
			height: 1000,
			expiries: []int32{
				999,
				1000,
				1001,
			},
			fees: []int64{
				200,
			},
			wantCalls: 1,
			wantRemaining: []uint32{
				1,
			},
			wantFee:   600,
			wantKnown: true,
		},
		{
			name:   "zero fee is a valid quote",
			height: 1000,
			expiries: []int32{
				1200,
			},
			fees: []int64{
				0,
			},
			wantCalls: 1,
			wantKnown: true,
		},
		{
			name: "missing height cannot produce complete quote",
			expiries: []int32{
				1200,
			},
			wantFee: 161,
		},
		{
			name:   "missing expiry cannot produce complete quote",
			height: 1000,
			expiries: []int32{
				0,
			},
			wantFee: 161,
		},
		{
			name:   "missing later expiry discards partial sum",
			height: 1000,
			expiries: []int32{
				1200,
				0,
			},
			fees: []int64{
				500,
			},
			wantCalls: 1,
			wantFee:   219,
		},
		{
			name:   "operator failure discards partial sum",
			height: 1000,
			expiries: []int32{
				1200,
				1300,
			},
			fees: []int64{
				500,
				500,
			},
			failLast:  true,
			wantCalls: 2,
			wantFee:   219,
		},
		{
			name:   "nil response falls back",
			height: 1000,
			expiries: []int32{
				1200,
			},
			nilReply:  true,
			wantCalls: 1,
			wantFee:   161,
		},
		{
			name:   "negative fee falls back",
			height: 1000,
			expiries: []int32{
				1200,
			},
			fees: []int64{
				-1,
			},
			wantCalls: 1,
			wantFee:   161,
		},
		{
			name:   "excessive fee falls back",
			height: 1000,
			expiries: []int32{
				1200,
			},
			fees: []int64{
				btcutil.MaxSatoshi + 1,
			},
			wantCalls: 1,
			wantFee:   161,
		},
		{
			name:   "excessive sum with reused quote",
			height: 1000,
			expiries: []int32{
				1200,
				1200,
			},
			fees: []int64{
				btcutil.MaxSatoshi,
			},
			wantCalls: 1,
			wantFee:   219,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r, _, rpc := newRouterFixture(t)
			var inputs []*waverpc.VTXO
			for _, expiry := range tt.expiries {
				inputs = append(inputs, &waverpc.VTXO{
					AmountSat:   10000,
					BatchExpiry: expiry,
				})
			}
			var remaining []uint32
			rpc.estimateFeeFn = func(
				req *waverpc.EstimateFeeRequest) (
				*waverpc.EstimateFeeResponse, error) {

				remaining = append(
					remaining, req.GetRemainingBlocks(),
				)
				if tt.nilReply {
					return nil, nil
				}
				if tt.failLast &&
					len(remaining) == tt.wantCalls {
					return nil, errors.New("operator " +
						"unavailable")
				}

				return &waverpc.EstimateFeeResponse{
					TotalFeeSat: tt.fees[len(remaining)-1],
				}, nil
			}

			quote, err := r.estimateOnchainFee(
				t.Context(), inputs, true, onchainTerms{
					blockHeight: tt.height,
					feeRate:     1,
				},
			)
			require.NoError(t, err)
			require.Equal(t, tt.wantFee, quote.feeSat)
			require.Equal(t, tt.wantKnown, quote.feeKnown)
			require.Equal(t, tt.wantCalls, rpc.estimateFeeCalls)
			if tt.wantRemaining != nil {
				require.Equal(t, tt.wantRemaining, remaining)
			}
			wantStatus := wavewalletrpc.
				SendQuoteStatus_SEND_QUOTE_STATUS_LOCAL_ONLY
			if tt.wantKnown {
				wantStatus = complete
			}
			require.Equal(t, wantStatus, quote.quoteStatus)
			if tt.wantKnown {
				require.Empty(t, quote.warning)

				return
			}

			require.Contains(
				t, quote.warning, "per-input operator quote",
			)
		})
	}
}

// TestRouterOnchainRejectsUneconomicInput prevents an explicit operator
// warning from being replaced by the cheaper local fallback estimate.
func TestRouterOnchainRejectsUneconomicInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		sweepAll bool
		fee      int64
	}{
		{
			sweepAll: false,
			fee:      600,
		},
		{
			sweepAll: false,
			fee:      1200,
		},
		{
			sweepAll: true,
			fee:      600,
		},
		{
			sweepAll: true,
			fee:      1200,
		},
	}
	for _, tt := range tests {
		name := fmt.Sprintf("sweep=%t/fee=%d", tt.sweepAll, tt.fee)
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			r, _, rpc := newRouterFixture(t)
			rpc.getInfoResp = &waverpc.GetInfoResponse{
				BlockHeight: 1000,
				ServerInfo: &waverpc.ServerInfo{
					FeeRate: 1,
				},
			}
			rpc.listVTXOsResp = &waverpc.ListVTXOsResponse{
				Vtxos: []*waverpc.VTXO{
					{
						Outpoint:    "large:0",
						AmountSat:   10000,
						BatchExpiry: 1200,
					},
					{
						Outpoint:    "small:0",
						AmountSat:   1000,
						BatchExpiry: 1200,
					},
				},
			}
			rpc.estimateFeeFn = func(
				req *waverpc.EstimateFeeRequest) (
				*waverpc.EstimateFeeResponse, error) {

				if req.GetAmountSat() == 10000 {
					return &waverpc.EstimateFeeResponse{
						TotalFeeSat: 100,
					}, nil
				}

				return &waverpc.EstimateFeeResponse{
					TotalFeeSat:      tt.fee,
					BelowDustWarning: true,
				}, nil
			}
			req := &wavewalletrpc.PrepareSendRequest{
				Destination: &wavewalletrpc.
					PrepareSendRequest_OnchainAddress{
					OnchainAddress: "bcrt1qaddr",
				},
				SweepAll: tt.sweepAll,
			}
			if !tt.sweepAll {
				req.AmtSat = 10001
			}

			resp, err := r.PrepareSend(t.Context(), req)
			require.Equal(
				t, codes.FailedPrecondition, status.Code(err),
			)
			require.ErrorContains(t, err, "small:0")
			require.ErrorContains(t, err, "uneconomic")
			require.Nil(t, resp)
			require.Empty(t, r.intents.intents)
			require.Equal(t, 2, rpc.estimateFeeCalls)
		})
	}
}

// TestRouterOnchainSweepOutputFloor checks the destination amount after all
// fees, even when the operator does not flag individual inputs as uneconomic.
func TestRouterOnchainSweepOutputFloor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		amount     int64
		fee        int64
		dust       uint64
		local      bool
		wantReject bool
	}{
		{
			name:       "fee exceeds balance",
			amount:     1000,
			fee:        1200,
			wantReject: true,
		},
		{
			name:       "fee consumes balance",
			amount:     1000,
			fee:        1000,
			wantReject: true,
		},
		{
			name:       "below advertised floor",
			amount:     1000,
			fee:        671,
			dust:       330,
			wantReject: true,
		},
		{
			name:   "at advertised floor",
			amount: 1000,
			fee:    670,
			dust:   330,
		},
		{
			name:   "zero fee",
			amount: 330,
			dust:   330,
		},
		{
			name:       "local below floor",
			amount:     490,
			dust:       330,
			local:      true,
			wantReject: true,
		},
		{
			name:   "local at floor",
			amount: 491,
			dust:   330,
			local:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r, _, rpc := newRouterFixture(t)
			rpc.getInfoResp = &waverpc.GetInfoResponse{
				BlockHeight: 1000,
				ServerInfo: &waverpc.ServerInfo{
					FeeRate:   1,
					DustLimit: tt.dust,
				},
			}
			rpc.listVTXOsResp = &waverpc.ListVTXOsResponse{
				Vtxos: []*waverpc.VTXO{
					{
						Outpoint:    "tx1:0",
						AmountSat:   tt.amount,
						BatchExpiry: 1200,
					},
				},
			}
			rpc.estimateFeeResp = &waverpc.EstimateFeeResponse{
				TotalFeeSat: tt.fee,
			}
			if tt.local {
				rpc.estimateFeeErr = errors.New("operator " +
					"unavailable")
			}

			dst := &wavewalletrpc.PrepareSendRequest_OnchainAddress{
				OnchainAddress: "bcrt1qaddr",
			}
			resp, err := r.PrepareSend(
				t.Context(), &wavewalletrpc.PrepareSendRequest{
					Destination: dst,
					SweepAll:    true,
				},
			)
			if tt.wantReject {
				require.Equal(
					t, codes.FailedPrecondition,
					status.Code(err),
				)
				require.ErrorContains(t, err, "after fees")
				require.Nil(t, resp)
				require.Empty(t, r.intents.intents)

				return
			}

			require.NoError(t, err)
			require.NotEmpty(t, resp.GetSendIntentId())
			require.Equal(t, !tt.local, resp.GetFeeKnown())
		})
	}
}
