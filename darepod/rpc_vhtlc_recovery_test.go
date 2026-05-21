package darepod

import (
	"context"
	"testing"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/vhtlcrecovery/coordinator"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestArmVHTLCRecoveryRequiresRequestID verifies that the RPC boundary rejects
// a missing idempotency key before the request can reach durable storage.
func TestArmVHTLCRecoveryRequiresRequestID(t *testing.T) {
	t.Parallel()

	walletReady := make(chan struct{})
	close(walletReady)

	rpcServer := &RPCServer{
		server: &Server{
			walletReady:   walletReady,
			vhtlcRecovery: &coordinator.Service{},
		},
	}

	_, err := rpcServer.ArmVHTLCRecovery(
		context.Background(), &daemonrpc.ArmVHTLCRecoveryRequest{},
	)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, status.Convert(err).Message(), "request_id")
}
