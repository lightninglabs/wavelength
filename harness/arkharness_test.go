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

	expectedWalletType, err := resolveClientDaemonWalletType("")
	require.NoError(t, err)
	require.Equal(t, expectedWalletType, info.WalletType)
	require.True(t, info.WalletReady, "wallet should be ready")
	require.NotEmpty(
		t, info.IdentityPubkey, "daemon identity should be set",
	)
}

// TestClientDaemonHarnessTriggerRoundRegistration verifies the harness can
// inject RegistrationRequested into a real daemon's round actor.
func TestClientDaemonHarnessTriggerRoundRegistration(t *testing.T) {
	clientOpts := client_harness.DefaultOptions()
	clientOpts.GroupName = t.Name()
	clientOpts.StartTapd = false

	h := NewArkHarness(t, &ArkHarnessOptions{
		ClientOptions: &clientOpts,
	})
	t.Cleanup(h.Stop)

	h.Start()

	alice := h.StartClientDaemon("alice")

	addrResp, err := alice.RPCClient.NewAddress(
		t.Context(), &daemonrpc.NewAddressRequest{},
	)
	require.NoError(t, err)

	h.Faucet(addrResp.Address, 100_000)
	h.Generate(7)
	require.Eventually(t, func() bool {
		balance, balanceErr := alice.RPCClient.GetBalance(
			t.Context(), &daemonrpc.GetBalanceRequest{},
		)
		require.NoError(t, balanceErr)

		return balance.BoardingConfirmedSat == 100_000
	}, defaultTimeout, pollInterval)
	require.Eventually(t, func() bool {
		rounds, listErr := alice.RPCClient.ListRounds(
			t.Context(), &daemonrpc.ListRoundsRequest{},
		)
		require.NoError(t, listErr)

		for _, roundInfo := range rounds.Rounds {
			if roundInfo.State == daemonrpc.RoundState_ROUND_STATE_PENDING_ASSEMBLY {
				return true
			}
		}

		return false
	}, defaultTimeout, pollInterval)

	alice.TriggerRoundRegistration()
	require.Eventually(t, func() bool {
		rounds, listErr := alice.RPCClient.ListRounds(
			t.Context(), &daemonrpc.ListRoundsRequest{},
		)
		require.NoError(t, listErr)

		for _, roundInfo := range rounds.Rounds {
			if roundInfo.State != daemonrpc.RoundState_ROUND_STATE_PENDING_ASSEMBLY {
				return true
			}
		}

		return false
	}, defaultTimeout, pollInterval)
}
