package ledger

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/darepo/fees"
	"github.com/lightningnetwork/lnd/clock"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// errBalanceRead is the sentinel returned by mockBalanceReader
// when a test wants the reader to fail. Declared at package
// scope so errors.Is checks can match on identity.
var errBalanceRead = errors.New("balance read failed")

// mockBalanceReader is a trivial LedgerBalanceReader that serves
// pre-seeded per-account totals. Used to drive
// reseedTreasuryTracker under test without a real DB.
type mockBalanceReader struct {
	balances map[fees.AccountID]btcutil.Amount
	err      error
}

func (m *mockBalanceReader) GetAccountBalance(
	_ context.Context,
	account fees.AccountID) (btcutil.Amount, error) {

	if m.err != nil {
		return 0, m.err
	}

	return m.balances[account], nil
}

// TestReseedTreasuryTrackerFromLedger verifies the rebuild path
// that H-1/H-2 closes: on Start, deployed capital and wallet
// balance are loaded from the persisted ledger so congestion
// pricing reads correct utilization immediately after restart
// instead of zero until new events flow through the actor.
func TestReseedTreasuryTrackerFromLedger(t *testing.T) {
	t.Parallel()

	tracker := fees.NewTreasuryTracker()
	reader := &mockBalanceReader{
		balances: map[fees.AccountID]btcutil.Amount{
			fees.AccountDeployedCapital: 1_234_567,
			fees.AccountTreasuryWallet:  89_000,
		},
	}

	a := &LedgerActor{
		cfg: ActorConfig{
			TreasuryTracker: tracker,
			BalanceReader: fn.Some[LedgerBalanceReader](
				reader,
			),
		},
		log:  disabledLogger(),
		clk:  clock.NewTestClock(fixedTestTime()),
		utxo: newUTXOTracker(),
	}

	err := a.reseedTreasuryTracker(context.Background())
	require.NoError(t, err)

	snap := tracker.Snapshot()
	require.Equal(t, int64(1_234_567), snap.DeployedCapitalSat)
	require.Equal(t, int64(89_000), snap.WalletBalanceSat)
	require.Equal(t, int64(0), snap.PendingSweepSat)
	require.Equal(t, 0, snap.LiveVTXOCount)
}

// TestReseedTreasuryTrackerNoOp verifies the reseed gracefully
// degrades when either the tracker or the balance reader is
// absent: a unit-test harness without a wired reader must be
// able to Start the actor without a nil-deref.
func TestReseedTreasuryTrackerNoOp(t *testing.T) {
	t.Parallel()

	// No tracker -> no-op.
	aNoTracker := &LedgerActor{
		cfg:  ActorConfig{},
		log:  disabledLogger(),
		clk:  clock.NewTestClock(fixedTestTime()),
		utxo: newUTXOTracker(),
	}
	require.NoError(
		t, aNoTracker.reseedTreasuryTracker(
			context.Background(),
		),
	)

	// Tracker but no reader -> no-op, tracker stays at zero.
	tracker := fees.NewTreasuryTracker()
	aNoReader := &LedgerActor{
		cfg: ActorConfig{
			TreasuryTracker: tracker,
		},
		log:  disabledLogger(),
		clk:  clock.NewTestClock(fixedTestTime()),
		utxo: newUTXOTracker(),
	}
	require.NoError(
		t, aNoReader.reseedTreasuryTracker(
			context.Background(),
		),
	)

	snap := tracker.Snapshot()
	require.Equal(t, int64(0), snap.DeployedCapitalSat)
	require.Equal(t, int64(0), snap.WalletBalanceSat)
}

// TestReseedTreasuryTrackerPropagatesReadError verifies that a
// balance-reader error short-circuits reseed so Start surfaces
// the failure rather than silently accepting a stale zero
// tracker.
func TestReseedTreasuryTrackerPropagatesReadError(t *testing.T) {
	t.Parallel()

	reader := &mockBalanceReader{
		err: errBalanceRead,
	}
	tracker := fees.NewTreasuryTracker()

	a := &LedgerActor{
		cfg: ActorConfig{
			TreasuryTracker: tracker,
			BalanceReader: fn.Some[LedgerBalanceReader](
				reader,
			),
		},
		log:  disabledLogger(),
		clk:  clock.NewTestClock(fixedTestTime()),
		utxo: newUTXOTracker(),
	}

	err := a.reseedTreasuryTracker(context.Background())
	require.ErrorIs(t, err, errBalanceRead)
}
