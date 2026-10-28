package waved

import (
	"context"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"gopkg.in/macaroon-bakery.v2/bakery"
	"gopkg.in/macaroon-bakery.v2/bakery/checkers"
	"gopkg.in/macaroon.v2"
)

// newReadOnlyTestMacaroon bakes a read-only macaroon through the real waved
// permission map.
func newReadOnlyTestMacaroon(t *testing.T,
	authService *lndclient.MacaroonService) *macaroon.Macaroon {

	t.Helper()

	ctx := macaroons.ContextWithRootKeyID(
		t.Context(), macaroons.DefaultRootKeyID,
	)
	mac, err := authService.NewMacaroon(
		ctx, macaroons.DefaultRootKeyID, wavedReadOnlyPermissions()...,
	)
	require.NoError(t, err)

	return mac.M()
}

// macaroonMetadata returns incoming RPC metadata carrying mac the way a
// client's per-RPC credentials would.
func macaroonMetadata(t *testing.T, mac *macaroon.Macaroon) metadata.MD {
	t.Helper()

	macBytes, err := mac.MarshalBinary()
	require.NoError(t, err)

	return metadata.Pairs("macaroon", hex.EncodeToString(macBytes))
}

// TestMacaroonAuthFailureStatusCodes runs every class of macaroon failure
// through the real lnd interceptors as wrapped by waved and verifies each one
// carries the gRPC code the CLI and the REST gateway rely on, rather than
// codes.Unknown.
func TestMacaroonAuthFailureStatusCodes(t *testing.T) {
	t.Parallel()

	adminPath := filepath.Join(t.TempDir(), "admin.macaroon")
	authService := newTestMacaroonService(
		t, adminPath, wavedMacaroonLocation, wavedRPCPermissions,
	)
	lndUnary, lndStream, err := authService.Interceptors()
	require.NoError(t, err)
	unary := macaroonStatusUnaryInterceptor(
		lndUnary, wavedRPCPermissions,
	)
	stream := macaroonStatusStreamInterceptor(
		lndStream, wavedRPCPermissions,
	)

	readOnly := newReadOnlyTestMacaroon(t, authService)

	expired := readOnly.Clone()
	expiredCaveat := checkers.TimeBeforeCaveat(time.Now().Add(-time.Hour))
	require.NoError(
		t,
		expired.AddFirstPartyCaveat(
			[]byte(expiredCaveat.Condition),
		),
	)

	wrongKey, err := macaroon.New(
		[]byte("not the daemon root key"), readOnly.Id(),
		wavedMacaroonLocation, macaroon.V2,
	)
	require.NoError(t, err)

	// A version-3 ID byte followed by bytes that are not a valid ID
	// encoding fails while the bakery looks up the root key.
	badID, err := macaroon.New(
		[]byte("not the daemon root key"),
		[]byte{byte(bakery.Version3), 1, 2, 3}, wavedMacaroonLocation,
		macaroon.V2,
	)
	require.NoError(t, err)

	// Recv requires address:write, which the read-only macaroon lacks.
	// Balance requires info:read, which it grants.
	recv := wavewalletrpc.WalletService_Recv_FullMethodName
	balance := wavewalletrpc.WalletService_Balance_FullMethodName

	testCases := []struct {
		name       string
		md         metadata.MD
		method     string
		wantCode   codes.Code
		wantRunsOK bool
	}{
		{
			name:     "missing permission",
			md:       macaroonMetadata(t, readOnly),
			method:   recv,
			wantCode: codes.PermissionDenied,
		},
		{
			name:     "expired macaroon",
			md:       macaroonMetadata(t, expired),
			method:   balance,
			wantCode: codes.Unauthenticated,
		},
		{
			name:     "wrong root key",
			md:       macaroonMetadata(t, wrongKey),
			method:   balance,
			wantCode: codes.Unauthenticated,
		},
		{
			// A method missing from the permission map is a
			// daemon fault, not a credential problem.
			name:     "unmapped method",
			md:       macaroonMetadata(t, readOnly),
			method:   "/waverpc.DaemonService/NotARealMethod",
			wantCode: codes.Internal,
		},
		{
			name:     "malformed macaroon id",
			md:       macaroonMetadata(t, badID),
			method:   balance,
			wantCode: codes.Unauthenticated,
		},
		{
			name:     "no macaroon",
			method:   balance,
			wantCode: codes.Unauthenticated,
		},
		{
			name:     "undecodable macaroon",
			md:       metadata.Pairs("macaroon", "zz"),
			method:   balance,
			wantCode: codes.Unauthenticated,
		},
		{
			name:       "authorized",
			md:         macaroonMetadata(t, readOnly),
			method:     balance,
			wantCode:   codes.OK,
			wantRunsOK: true,
		},
	}

	for _, tc := range testCases {
		// A case without metadata models a client that sent no
		// macaroon at all.
		ctx := t.Context()
		if tc.md != nil {
			ctx = metadata.NewIncomingContext(ctx, tc.md)
		}

		t.Run(tc.name+"/unary", func(t *testing.T) {
			ran := false
			_, err := unary(
				ctx, nil,
				&grpc.UnaryServerInfo{FullMethod: tc.method},
				func(context.Context, any) (any, error) {
					ran = true

					return nil, nil
				},
			)
			require.Equal(t, tc.wantRunsOK, ran)
			requireStatusCode(t, err, tc.wantCode)
		})

		t.Run(tc.name+"/stream", func(t *testing.T) {
			ran := false
			err := stream(
				nil, &testServerStream{ctx: ctx},
				&grpc.StreamServerInfo{FullMethod: tc.method},
				func(any, grpc.ServerStream) error {
					ran = true

					return nil
				},
			)
			require.Equal(t, tc.wantRunsOK, ran)
			requireStatusCode(t, err, tc.wantCode)
		})
	}
}

