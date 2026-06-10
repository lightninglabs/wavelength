package swaps

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
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
	ackErr       error
	lastAckReq   *swaprpc.AcknowledgeOutSwapHtlcRequest
}

func (c *testSwapServiceClient) AuthorizeInSwapRefund(context.Context,
	*swaprpc.AuthorizeInSwapRefundRequest, ...grpc.CallOption) (
	*swaprpc.AuthorizeInSwapRefundResponse, error) {

	return nil, c.authorizeErr
}

func (c *testSwapServiceClient) AcknowledgeOutSwapHtlc(_ context.Context,
	req *swaprpc.AcknowledgeOutSwapHtlcRequest, _ ...grpc.CallOption) (
	*swaprpc.AcknowledgeOutSwapHtlcResponse, error) {

	c.lastAckReq = req

	return nil, c.ackErr
}

// TestAuthorizeInSwapRefundPreservesStatusCode verifies the pay session can
// still distinguish retryable "not ready" authorization responses.
func TestAuthorizeInSwapRefundPreservesStatusCode(t *testing.T) {
	t.Parallel()

	conn := &GRPCSwapServerConn{
		client: &testSwapServiceClient{
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

// TestAcknowledgeOutSwapHTLCPreservesStatusCode verifies the receive session
// can distinguish retryable or terminal server ACK failures by their original
// gRPC status code.
func TestAcknowledgeOutSwapHTLCPreservesStatusCode(t *testing.T) {
	t.Parallel()

	client := &testSwapServiceClient{
		ackErr: status.Error(codes.FailedPrecondition, "not ready"),
	}
	conn := &GRPCSwapServerConn{
		client: client,
	}

	pubkey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	hash := lntypes.Hash{1, 2, 3}
	err = conn.AcknowledgeOutSwapHTLC(
		context.Background(), hash, pubkey.PubKey(),
	)
	require.Error(t, err)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.Equal(t, hash[:], client.lastAckReq.GetPaymentHash())
	require.Equal(
		t, pubkey.PubKey().SerializeCompressed(),
		client.lastAckReq.GetClientVhtlcPubkey(),
	)
}

// TestAcknowledgeOutSwapHTLCRejectsMissingPubkey verifies malformed local
// state is rejected before an invalid request can reach the swap server.
func TestAcknowledgeOutSwapHTLCRejectsMissingPubkey(t *testing.T) {
	t.Parallel()

	client := &testSwapServiceClient{}
	conn := &GRPCSwapServerConn{
		client: client,
	}

	err := conn.AcknowledgeOutSwapHTLC(
		context.Background(), lntypes.Hash{}, nil,
	)
	require.ErrorContains(t, err, "vHTLC pubkey must be provided")
	require.Nil(t, client.lastAckReq)
}
