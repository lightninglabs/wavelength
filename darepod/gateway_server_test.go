package darepod

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/gateway"
	"github.com/lightninglabs/darepo-client/rpcauth"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"gopkg.in/macaroon-bakery.v2/bakery"
)

// TestMacaroonHeaderMatcher forwards macaroon headers as metadata.
func TestMacaroonHeaderMatcher(t *testing.T) {
	t.Parallel()

	key, ok := macaroonHeaderMatcher("Macaroon")
	require.True(t, ok)
	require.Equal(t, rpcauth.MacaroonMetadataKey, key)
}

// TestGatewayForwardsMacaroonToGRPC verifies a REST request's macaroon header
// reaches the daemon gRPC macaroon interceptor.
func TestGatewayForwardsMacaroonToGRPC(t *testing.T) {
	t.Parallel()

	readOp := bakery.Op{
		Entity: darepodMacaroonEntity,
		Action: "read",
	}
	tempDir := t.TempDir()
	authService, err := rpcauth.NewService(
		filepath.Join(tempDir, "macaroons.db"),
		filepath.Join(tempDir, "admin.macaroon"),
		"darepod", []bakery.Op{readOp},
		map[string][]bakery.Op{
			daemonrpc.DaemonService_GetInfo_FullMethodName: {
				readOp,
			},
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, authService.Close())
	})

	grpcServer := grpc.NewServer(
		grpc.UnaryInterceptor(
			authService.UnaryServerInterceptor(),
		),
	)
	daemonrpc.RegisterDaemonServiceServer(
		grpcServer, &gatewayTestDaemonServer{},
	)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(grpcServer.Stop)
	go func() {
		_ = grpcServer.Serve(listener)
	}()

	mux := runtime.NewServeMux(
		gateway.ServeMuxOptions(macaroonHeaderMatcher)...,
	)
	err = daemonrpc.RegisterDaemonServiceHandlerFromEndpoint(
		context.Background(), mux, listener.Addr().String(),
		[]grpc.DialOption{
			grpc.WithTransportCredentials(
				insecure.NewCredentials(),
			),
		},
	)
	require.NoError(t, err)

	req := httptest.NewRequest(
		http.MethodPost, "/v1/daemon/get-info",
		bytes.NewBufferString("{}"),
	)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.NotEqual(t, http.StatusOK, rec.Code)

	macHex, err := rpcauth.HexFromFile(
		filepath.Join(tempDir, "admin.macaroon"),
	)
	require.NoError(t, err)
	req = httptest.NewRequest(
		http.MethodPost, "/v1/daemon/get-info",
		bytes.NewBufferString("{}"),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(rpcauth.MacaroonMetadataKey, macHex)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
}

type gatewayTestDaemonServer struct {
	daemonrpc.UnimplementedDaemonServiceServer
}

func (*gatewayTestDaemonServer) GetInfo(context.Context,
	*daemonrpc.GetInfoRequest) (*daemonrpc.GetInfoResponse, error) {

	return &daemonrpc.GetInfoResponse{}, nil
}
