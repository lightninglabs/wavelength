package wavewalletdk

import (
	"context"
	"testing"

	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// previewWalletClient implements only the read-only RPC. Calling an
// address-allocating legacy plan or a spending RPC fails the test.
type previewWalletClient struct {
	// WalletServiceClient leaves every other wallet RPC unimplemented.
	wavewalletrpc.WalletServiceClient

	request *wavewalletrpc.GetExitPlanRequest
	err     error
}

// PreviewUnilateralExit records the requested selection.
func (s *previewWalletClient) PreviewUnilateralExit(_ context.Context,
	req *wavewalletrpc.GetExitPlanRequest, _ ...grpc.CallOption) (
	*wavewalletrpc.PreviewUnilateralExitResponse, error) {

	s.request = req

	return &wavewalletrpc.PreviewUnilateralExitResponse{
		SelectedAmountSat:    20_000,
		AdditionalFundingSat: 10_000,
		Entries: []*wavewalletrpc.UnilateralExitTarget{
			{
				Outpoint:        "a:0",
				AdmissionError:  "uneconomical",
				RoundCommitment: "pending round",
			},
		},
	}, s.err
}

// TestPreviewUnilateralExit checks the target snapshot and read-only behavior
// on servers that lack the new RPC.
func TestPreviewUnilateralExit(t *testing.T) {
	t.Parallel()
	wallet := &previewWalletClient{}
	client := newExitTestClient(wallet)
	req := GetExitPlanRequest{Outpoints: []string{"a:0"}, ConfTarget: 2}
	preview, err := client.PreviewUnilateralExit(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, req.Outpoints, wallet.request.GetOutpoints())
	require.EqualValues(t, 2, wallet.request.GetConfTarget())
	require.EqualValues(t, 20_000, preview.SelectedAmountSat)
	require.EqualValues(t, 10_000, preview.AdditionalFundingSat)
	require.Len(t, preview.Entries, 1)
	require.Equal(t, "uneconomical", preview.Entries[0].AdmissionError)
	require.Equal(t, "pending round", preview.Entries[0].RoundCommitment)

	wallet.err = status.Error(codes.Unimplemented, "unknown method")
	_, err = client.PreviewUnilateralExit(t.Context(), req)
	require.Equal(t, codes.Unimplemented, status.Code(err))
}
