package waved

import (
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
)

// TestEstimateFeeProxiesEveryField asserts that the daemon's
// EstimateFee handler faithfully proxies every field of the
// operator's EstimateFeeResponse. A regression that dropped any
// leg (liquidity / on-chain / margin) or the BelowDustWarning
// boolean would cause the CLI to display a different fee than
// the operator booked, breaking the client↔server amount-match
// invariant the issue calls out under C.4 (2).
func TestEstimateFeeProxiesEveryField(t *testing.T) {
	t.Parallel()

	svc := &fakeArkService{
		response: &arkrpc.EstimateFeeResponse{
			LiquidityFeeSat:     1_234,
			OnchainShareSat:     56,
			MarginSat:           789,
			TotalFeeSat:         2_079,
			EffectiveAnnualRate: 0.0875,
			MinViableAmountSat:  500,
			BelowDustWarning:    true,
		},
	}

	s := &Server{
		serverConn: newBufconnClient(t, svc),
		log:        btclog.Disabled,
	}
	r := &RPCServer{server: s}

	resp, err := r.EstimateFee(
		t.Context(), &waverpc.EstimateFeeRequest{
			AmountSat:       100_000,
			IsBoarding:      false,
			RemainingBlocks: 144,
		},
	)
	require.NoError(t, err)
	require.NotNil(t, resp)

	// Every field in the operator's reply must round-trip to
	// the daemon's reply verbatim. No caching, no
	// re-computation.
	require.Equal(t, int64(1_234), resp.LiquidityFeeSat)
	require.Equal(t, int64(56), resp.OnchainShareSat)
	require.Equal(t, int64(789), resp.MarginSat)
	require.Equal(t, int64(2_079), resp.TotalFeeSat)
	require.InDelta(
		t, 0.0875, resp.EffectiveAnnualRate, 1e-9,
	)
	require.Equal(t, int64(500), resp.MinViableAmountSat)
	require.True(t, resp.BelowDustWarning)

	// And the operator saw the client's request verbatim (no
	// rewriting of amount or remaining-blocks).
	require.NotNil(t, svc.lastRequest)
	require.Equal(t, int64(100_000), svc.lastRequest.AmountSat)
	require.False(t, svc.lastRequest.IsBoarding)
	require.Equal(
		t, uint32(144), svc.lastRequest.RemainingBlocks,
	)
}

// TestEstimateFeeAmountMatchesQuoteOperatorFee verifies that
// the CLI-facing EstimateFee RPC and the internal
// quoteOperatorFee helper return the SAME TotalFeeSat for the
// same inputs. Without this invariant the number the user
// sees in the CLI confirmation prompt would diverge from the
// number the wallet actor applies during boarding / send, and
// the server would silently under- or over-charge.
func TestEstimateFeeAmountMatchesQuoteOperatorFee(t *testing.T) {
	t.Parallel()

	svc := &fakeArkService{
		response: &arkrpc.EstimateFeeResponse{
			LiquidityFeeSat: 250,
			OnchainShareSat: 10,
			MarginSat:       100,
			TotalFeeSat:     360,
		},
	}

	s := &Server{
		serverConn: newBufconnClient(t, svc),
		log:        btclog.Disabled,
	}
	r := &RPCServer{server: s}

	// CLI path.
	cliResp, err := r.EstimateFee(
		t.Context(), &waverpc.EstimateFeeRequest{
			AmountSat:       200_000,
			IsBoarding:      true,
			RemainingBlocks: 0,
		},
	)
	require.NoError(t, err)

	// Internal-wallet-actor path.
	internal, err := s.quoteOperatorFee(
		t.Context(), 200_000, true, 0,
	)
	require.NoError(t, err)

	// Both paths must see the same TotalFeeSat so the CLI
	// confirmation and the actual booking align.
	require.Equal(
		t, cliResp.TotalFeeSat, int64(internal),
		"CLI-visible total must match wallet-used total",
	)
}
