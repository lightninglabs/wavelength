package wavewalletdk

import (
	"context"
	"testing"

	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// creditCapWalletClient records PrepareSend requests from the SDK facade.
type creditCapWalletClient struct {
	wavewalletrpc.WalletServiceClient

	lastPrepare *wavewalletrpc.PrepareSendRequest
}

// PrepareSend records the request and returns a minimal prepared response.
func (c *creditCapWalletClient) PrepareSend(_ context.Context,
	req *wavewalletrpc.PrepareSendRequest, _ ...grpc.CallOption) (
	*wavewalletrpc.PrepareSendResponse, error) {

	c.lastPrepare = req

	return &wavewalletrpc.PrepareSendResponse{
		SendIntentId: "intent",
	}, nil
}

// TestPrepareSendForwardsIndependentCreditCaps verifies host and mobile
// callers can set both limits through the stable SDK DTO.
func TestPrepareSendForwardsIndependentCreditCaps(t *testing.T) {
	t.Parallel()

	wallet := &creditCapWalletClient{}
	client := newExitTestClient(wallet)

	_, err := client.PrepareSend(t.Context(), PrepareSendRequest{
		Invoice:           "lnbcrt1example",
		MaxCreditSat:      500,
		MaxCreditTopupSat: 1_000,
	})
	require.NoError(t, err)
	require.NotNil(t, wallet.lastPrepare)
	require.Equal(t, uint64(500), wallet.lastPrepare.GetMaxCreditSat())
	require.Equal(
		t, uint64(1_000), wallet.lastPrepare.GetMaxCreditTopupSat(),
	)
}
