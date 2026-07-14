package waved

import (
	"context"
	"net"
	"testing"

	btcwalletrpc "github.com/btcsuite/btcwallet/rpc/walletrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// TestBtcwalletRPCRejectsUnavailableBackend verifies the native btcwallet
// surface is registered at daemon startup, but fails clearly until a
// self-managed wallet backend has been loaded.
func TestBtcwalletRPCRejectsUnavailableBackend(t *testing.T) {
	server := &Server{
		cfg: &Config{
			Wallet: &WalletConfig{
				Type: WalletTypeLnd,
			},
		},
	}

	listener := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer()
	cleanup := registerBtcwalletRPC(grpcServer, server)
	defer cleanup()

	errChan := make(chan error, 1)
	go func() {
		errChan <- grpcServer.Serve(listener)
	}()
	defer func() {
		grpcServer.Stop()
		_ = listener.Close()
		<-errChan
	}()

	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn,
			error) {

			return listener.Dial()
		}),
		grpc.WithTransportCredentials(
			insecure.NewCredentials(),
		),
	)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, conn.Close())
	}()

	client := btcwalletrpc.NewWalletServiceClient(conn)
	_, err = client.Ping(context.Background(), &btcwalletrpc.PingRequest{})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.Contains(t, err.Error(), "lwwallet and btcwallet")
}
