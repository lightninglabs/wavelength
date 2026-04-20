package ledger

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/darepo/fees"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// errSnapshotRead is the sentinel returned by
// mockSnapshotReader when a test wants the reader to fail.
var errSnapshotRead = errors.New("snapshot read failed")

// mockSnapshotReader implements UTXOSnapshotReader for tests
// that exercise the reseedUTXOSnapshot path.
type mockSnapshotReader struct {
	mu           sync.Mutex
	utxos        []WalletUTXO
	lastBlockHgt int64
	err          error

	// rowCount is what CountAuditRows returns. Set per-test to
	// drive the fresh-install vs empty-but-historical split.
	rowCount int64

	// countErr (when non-nil) short-circuits CountAuditRows
	// before utxos/err are consulted. Keeps the two failure
	// modes distinct so tests can target each one.
	countErr error
}

// ListLiveWalletUTXOs returns the configured set.
func (m *mockSnapshotReader) ListLiveWalletUTXOs(
	_ context.Context) ([]WalletUTXO, int64, error) {

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.err != nil {
		return nil, 0, m.err
	}

	out := make([]WalletUTXO, len(m.utxos))
	copy(out, m.utxos)

	return out, m.lastBlockHgt, nil
}

// CountAuditRows returns the configured row count.
func (m *mockSnapshotReader) CountAuditRows(
	_ context.Context) (int64, error) {

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.countErr != nil {
		return 0, m.countErr
	}

	return m.rowCount, nil
}

// TestReseedUTXOSnapshotRehydratesTracker verifies the rebuild
// path that H-6 closes: Start loads the live UTXO set from the
// persisted audit log and marks the tracker seeded so the next
// block epoch attributes new UTXOs as real deposits instead of
// silently treating them as part of the seeding baseline.
func TestReseedUTXOSnapshotRehydratesTracker(t *testing.T) {
	t.Parallel()

	a, ledgerStore, lister, audit := newDiffTestActor(t)
	ctx := context.Background()

	reader := &mockSnapshotReader{
		utxos: []WalletUTXO{
			{Outpoint: makeOutpoint(1), Amount: 10_000},
			{Outpoint: makeOutpoint(2), Amount: 25_000},
		},
		lastBlockHgt: 800_000,
	}
	a.cfg.UTXOSnapshotReader = fn.Some[UTXOSnapshotReader](reader)

	require.NoError(t, a.reseedUTXOSnapshot(ctx))

	// After reseed, the tracker should see two live UTXOs
	// and be flagged seeded.
	a.utxo.mu.Lock()
	require.True(t, a.utxo.seeded)
	require.Len(t, a.utxo.prev, 2)
	require.Equal(
		t, btcutil.Amount(10_000),
		a.utxo.prev[makeOutpoint(1)],
	)
	require.Equal(
		t, btcutil.Amount(25_000),
		a.utxo.prev[makeOutpoint(2)],
	)
	a.utxo.mu.Unlock()

	// Simulate the post-restart block: a NEW deposit arrives
	// alongside the two already-known UTXOs. Because the
	// tracker is seeded, this must book an external_deposit
	// ledger leg rather than silently skip as during baseline
	// seeding.
	lister.set([]WalletUTXO{
		{Outpoint: makeOutpoint(1), Amount: 10_000},
		{Outpoint: makeOutpoint(2), Amount: 25_000},
		{Outpoint: makeOutpoint(9), Amount: 50_000},
	})
	require.NoError(t, a.handleBlockEpoch(ctx, &BlockEpochMsg{
		BlockHeight: 800_001,
	}))

	// Exactly one ledger entry: the new external deposit.
	entries := ledgerStore.getEntries()
	require.Len(t, entries, 1)
	require.Equal(t, fees.LedgerEventExternalDeposit,
		entries[0].EventType)
	require.Equal(t, btcutil.Amount(50_000), entries[0].Amount)

	// One audit row for the new UTXO.
	rows := audit.get()
	require.Len(t, rows, 1)
	require.Equal(t, UTXOAuditCreated, rows[0].Event)
}

// TestReseedUTXOSnapshotEmptyAuditLogKeepsSeedingPass verifies
// that when the audit log is empty (fresh install), the
// tracker stays unseeded so the first genuine block epoch
// still performs the baseline pass rather than mis-booking
// the first observed UTXO set as external deposits.
func TestReseedUTXOSnapshotEmptyAuditLogKeepsSeedingPass(t *testing.T) {
	t.Parallel()

	a, ledgerStore, lister, _ := newDiffTestActor(t)
	ctx := context.Background()

	reader := &mockSnapshotReader{
		utxos:        nil,
		lastBlockHgt: 0,
		rowCount:     0,
	}
	a.cfg.UTXOSnapshotReader = fn.Some[UTXOSnapshotReader](reader)

	require.NoError(t, a.reseedUTXOSnapshot(ctx))

	a.utxo.mu.Lock()
	require.False(t, a.utxo.seeded)
	require.Empty(t, a.utxo.prev)
	a.utxo.mu.Unlock()

	// First real block still behaves like a baseline pass:
	// audit rows yes, no ledger booking.
	lister.set([]WalletUTXO{
		{Outpoint: makeOutpoint(1), Amount: 10_000},
	})
	require.NoError(t, a.handleBlockEpoch(ctx, &BlockEpochMsg{
		BlockHeight: 800_000,
	}))
	require.Empty(t, ledgerStore.getEntries())
}