// TestMacaroonStatusInterceptorPassesHandlerErrors verifies the wrapper only
// rewrites failures from the authorization check. An error the handler
// returns after a successful check must reach the caller unchanged, so a
// handler that chose its own code (or relies on a later mapping interceptor)
// is not reported as an authentication failure.
func TestMacaroonStatusInterceptorPassesHandlerErrors(t *testing.T) {
	t.Parallel()

	allow := func(ctx context.Context, req any, _ *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler) (any, error) {

		return handler(ctx, req)
	}
	allowStream := func(srv any, ss grpc.ServerStream,
		_ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {

		return handler(srv, ss)
	}

	handlerErr := errors.New("handler failed")

	_, err := macaroonStatusUnaryInterceptor(allow, nil)(
		t.Context(), nil, &grpc.UnaryServerInfo{},
		func(context.Context, any) (any, error) {
			return nil, handlerErr
		},
	)
	require.ErrorIs(t, err, handlerErr)

	err = macaroonStatusStreamInterceptor(allowStream, nil)(
		nil, &testServerStream{ctx: t.Context()},
		&grpc.StreamServerInfo{},
		func(any, grpc.ServerStream) error {
			return handlerErr
		},
	)
	require.ErrorIs(t, err, handlerErr)
}

// TestMacaroonAuthStatusKeepsExistingStatus verifies an authorization error
// that already carries a gRPC status keeps its code.
func TestMacaroonAuthStatusKeepsExistingStatus(t *testing.T) {
	t.Parallel()

	err := status.Error(codes.Unavailable, "not ready")
	requireStatusCode(
		t, macaroonAuthStatus(err, "", nil), codes.Unavailable,
	)
	require.NoError(t, macaroonAuthStatus(nil, "", nil))
}

// requireStatusCode asserts err carries the given gRPC status code.
func requireStatusCode(t *testing.T, err error, want codes.Code) {
	t.Helper()

	st, ok := status.FromError(err)
	require.True(t, ok, "error %v carries no gRPC status", err)
	require.Equal(t, want, st.Code(), "error: %v", err)
}

// testServerStream is a minimal grpc.ServerStream that only supplies a
// context.
type testServerStream struct {
	grpc.ServerStream

	ctx context.Context //nolint:containedctx
}

// Context returns the stream's context.
func (s *testServerStream) Context() context.Context {
	return s.ctx
}
