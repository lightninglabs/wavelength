package harness

import (
	"context"
	"testing"

	client_harness "github.com/lightninglabs/darepo-client/harness"
	"github.com/lightninglabs/darepo/adminrpc"
	"github.com/stretchr/testify/require"
)

// TestArkHarnessCanStart verifies that the ArkHarness can successfully start
// the infrastructure and arkd server, then connect to the admin RPC.
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
