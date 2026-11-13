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

// batchWalletClient fails loudly if ExitBatch falls back to any singular RPC.
type batchWalletClient struct {
	wavewalletrpc.WalletServiceClient
	request  *wavewalletrpc.ExitBatchRequest
	response *wavewalletrpc.ExitBatchResponse
	err      error
	calls    int
}

// ExitBatch captures one request and returns the configured response or error.
func (s *batchWalletClient) ExitBatch(_ context.Context,
	req *wavewalletrpc.ExitBatchRequest, _ ...grpc.CallOption) (
	*wavewalletrpc.ExitBatchResponse, error) {

	s.calls++
	s.request = req

	return s.response, s.err
}

// TestClientExitBatchPreservesResults pins selection, destination, result order
// and the unknown mapping for a future daemon enum value.
func TestClientExitBatchPreservesResults(t *testing.T) {
	t.Parallel()
	accepted := wavewalletrpc.ExitBatchOutcome_EXIT_BATCH_ACCEPTED
	failed := wavewalletrpc.ExitBatchOutcome_EXIT_BATCH_FAILED
	wallet := &batchWalletClient{response: &wavewalletrpc.ExitBatchResponse{
		OnchainAddress: "common-address",
		ActivityId:     "batch-activity",
		Results: []*wavewalletrpc.ExitBatchInputResult{
			{
				Outpoint: "a:0",
				Outcome:  accepted,
			},
			{
				Outpoint: "b:1",
				Outcome:  failed,
				Error:    "missing",
			},
			{
				Outpoint: "c:2",
				Outcome:  wavewalletrpc.ExitBatchOutcome(99),
				Error:    "uncertain",
			},
		},
	}}
	client := newExitTestClient(wallet)
	targets := []string{"a:0", "b:1", "c:2"}
	response, err := client.ExitBatch(t.Context(), ExitBatchRequest{
		Outpoints:   targets,
		Destination: "common-address",
	})
	require.NoError(t, err)
	require.Equal(t, "batch-activity", response.ActivityID)
	require.Equal(t, 1, wallet.calls)
	require.Equal(t, targets, wallet.request.Outpoints)
	require.Equal(t, "common-address", wallet.request.OnchainAddress)
	require.Equal(t, "common-address", response.Destination)
	require.Equal(t, []ExitBatchInputResult{
		{Outpoint: "a:0", Outcome: ExitBatchAccepted},
		{Outpoint: "b:1", Outcome: ExitBatchFailed, Error: "missing"},
		{
			Outpoint: "c:2",
			Outcome:  ExitBatchUnknown,
			Error:    "uncertain",
		},
	}, response.Results)
}

// TestClientExitBatchNeverRetries proves an old server and an ambiguous reply
// both stop after one batch RPC, without singular or unilateral fallback.
func TestClientExitBatchNeverRetries(t *testing.T) {
	t.Parallel()
	for _, code := range []codes.Code{
		codes.Unimplemented,
		codes.DeadlineExceeded,
	} {
		t.Run(code.String(), func(t *testing.T) {
			t.Parallel()
			wallet := &batchWalletClient{
				err: status.Error(code, "RPC failed"),
			}
			client := newExitTestClient(wallet)
			result, err := client.ExitBatch(
				t.Context(), ExitBatchRequest{
					Outpoints: []string{"a:0", "b:1"},
				},
			)
			require.Nil(t, result)
			require.Equal(t, code, status.Code(err))
			require.Equal(t, 1, wallet.calls)
		})
	}
}

// TestClientExitBatchRejectsEmptySelection keeps an empty selection local.
func TestClientExitBatchRejectsEmptySelection(t *testing.T) {
	t.Parallel()
	wallet := &batchWalletClient{}
	_, err := newExitTestClient(wallet).ExitBatch(
		t.Context(), ExitBatchRequest{},
	)
	require.ErrorContains(t, err, "outpoints list is empty")
	require.Zero(t, wallet.calls)
}
