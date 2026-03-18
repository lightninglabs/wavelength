//go:build itest
// +build itest

package harness

import (
	"context"
	"testing"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	client_harness "github.com/lightninglabs/darepo-client/harness"
	"github.com/lightninglabs/darepo/adminrpc"
	"github.com/stretchr/testify/require"
)

// TestArkHarnessCanStart verifies that the ArkHarness can
// successfully start the infrastructure and arkd server, then connect to the
// admin RPC.
func TestArkHarnessCanStart(t *testing.T) {
	clientOpts := client_harness.DefaultOptions()
	clientOpts.GroupName = t.Name()
	clientOpts.StartTapd = false // Don't start tapd for this basic test.

	h := NewArkHarness(t, &ArkHarnessOptions{
		ClientOptions: &clientOpts,
	})
	t.Cleanup(func() { h.Stop() })

	// Start the harness which will start bitcoind, lnd, and arkd.
	h.Start()

	// Verify we can reach the arkd admin RPC.
	ctx, cancel := context.WithTimeout(t.Context(), defaultSmallTimeout)
	defer cancel()

	resp, err := h.ArkAdminClient.Info(ctx, &adminrpc.InfoRequest{})
	require.NoError(h.T, err, "Admin Info RPC failed")
	h.T.Logf("Admin Info: version=%s", resp.Version)

	t.Logf("ArkAdminAddr=%s, ArkRPCAddr=%s", h.ArkAdminAddr, h.ArkRPCAddr)
	t.Logf("ArkHarness test completed successfully")
}

// TestArkHarnessCanStartClientDaemon verifies the real-daemon integration
// harness can boot arkd plus a real in-process darepod and reach the daemon
// through its public gRPC surface.
func TestArkHarnessCanStartClientDaemon(t *testing.T) {
	clientOpts := client_harness.DefaultOptions()
	clientOpts.GroupName = t.Name()
	clientOpts.StartTapd = false

	h := NewArkHarness(t, &ArkHarnessOptions{
		ClientOptions: &clientOpts,
	})
	t.Cleanup(h.Stop)

	h.Start()

	alice := h.StartClientDaemon("alice")

	ctx, cancel := context.WithTimeout(t.Context(), defaultSmallTimeout)
	defer cancel()

	info, err := alice.RPCClient.GetInfo(ctx, &daemonrpc.GetInfoRequest{})
	require.NoError(t, err, "client daemon GetInfo RPC failed")
	require.Equal(t, "regtest", info.Network)
	require.Equal(t, "lnd", info.WalletType)
	require.True(t, info.WalletReady, "wallet should be ready in lnd mode")
	require.NotEmpty(
		t, info.IdentityPubkey, "daemon identity should be set",
	)
}
