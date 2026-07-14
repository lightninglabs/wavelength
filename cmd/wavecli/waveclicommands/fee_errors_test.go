package waveclicommands

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestMapFeeErrorRecognizesServerSentinels verifies that every
// server-side fee rejection pattern the CLI cares about gets
// mapped to a concise actionable message. Matching is substring-
// based because the daemon sanitizes the upstream gRPC status;
// this test locks in the substrings we depend on.
func TestMapFeeErrorRecognizesServerSentinels(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		serverMsg   string
		wantMatch   string
		wantNoMatch bool
	}{
		{
			name: "ErrVTXOBelowMinViable",
			serverMsg: "VTXO amount is below minimum " +
				"viable threshold: amount 1000 " +
				"sats, fee 600 sats",
			wantMatch: "below the operator's minimum viable",
		},
		{
			name: "ErrOperatorFeeTooLow",
			serverMsg: "operator fee is below minimum: " +
				"got 100 sats, required 250 sats " +
				"across 1 boarding inputs",
			wantMatch: "fee schedule changed",
		},
		{
			// The verbatim wallet error format from
			// wallet.go's boarding-balance gate; we match
			// the "too small after operator fee" tail so
			// internal errors that mention "boarding
			// balance" but not this specific gate (e.g.
			// "peek boarding balance: ...") fall through.
			name: "BoardingTooSmall",
			serverMsg: "boarding balance (100) too small " +
				"after operator fee (200)",
			wantMatch: "boarding balance is too small",
		},
		{
			// Internal Board failure that mentions
			// "boarding balance" but isn't the wallet's
			// "too small after operator fee" gate. Must
			// fall through unmatched so the CLI surfaces
			// the raw error rather than rewriting it as
			// the friendly fee-rejection message.
			name: "BoardingPeekInternalFallsThrough",
			serverMsg: "peek boarding balance: " +
				"wallet actor not initialized",
			wantNoMatch: true,
		},
		{
			name:      "EstimateFeeNoConfig",
			serverMsg: "fee calculator not configured",
			wantMatch: "EstimateFee is unavailable",
		},
		{
			name:        "Unrelated",
			serverMsg:   "network unreachable",
			wantNoMatch: true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Simulate an incoming gRPC error with an
			// unwrapped message. mapFeeError works on
			// substring match so the exact status code
			// is irrelevant.
			serverErr := status.Error(
				codes.InvalidArgument, tc.serverMsg,
			)
			mapped := mapFeeError(serverErr)
			if tc.wantNoMatch {
				require.Nil(t, mapped)

				return
			}

			require.NotNil(t, mapped)
			require.Contains(t, mapped.Error(), tc.wantMatch)
			// The original error text is included for
			// context so operators can still grep the
			// server logs.
			require.Contains(t, mapped.Error(), tc.serverMsg)
		})
	}
}

// TestMapFeeErrorNilSafe verifies that a nil input does not
// panic and returns nil.
func TestMapFeeErrorNilSafe(t *testing.T) {
	t.Parallel()

	require.Nil(t, mapFeeError(nil))
	require.Nil(t, mapFeeError(errors.New("unrelated")))
}
