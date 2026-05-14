package chainfees

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

func TestDefaultMempoolSpaceURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		params *chaincfg.Params
		want   string
	}{
		{
			name:   "mainnet",
			params: &chaincfg.MainNetParams,
			want:   mempoolSpaceMainnetURL,
		},
		{
			name:   "testnet3",
			params: &chaincfg.TestNet3Params,
			want:   mempoolSpaceTestnetURL,
		},
		{
			name:   "testnet4",
			params: &chaincfg.TestNet4Params,
			want:   mempoolSpaceTestnet4URL,
		},
		{
			name:   "signet",
			params: &chaincfg.SigNetParams,
			want:   mempoolSpaceSignetURL,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := DefaultMempoolSpaceURL(tc.params)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestMempoolSpaceEstimatorMapsTargets(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			_, err := fmt.Fprint(w, `{
				"fastestFee": 8,
				"halfHourFee": 6,
				"hourFee": 4,
				"economyFee": 2,
				"minimumFee": 1
			}`)
			require.NoError(t, err)
		},
	))
	defer server.Close()

	estimator, err := NewMempoolSpaceEstimator(MempoolSpaceConfig{
		URL: server.URL,
	})
	require.NoError(t, err)

	tests := []struct {
		name   string
		target uint32
		want   chainfee.SatPerKWeight
	}{
		{
			name:   "fastest",
			target: 1,
			want:   chainfee.SatPerKWeight(2_000),
		},
		{
			name:   "half hour",
			target: 3,
			want:   chainfee.SatPerKWeight(1_500),
		},
		{
			name:   "hour",
			target: 6,
			want:   chainfee.SatPerKWeight(1_000),
		},
		{
			name:   "economy",
			target: 12,
			want:   chainfee.SatPerKWeight(500),
		},
		{
			name:   "minimum",
			target: 1_008,
			want:   chainfee.FeePerKwFloor,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, err := estimator.EstimateFeePerKW(tc.target)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestMempoolSpaceEstimatorRejectsNonPositiveRates(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			_, err := fmt.Fprint(w, `{
				"fastestFee": 0,
				"halfHourFee": 0,
				"hourFee": 0,
				"economyFee": 0,
				"minimumFee": 0
			}`)
			require.NoError(t, err)
		},
	))
	defer server.Close()

	estimator, err := NewMempoolSpaceEstimator(MempoolSpaceConfig{
		URL: server.URL,
	})
	require.NoError(t, err)

	_, err = estimator.EstimateFeePerKW(6)
	require.ErrorContains(t, err, "non-positive fee rate")
}

func TestMempoolSpaceEstimatorRejectsExcessiveRates(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			_, err := fmt.Fprintf(w, `{
				"fastestFee": %d,
				"halfHourFee": %d,
				"hourFee": %d,
				"economyFee": %d,
				"minimumFee": %d
			}`, maxMempoolSpaceSatPerVByte+1,
				maxMempoolSpaceSatPerVByte+1,
				maxMempoolSpaceSatPerVByte+1,
				maxMempoolSpaceSatPerVByte+1,
				maxMempoolSpaceSatPerVByte+1)
			require.NoError(t, err)
		},
	))
	defer server.Close()

	estimator, err := NewMempoolSpaceEstimator(MempoolSpaceConfig{
		URL: server.URL,
	})
	require.NoError(t, err)

	_, err = estimator.EstimateFeePerKW(6)
	require.ErrorContains(t, err, "excessive fee rate")
}

func TestMempoolSpaceEstimatorCachesRecommendedFees(t *testing.T) {
	t.Parallel()

	var requests atomic.Uint32
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			count := requests.Add(1)
			_, err := fmt.Fprintf(w, `{
				"fastestFee": %d,
				"halfHourFee": %d,
				"hourFee": %d,
				"economyFee": %d,
				"minimumFee": %d
			}`, count, count, count, count, count)
			require.NoError(t, err)
		},
	))
	defer server.Close()

	estimator, err := NewMempoolSpaceEstimator(MempoolSpaceConfig{
		URL:      server.URL,
		CacheTTL: time.Minute,
	})
	require.NoError(t, err)

	now := time.Unix(100, 0)
	estimator.now = func() time.Time {
		return now
	}

	first, err := estimator.EstimateFeePerKW(6)
	require.NoError(t, err)
	second, err := estimator.EstimateFeePerKW(6)
	require.NoError(t, err)
	require.Equal(t, first, second)
	require.Equal(t, uint32(1), requests.Load())

	now = now.Add(time.Minute)
	third, err := estimator.EstimateFeePerKW(6)
	require.NoError(t, err)
	require.Greater(t, third, second)
	require.Equal(t, uint32(2), requests.Load())
}
