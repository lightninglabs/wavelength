package rpcauth

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"
	"gopkg.in/macaroon-bakery.v2/bakery"
)

var (
	testReadOp = bakery.Op{
		Entity: "test",
		Action: "read",
	}
	testWriteOp = bakery.Op{
		Entity: "test",
		Action: "write",
	}
)

// TestServiceAuthorizeAcceptsGrantedMacaroon verifies a macaroon with the
// required permission passes auth.
func TestServiceAuthorizeAcceptsGrantedMacaroon(t *testing.T) {
	t.Parallel()

	svc, macHex := newTestAuthService(t, []bakery.Op{testReadOp})
	ctx := metadata.NewIncomingContext(
		context.Background(),
		metadata.Pairs(MacaroonMetadataKey, macHex),
	)

	require.NoError(t, svc.authorize(ctx, "/test.Service/GetInfo"))
}

// TestServiceAuthorizeRejectsMissingMacaroon verifies auth fails without
// macaroon metadata.
func TestServiceAuthorizeRejectsMissingMacaroon(t *testing.T) {
	t.Parallel()

	svc, _ := newTestAuthService(t, []bakery.Op{testReadOp})

	err := svc.authorize(context.Background(), "/test.Service/GetInfo")
	require.ErrorContains(t, err, "metadata")
}

// TestServiceAuthorizeRejectsInsufficientPermission verifies a valid macaroon
// cannot call a method with a missing permission.
func TestServiceAuthorizeRejectsInsufficientPermission(t *testing.T) {
	t.Parallel()

	svc, macHex := newTestAuthService(t, []bakery.Op{testReadOp})
	ctx := metadata.NewIncomingContext(
		context.Background(),
		metadata.Pairs(MacaroonMetadataKey, macHex),
	)

	err := svc.authorize(ctx, "/test.Service/Send")
	require.Error(t, err)
}

// TestServiceAuthorizeRejectsUnknownMethod verifies unmapped methods fail
// closed before macaroon permissions are considered.
func TestServiceAuthorizeRejectsUnknownMethod(t *testing.T) {
	t.Parallel()

	svc, macHex := newTestAuthService(
		t, []bakery.Op{testReadOp, testWriteOp},
	)
	ctx := metadata.NewIncomingContext(
		context.Background(),
		metadata.Pairs(MacaroonMetadataKey, macHex),
	)

	err := svc.authorize(ctx, "/test.Service/Unknown")
	require.ErrorContains(t, err, "unknown macaroon permissions")
}

// newTestAuthService returns an auth service and hex macaroon for tests.
func newTestAuthService(t *testing.T, ops []bakery.Op) (*Service, string) {
	t.Helper()

	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "macaroons.db")
	macaroonPath := filepath.Join(tempDir, "admin.macaroon")

	svc, err := NewService(dbPath, macaroonPath, "test", ops,
		map[string][]bakery.Op{
			"/test.Service/GetInfo": {testReadOp},
			"/test.Service/Send":    {testWriteOp},
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, svc.Close())
	})

	macHex, err := HexFromFile(macaroonPath)
	require.NoError(t, err)

	return svc, macHex
}
