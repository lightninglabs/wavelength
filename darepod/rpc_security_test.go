package darepod

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/stretchr/testify/require"
)

// TestBakeReadOnlyMacaroon bakes the read-only macaroon through the real
// permission map and verifies it authorizes a read method, rejects a write
// method, and is created only when absent.
func TestBakeReadOnlyMacaroon(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	adminPath := filepath.Join(tempDir, "admin.macaroon")
	authService := newTestMacaroonService(
		t, adminPath, darepodMacaroonLocation, darepodRPCPermissions,
	)

	ctx := context.Background()

	require.NoError(t, bakeReadOnlyMacaroon(ctx, authService, adminPath))

	readOnlyPath := filepath.Join(tempDir, readOnlyMacaroonFileName)
	macBytes, err := os.ReadFile(readOnlyPath)
	require.NoError(t, err)
	require.NotEmpty(t, macBytes)

	// A read method must be authorized by the read-only macaroon.
	getInfo := daemonrpc.DaemonService_GetInfo_FullMethodName
	require.NoError(
		t, authService.CheckMacAuth(
			ctx, macBytes, darepodRPCPermissions[getInfo], getInfo,
		),
	)

	// A mutating method must be rejected.
	sendVTXO := daemonrpc.DaemonService_SendVTXO_FullMethodName
	require.Error(
		t, authService.CheckMacAuth(
			ctx, macBytes, darepodRPCPermissions[sendVTXO],
			sendVTXO,
		),
	)

	// Baking again is a no-op that leaves the existing file untouched, so a
	// previously distributed copy stays valid.
	require.NoError(t, bakeReadOnlyMacaroon(ctx, authService, adminPath))
	after, err := os.ReadFile(readOnlyPath)
	require.NoError(t, err)
	require.Equal(t, macBytes, after)
}
