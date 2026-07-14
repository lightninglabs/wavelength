package walletdk

import (
	"context"
	"net"
	"testing"

	btcwalletrpc "github.com/btcsuite/btcwallet/rpc/walletrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

type stubBtcwalletServer struct {
	btcwalletrpc.UnimplementedWalletServiceServer
}

// Ping confirms the SDK-native client reached this private test server.
func (s stubBtcwalletServer) Ping(context.Context, *btcwalletrpc.PingRequest) (
	*btcwalletrpc.PingResponse, error) {

	return &btcwalletrpc.PingResponse{}, nil
}

// TestBtcwalletRPCUsesClientBufconn verifies walletdk's native btcwallet
// escape hatch is built from the same private gRPC connection as the rest of
// the SDK. Embedded clients use this path to reach waved's registered
// btcsuite walletrpc service without opening another listener.
func TestBtcwalletRPCUsesClientBufconn(t *testing.T) {
	t.Parallel()

	listener := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer()
	btcwalletrpc.RegisterWalletServiceServer(
		grpcServer, &stubBtcwalletServer{},
	)

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
		grpc.WithContextDialer(func(ctx context.Context, _ string) (
			net.Conn, error) {

			return listener.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(
			insecure.NewCredentials(),
		),
	)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, conn.Close())
	}()

	client := newClient(conn, true, closedWaitChan(),
		func(context.Context) error {
			return conn.Close()
		},
	)

	require.NotNil(t, client.BtcwalletRPC())
	_, err = client.BtcwalletRPC().Ping(
		context.Background(), &btcwalletrpc.PingRequest{},
	)
	require.NoError(t, err)
}

// TestPrepareSendRejectsAmbiguousDestination ensures wrappers cannot
// accidentally set both destination fields and have walletdk pick one.
func TestPrepareSendRejectsAmbiguousDestination(t *testing.T) {
	t.Parallel()

	client := &Client{canWallet: true}

	_, err := client.PrepareSend(context.Background(), PrepareSendRequest{
		Invoice:        "lnbcrt...",
		OnchainAddress: "bcrt1...",
	})
	require.ErrorContains(t, err, "not both")
}

// TestListRejectsUnknownKind ensures SDK-side filters fail before a request is
// sent with ENTRY_KIND_UNSPECIFIED.
func TestListRejectsUnknownKind(t *testing.T) {
	t.Parallel()

	client := &Client{canWallet: true}

	_, err := client.List(context.Background(), ListRequest{
		Kinds: []EntryKind{"junk"},
	})
	require.ErrorContains(t, err, "unknown entry kind")
}

// TestSubscribeRejectsUnknownKind mirrors List validation for streaming
// subscriptions.
func TestSubscribeRejectsUnknownKind(t *testing.T) {
	t.Parallel()

	client := &Client{canWallet: true}

	_, _, err := client.Subscribe(context.Background(), SubscribeRequest{
		Kinds: []EntryKind{"junk"},
	})
	require.ErrorContains(t, err, "unknown entry kind")
}
