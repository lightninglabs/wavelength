package waved

import (
	"bytes"
	"context"
	"encoding/hex"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/lightninglabs/wavelength/gateway"
	"github.com/lightninglabs/wavelength/rpcauth"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/macaroons"
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
// reaches the daemon gRPC macaroon interceptor, and that authorization
// failures surface as 401 or 403 rather than a generic 500.
func TestGatewayForwardsMacaroonToGRPC(t *testing.T) {
	t.Parallel()

	readOp := bakery.Op{
		Entity: entityInfo,
		Action: "read",
	}
	tempDir := t.TempDir()
	macaroonPath := filepath.Join(tempDir, "admin.macaroon")
	permissions := map[string][]bakery.Op{
		waverpc.DaemonService_GetInfo_FullMethodName: {
			readOp,
		},
	}
	authService := newTestMacaroonService(
		t, macaroonPath, "waved", permissions,
	)
	macaroonUnaryInterceptor, _, err := authService.Interceptors()
	require.NoError(t, err)

	grpcServer := grpc.NewServer(
		grpc.UnaryInterceptor(
			macaroonStatusUnaryInterceptor(
				macaroonUnaryInterceptor, permissions,
			),
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
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	// A valid macaroon that lacks the required op is a permission failure,
	// not an authentication failure.
	idCtx := macaroons.ContextWithRootKeyID(
		t.Context(), macaroons.DefaultRootKeyID,
	)
	scoped, err := authService.NewMacaroon(
		idCtx, macaroons.DefaultRootKeyID, bakery.Op{
			Entity: entityVTXO,
			Action: "read",
		},
	)
	require.NoError(t, err)
	scopedBytes, err := scoped.M().MarshalBinary()
	require.NoError(t, err)
	req = httptest.NewRequest(
		http.MethodPost, "/v1/daemon/get-info",
		bytes.NewBufferString("{}"),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(
		rpcauth.MacaroonMetadataKey, hex.EncodeToString(scopedBytes),
	)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code)

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
