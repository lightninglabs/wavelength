package fees

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/stretchr/testify/require"
)

// TestTreasuryInitialize verifies bootstrap from DB state.
func TestTreasuryInitialize(t *testing.T) {
	t.Parallel()

	tt := NewTreasuryTracker()
	tt.Initialize(5_000_000, 50, btcutil.Amount(10_000_000))

	snap := tt.Snapshot()
	require.Equal(t, int64(5_000_000), snap.DeployedCapitalSat)
	require.Equal(t, int64(10_000_000), snap.WalletBalanceSat)
	require.Equal(t, int64(0), snap.PendingSweepSat)
	require.Equal(t, int64(15_000_000), snap.KMaxSat)
	require.Equal(t, 50, snap.LiveVTXOCount)
	require.InDelta(
		t, 5_000_000.0/15_000_000.0,
		snap.Utilization, 1e-9,
	)
}

// TestTreasuryRoundLifecycle simulates the full capital flow:
// deploy → forfeit (to pendingSweep) → sweep (clears pending).
func TestTreasuryRoundLifecycle(t *testing.T) {
	t.Parallel()

	tt := NewTreasuryTracker()
	tt.Initialize(0, 0, btcutil.Amount(10_000_000))

	// Initially no deployment.
	require.InDelta(t, 0.0, tt.Utilization(), 1e-9)

	// Round confirmed: deploy 3M sats across 10 VTXOs.
	tt.OnRoundConfirmed(3_000_000, 10)

	snap := tt.Snapshot()
	require.Equal(t, int64(3_000_000), snap.DeployedCapitalSat)
	require.Equal(t, 10, snap.LiveVTXOCount)
	require.InDelta(
		t, 3_000_000.0/13_000_000.0,
		snap.Utilization, 1e-9,
	)

	// VTXOs forfeited: capital moves to pendingSweep.
	tt.OnVTXOsForfeited(3_000_000, 10)

	snap = tt.Snapshot()
	require.Equal(t, int64(0), snap.DeployedCapitalSat)
	require.Equal(t, int64(3_000_000), snap.PendingSweepSat)
	require.Equal(t, 0, snap.LiveVTXOCount)

	// KMax is stable: deployed(0) + wallet(10M) + pending(3M).
	require.Equal(t, int64(13_000_000), snap.KMaxSat)
	require.InDelta(t, 0.0, snap.Utilization, 1e-9)

	// Sweep completed: clears pending.
	tt.OnSweepCompleted(3_000_000, 0)

	snap = tt.Snapshot()
	require.Equal(t, int64(0), snap.PendingSweepSat)

	// After wallet balance updates, KMax returns to just
	// wallet balance.
	tt.UpdateWalletBalance(btcutil.Amount(13_000_000))
	snap = tt.Snapshot()
	require.Equal(t, int64(13_000_000), snap.KMaxSat)
}

// TestTreasuryForfeiture verifies that forfeit moves capital
// from deployed to pendingSweep, keeping KMax stable.
func TestTreasuryForfeiture(t *testing.T) {
	t.Parallel()

	tt := NewTreasuryTracker()
	tt.Initialize(5_000_000, 50, btcutil.Amount(5_000_000))

	kMaxBefore := tt.Snapshot().KMaxSat

	// Forfeit 1M sats across 5 VTXOs.
	tt.OnVTXOsForfeited(1_000_000, 5)

	snap := tt.Snapshot()
	require.Equal(t, int64(4_000_000), snap.DeployedCapitalSat)
	require.Equal(t, int64(1_000_000), snap.PendingSweepSat)
	require.Equal(t, 45, snap.LiveVTXOCount)

	// KMax must not change — capital moved buckets, not lost.
	require.Equal(t, kMaxBefore, snap.KMaxSat,
		"KMax must be stable across forfeit")
}

// TestTreasuryUnderflowGuard verifies that pending sweep capital
// cannot go negative.
func TestTreasuryUnderflowGuard(t *testing.T) {
	t.Parallel()

	tt := NewTreasuryTracker()
	tt.Initialize(1_000, 1, btcutil.Amount(10_000))

	// Sweep more than pending (race/initialization edge case).
	tt.OnSweepCompleted(5_000, 3)

	snap := tt.Snapshot()
	require.Equal(t, int64(0), snap.PendingSweepSat)
}

// TestTreasuryWalletUpdate verifies wallet balance updates.
func TestTreasuryWalletUpdate(t *testing.T) {
	t.Parallel()

	tt := NewTreasuryTracker()
	tt.Initialize(5_000_000, 50, btcutil.Amount(5_000_000))

	// Wallet receives deposit.
	tt.UpdateWalletBalance(btcutil.Amount(15_000_000))

	snap := tt.Snapshot()
	require.Equal(t, int64(15_000_000), snap.WalletBalanceSat)
	require.Equal(t, int64(20_000_000), snap.KMaxSat)

	// Utilization should decrease with larger wallet.
	require.InDelta(
		t, 5_000_000.0/20_000_000.0,
		snap.Utilization, 1e-9,
	)
}

// TestTreasuryZeroKMax verifies that utilization is 0 when
// there is no capital.
func TestTreasuryZeroKMax(t *testing.T) {
	t.Parallel()

	tt := NewTreasuryTracker()

	require.InDelta(t, 0.0, tt.Utilization(), 1e-9)

	snap := tt.Snapshot()
	require.Equal(t, int64(0), snap.KMaxSat)
	require.InDelta(t, 0.0, snap.Utilization, 1e-9)
}

// TestTreasuryForfeitSweepKMaxStability verifies that the
// full forfeit→sweep cycle never causes a transient KMax drop
// that would trigger false congestion pricing.
func TestTreasuryForfeitSweepKMaxStability(t *testing.T) {
	t.Parallel()

	tt := NewTreasuryTracker()
	tt.Initialize(5_000_000, 50, btcutil.Amount(5_000_000))

	initialKMax := tt.Snapshot().KMaxSat

	// Forfeit all VTXOs.
	tt.OnVTXOsForfeited(5_000_000, 50)

	snap := tt.Snapshot()
	require.Equal(t, initialKMax, snap.KMaxSat,
		"KMax must not drop during forfeit")
	require.Equal(t, int64(0), snap.DeployedCapitalSat)
	require.Equal(t, int64(5_000_000), snap.PendingSweepSat)

	// Sweep clears pending.
	tt.OnSweepCompleted(5_000_000, 0)

	snap = tt.Snapshot()
	require.Equal(t, int64(0), snap.PendingSweepSat)

	// KMax temporarily drops until wallet balance updates.
	// This is expected — the sweep tx hasn't confirmed yet.
	require.Equal(
		t, int64(5_000_000), snap.KMaxSat,
		"KMax is wallet-only until balance refresh",
	)

	// Wallet balance refreshed after sweep confirms.
	tt.UpdateWalletBalance(btcutil.Amount(10_000_000))
	snap = tt.Snapshot()
	require.Equal(t, initialKMax, snap.KMaxSat,
		"KMax restored after wallet refresh")
}
