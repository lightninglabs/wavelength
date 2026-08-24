package chainfees

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightninglabs/wavelength/chainsource"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

type backendEstimateStub struct {
	chainsource.ChainBackend

	rate btcutil.Amount
	err  error
}

// EstimateFee returns the configured fee estimate result.
func (b *backendEstimateStub) EstimateFee(context.Context, uint32) (
	btcutil.Amount, error) {

	return b.rate, b.err
}

// TestBackendEstimatorFallback verifies that a fresh or unavailable fee
// backend cannot prevent channel negotiation from using the relay floor.
func TestBackendEstimatorFallback(t *testing.T) {
	t.Parallel()

	relayFee := chainfee.SatPerKWeight(500)
	testCases := []struct {
		name    string
		backend *backendEstimateStub
	}{
		{
			name: "backend unavailable",
			backend: &backendEstimateStub{
				err: errors.New("no fee estimates available"),
			},
		},
		{
			name: "non-positive estimate",
			backend: &backendEstimateStub{
				rate: 0,
			},
		},
	}

	for _, testCase := range testCases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			estimator, err := NewBackendEstimator(
				testCase.backend, relayFee,
			)
			require.NoError(t, err)

			rate, err := estimator.EstimateFeePerKW(6)
			require.NoError(t, err)
			require.Equal(t, relayFee, rate)
		})
	}
}
