package swaps

import (
	"context"
	"testing"

	"github.com/lightninglabs/darepo-client/swaprpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type testSwapServiceClient struct {
	swaprpc.SwapServiceClient

	authorizeErr error
}

func (c testSwapServiceClient) AuthorizeInSwapRefund(context.Context,
	*swaprpc.AuthorizeInSwapRefundRequest, ...grpc.CallOption) (
	*swaprpc.AuthorizeInSwapRefundResponse, error) {

	return nil, c.authorizeErr
}

// TestAuthorizeInSwapRefundPreservesStatusCode verifies the pay session can
// still distinguish retryable "not ready" authorization responses.
func TestAuthorizeInSwapRefundPreservesStatusCode(t *testing.T) {
	t.Parallel()

	conn := &GRPCSwapServerConn{
		client: testSwapServiceClient{
			authorizeErr: status.Error(
				codes.FailedPrecondition, "refund unavailable",
			),
		},
	}

	_, err := conn.AuthorizeInSwapRefund(
		context.Background(), lntypes.Hash{}, "txid:0", 1, nil, nil,
		nil,
	)
	require.Error(t, err)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
}
