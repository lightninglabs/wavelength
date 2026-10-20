package waved

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
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
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"gopkg.in/macaroon-bakery.v2/bakery"
)

// TestMacaroonHeaderMatcher forwards macaroon headers as metadata.
func TestMacaroonHeaderMatcher(t *testing.T) {
	t.Parallel()

	key, ok := macaroonHeaderMatcher("Macaroon")
	require.True(t, ok)
	require.Equal(t, rpcauth.MacaroonMetadataKey, key)
}

// TestGatewayRequestBodyLimit verifies oversized bodies are stopped before the
// gateway handler can retain or decode them.
func TestGatewayRequestBodyLimit(t *testing.T) {
	t.Parallel()

	for _, chunked := range []bool{false, true} {
		t.Run(fmt.Sprintf("chunked=%t", chunked), func(t *testing.T) {
			called := false
			handler := limitGatewayRequestBody(
				http.HandlerFunc(
					func(http.ResponseWriter,
						*http.Request) {

						called = true
					},
				),
			)
			// Even a valid JSON prefix followed by excess
			// whitespace must be rejected before the decoder or
			// downstream handler runs.
			body := bytes.NewBufferString("{}")
			body.Write(
				bytes.Repeat(
					[]byte(" "),
					int(defaultGatewayMaxRequestBodyBytes),
				),
			)
			req := httptest.NewRequest(http.MethodPost, "/", body)
			if chunked {
				req.ContentLength = -1
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			require.False(t, called)
			require.Equal(
				t, http.StatusRequestEntityTooLarge, rec.Code,
			)
		})
	}
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

// TestGatewayAcceptsBase64Envelope verifies a bytes-heavy request within the
// default gRPC receive limit still fits after REST base64 expansion.
func TestGatewayAcceptsBase64Envelope(t *testing.T) {
	t.Parallel()

	req := &waverpc.SignReceiveAuthMessageRequest{
		PaymentHash: make([]byte, 32),
		Message:     bytes.Repeat([]byte{42}, (4<<20)-128),
	}
	require.Less(t, proto.Size(req), 4<<20)
	encoded, err := protojson.Marshal(req)
	require.NoError(t, err)
	require.Greater(t, len(encoded), 4<<20)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := grpc.NewServer()
	waverpc.RegisterDaemonServiceServer(server, &gatewayTestDaemonServer{})
	t.Cleanup(server.Stop)
	go func() {
		_ = server.Serve(listener)
	}()

	mux := runtime.NewServeMux(gateway.ServeMuxOptions(nil)...)
	err = waverpc.RegisterDaemonServiceHandlerFromEndpoint(
		t.Context(), mux, listener.Addr().String(), []grpc.DialOption{
			grpc.WithTransportCredentials(
				insecure.NewCredentials(),
			),
		},
	)
	require.NoError(t, err)
	httpReq := httptest.NewRequest(
		http.MethodPost, "/v1/daemon/sign-receive-auth-message",
		bytes.NewReader(encoded),
	)
	httpReq.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	limitGatewayRequestBody(mux).ServeHTTP(rec, httpReq)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// SignReceiveAuthMessage accepts the transport envelope without wallet effects.
func (*gatewayTestDaemonServer) SignReceiveAuthMessage(_ context.Context,
	req *waverpc.SignReceiveAuthMessageRequest) (
	*waverpc.SignReceiveAuthMessageResponse, error) {

	return &waverpc.SignReceiveAuthMessageResponse{
		Signature: req.Message[:1],
	}, nil
}