// TestReseedUTXOSnapshotEmptyLiveWithHistorySeeds covers the
// case the earlier version mis-handled: a long-running
// deployment whose wallet is currently empty (everything swept
// or pending boarding) but whose audit log holds historical
// rows. The old code treated this like a fresh install and
// silently swallowed the first post-restart external deposit.
// After the fix, a non-zero audit row count flips seeded=true
// with an empty snapshot so the first new UTXO books as a real
// external_deposit rather than a seeding no-op.
func TestReseedUTXOSnapshotEmptyLiveWithHistorySeeds(t *testing.T) {
	t.Parallel()

	a, ledgerStore, lister, audit := newDiffTestActor(t)
	ctx := context.Background()

	// Empty live set, but history exists.
	reader := &mockSnapshotReader{
		utxos:    nil,
		rowCount: 42,
	}
	a.cfg.UTXOSnapshotReader = fn.Some[UTXOSnapshotReader](reader)

	require.NoError(t, a.reseedUTXOSnapshot(ctx))

	// Tracker must be flagged seeded with an empty snapshot.
	a.utxo.mu.Lock()
	require.True(t, a.utxo.seeded,
		"seeded must be true when history exists even if "+
			"live set is empty")
	require.Empty(t, a.utxo.prev)
	a.utxo.mu.Unlock()

	// First post-restart block observes a new deposit. Because
	// seeded=true, this must book an external_deposit rather
	// than being folded into a fresh baseline pass.
	lister.set([]WalletUTXO{
		{Outpoint: makeOutpoint(7), Amount: 75_000},
	})
	require.NoError(t, a.handleBlockEpoch(ctx, &BlockEpochMsg{
		BlockHeight: 900_000,
	}))

	entries := ledgerStore.getEntries()
	require.Len(t, entries, 1,
		"empty-live-with-history must book the first new "+
			"UTXO as external_deposit")
	require.Equal(t, fees.LedgerEventExternalDeposit,
		entries[0].EventType)
	require.Equal(t, btcutil.Amount(75_000), entries[0].Amount)

	rows := audit.get()
	require.Len(t, rows, 1)
	require.Equal(t, UTXOAuditCreated, rows[0].Event)
}

// TestReseedUTXOSnapshotPropagatesCountError verifies that a
// failure from CountAuditRows short-circuits Start so a
// transient DB error does not cause the actor to silently
// re-enter seeding and drop external-deposit attribution.
func TestReseedUTXOSnapshotPropagatesCountError(t *testing.T) {
	t.Parallel()

	a, _, _, _ := newDiffTestActor(t)
	ctx := context.Background()

	wantErr := errors.New("count failed")
	reader := &mockSnapshotReader{
		utxos:    nil,
		countErr: wantErr,
	}
	a.cfg.UTXOSnapshotReader = fn.Some[UTXOSnapshotReader](reader)

	err := a.reseedUTXOSnapshot(ctx)
	require.ErrorIs(t, err, wantErr)
}

// TestReseedUTXOSnapshotNoReaderNoOp verifies graceful
// degradation when the snapshot reader is not wired: reseed is
// a no-op and the first block epoch performs the in-memory
// seeding pass as before.
func TestReseedUTXOSnapshotNoReaderNoOp(t *testing.T) {
	t.Parallel()

	a, _, _, _ := newDiffTestActor(t)
	ctx := context.Background()

	// No UTXOSnapshotReader set.
	require.NoError(t, a.reseedUTXOSnapshot(ctx))

	a.utxo.mu.Lock()
	require.False(t, a.utxo.seeded)
	a.utxo.mu.Unlock()
}

// TestReseedUTXOSnapshotPropagatesReaderError verifies a reader
// error short-circuits Start so the actor never ships a stale
// or half-populated snapshot.
func TestReseedUTXOSnapshotPropagatesReaderError(t *testing.T) {
	t.Parallel()

	a, _, _, _ := newDiffTestActor(t)
	ctx := context.Background()

	reader := &mockSnapshotReader{err: errSnapshotRead}
	a.cfg.UTXOSnapshotReader = fn.Some[UTXOSnapshotReader](reader)

	err := a.reseedUTXOSnapshot(ctx)
	require.ErrorIs(t, err, errSnapshotRead)
}
