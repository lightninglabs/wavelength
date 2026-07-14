package waved

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/lightninglabs/wavelength/gateway"
	"github.com/lightninglabs/wavelength/rpcauth"
	"github.com/lightninglabs/wavelength/waverpc"
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
		Entity: entityInfo,
		Action: "read",
	}
	tempDir := t.TempDir()
	macaroonPath := filepath.Join(tempDir, "admin.macaroon")
	authService := newTestMacaroonService(
		t, macaroonPath, "waved",
		map[string][]bakery.Op{
			waverpc.DaemonService_GetInfo_FullMethodName: {
				readOp,
			},
		},
	)
	macaroonUnaryInterceptor, _, err := authService.Interceptors()
	require.NoError(t, err)

	grpcServer := grpc.NewServer(
		grpc.UnaryInterceptor(
			macaroonUnaryInterceptor,
		),
	)
	waverpc.RegisterDaemonServiceServer(
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
	err = waverpc.RegisterDaemonServiceHandlerFromEndpoint(
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
		macaroonPath)
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
	waverpc.UnimplementedDaemonServiceServer
}

func (*gatewayTestDaemonServer) GetInfo(context.Context,
	*waverpc.GetInfoRequest) (*waverpc.GetInfoResponse, error) {

	return &waverpc.GetInfoResponse{}, nil
}
