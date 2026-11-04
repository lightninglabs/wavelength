package wavewalletdk

import (
	"context"
	"testing"

	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// refreshWalletClient records Refresh requests from the SDK facade and
// answers with a canned response.
type refreshWalletClient struct {
	wavewalletrpc.WalletServiceClient

	lastRefresh *wavewalletrpc.RefreshRequest
	resp        *wavewalletrpc.RefreshResponse
}

// Refresh records the request and returns the canned response.
func (c *refreshWalletClient) Refresh(_ context.Context,
	req *wavewalletrpc.RefreshRequest, _ ...grpc.CallOption) (
	*wavewalletrpc.RefreshResponse, error) {

	c.lastRefresh = req

	return c.resp, nil
}

// TestRefreshForwardsSelectionAndDryRun verifies the SDK passes the
// selection and the preview flag through unchanged.
func TestRefreshForwardsSelectionAndDryRun(t *testing.T) {
	t.Parallel()

	wallet := &refreshWalletClient{
		resp: &wavewalletrpc.RefreshResponse{
			QueuedOutpoints: []string{
				"abc:0",
			},
			DryRun: true,
		},
	}
	client := newExitTestClient(wallet)

	result, err := client.Refresh(t.Context(), RefreshRequest{
		Outpoints: []string{"abc:0"},
		DryRun:    true,
	})
	require.NoError(t, err)
	require.Equal(t, []string{"abc:0"}, wallet.lastRefresh.GetOutpoints())
	require.True(t, wallet.lastRefresh.GetDryRun())
	require.Equal(t, []string{"abc:0"}, result.QueuedOutpoints)
	require.True(t, result.DryRun)
	require.Nil(t, result.FeeEstimate)
}

// TestRefreshKeepsFeeTotalPresence verifies a degraded estimate reaches the
// caller with no total rather than a zero one, which a confirmation dialog
// would render as a free refresh, while a priced estimate keeps its number.
func TestRefreshKeepsFeeTotalPresence(t *testing.T) {
	t.Parallel()

	wallet := &refreshWalletClient{
		resp: &wavewalletrpc.RefreshResponse{
			DryRun: true,
			FeeEstimate: &wavewalletrpc.RefreshFeeEstimate{
				EstimateError: "operator unreachable",
			},
		},
	}
	client := newExitTestClient(wallet)

	result, err := client.Refresh(t.Context(), RefreshRequest{
		DryRun: true,
	})
	require.NoError(t, err)
	require.NotNil(t, result.FeeEstimate)
	require.Nil(t, result.FeeEstimate.TotalFeeSat)
	require.Equal(
		t, "operator unreachable", result.FeeEstimate.EstimateError,
	)

	total := int64(1_234)
	wallet.resp.FeeEstimate = &wavewalletrpc.RefreshFeeEstimate{
		EstimatedTotalFeeSat: &total,
		FreeRefreshEligible:  true,
	}
	result, err = client.Refresh(t.Context(), RefreshRequest{
		DryRun: true,
	})
	require.NoError(t, err)
	require.NotNil(t, result.FeeEstimate.TotalFeeSat)
	require.EqualValues(t, 1_234, *result.FeeEstimate.TotalFeeSat)
	require.True(t, result.FeeEstimate.FreeRefreshEligible)
}
