package lnruntime

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestRemoteFundingNegotiationErrorClassification verifies only an explicit
// peer rejection can authorize pre-PONR cleanup after a remote call fails.
func TestRemoteFundingNegotiationErrorClassification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		err       error
		ambiguous bool
	}{
		{
			name:      "plain transport error",
			err:       errors.New("mailbox response lost"),
			ambiguous: true,
		},
		{
			name:      "context cancellation",
			err:       context.Canceled,
			ambiguous: true,
		},
		{
			name: "unavailable transport",
			err: status.Error(
				codes.Unavailable, "mailbox unavailable",
			),
			ambiguous: true,
		},
		{
			name: "explicit rejection",
			err: status.Error(
				codes.FailedPrecondition, "channel rejected",
			),
		},
		{
			name: "funding wire rejection",
			err: fmt.Errorf("%w: channel rejected",
				errFundingWireRequestRejected),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := remoteFundingNegotiationError(
				"remote step", test.err,
			)
			require.Equal(
				t, test.ambiguous,
				errors.Is(err, ErrFundingNegotiationAmbiguous),
			)
			require.ErrorContains(t, err, test.err.Error())
		})
	}
}

// TestAwaitNegotiatedPSBTPreservesLocalFailure verifies lnd's terminal local
// funding result is not mistaken for an ambiguous cross-endpoint response.
func TestAwaitNegotiatedPSBTPreservesLocalFailure(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("local lnd rejected funding")
	fundingErrors := make(chan error, 1)
	fundingErrors <- wantErr

	_, err := awaitNegotiatedPSBT(t.Context(), &FundingFlow{
		Updates: make(chan *lnrpc.OpenStatusUpdate),
		Errors:  fundingErrors,
	})
	require.ErrorIs(t, err, wantErr)
	require.NotErrorIs(t, err, ErrFundingNegotiationAmbiguous)
}
