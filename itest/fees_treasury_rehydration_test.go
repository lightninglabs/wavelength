//go:build itest

package itest

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	client_harness "github.com/lightninglabs/darepo-client/harness"
	"github.com/lightninglabs/darepo/adminrpc"
	"github.com/lightninglabs/darepo/harness"
	"github.com/stretchr/testify/require"
)

// TestFeesTreasuryStatusRehydratesFromLedger verifies that the
// TreasuryTracker snapshot exposed via GetTreasuryStatus
// survives a RestartArkd. The load-bearing invariant is that
// TreasuryTracker.Reseed runs at ledger-actor Start and
// replays every balance bucket from the persisted ledger. If a
// regression dropped the reseed path the utilization quote
// would silently jump on every restart, producing unstable
// congestion pricing.
//
// On a fresh harness the ledger is empty, so both snapshots
// show zero capital; the test asserts (a) the snapshot surface
// is callable, (b) the values are stable across a restart, and
// (c) the returned shape includes live_vtxo_count. A deeper
// rehydration test that seeds non-zero capital belongs on a
// systest that runs a full round cycle; that is tracked under
// issue #263 Phase E.10.
func TestFeesTreasuryStatusRehydratesFromLedger(t *testing.T) {
	t.Parallel()

	clientOpts := client_harness.DefaultOptions()
	clientOpts.GroupName = t.Name()
	clientOpts.StartTapd = false

	h := harness.NewArkHarness(t, &harness.ArkHarnessOptions{
		ClientOptions: &clientOpts,
	})
	t.Cleanup(h.Stop)

	h.Start()
	h.FundOperatorLND(btcutil.SatoshiPerBitcoin)

	ctx := t.Context()

	before, err := h.ArkAdminClient.GetTreasuryStatus(
		ctx, &adminrpc.GetTreasuryStatusRequest{},
	)
	require.NoError(t, err, "GetTreasuryStatus (before)")

	// Restart arkd in place. Ledger state survives in SQLite;
	// the TreasuryTracker is a projection that must rebuild
	// from scratch on boot.
	h.RestartArkd()

	after, err := h.ArkAdminClient.GetTreasuryStatus(
		ctx, &adminrpc.GetTreasuryStatusRequest{},
	)
	require.NoError(t, err, "GetTreasuryStatus (after restart)")

	// Snapshot equality across restart: every numeric field
	// must match, and the live VTXO count must be identical.
	// Utilization is a float; require InDelta for
	// floating-point comparison.
	require.Equal(
		t, before.DeployedCapitalSat,
		after.DeployedCapitalSat,
		"deployed capital must survive restart",
	)
	require.Equal(
		t, before.WalletBalanceSat,
		after.WalletBalanceSat,
		"wallet balance must survive restart",
	)
	require.Equal(
		t, before.KMaxSat, after.KMaxSat,
		"k_max must survive restart",
	)
	require.Equal(
		t, before.LiveVtxoCount, after.LiveVtxoCount,
		"live VTXO count must survive restart",
	)
	require.InDelta(
		t, before.Utilization, after.Utilization, 1e-9,
		"utilization must survive restart",
	)
}
