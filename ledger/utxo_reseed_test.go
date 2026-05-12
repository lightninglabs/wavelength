package ledger

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
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
func (m *mockSnapshotReader) ListLiveWalletUTXOs(_ context.Context) (
	[]WalletUTXO, int64, error) {

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
func (m *mockSnapshotReader) CountAuditRows(_ context.Context) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.countErr != nil {
		return 0, m.countErr
	}

	return m.rowCount, nil
}

// TestReseedUTXOSnapshotRehydratesTracker verifies the rebuild
// path: Start loads the live UTXO set from the persisted audit
// log so subsequent block epochs diff against the right
// baseline. The UTXO diff is audit-only (ledger legs land with
// the classifier PR), so this test asserts audit-row shape
// rather than ledger entries.
func TestReseedUTXOSnapshotRehydratesTracker(t *testing.T) {
	t.Parallel()

	a, ledgerStore, lister, audit := newDiffTestActor(t)
	ctx := context.Background()

	reader := &mockSnapshotReader{
		utxos: []WalletUTXO{
			{
				Outpoint: makeOutpoint(1),
				Amount:   10_000,
			},
			{
				Outpoint: makeOutpoint(2),
				Amount:   25_000,
			},
		},
		lastBlockHgt: 800_000,
	}
	a.cfg.UTXOSnapshotReader = fn.Some[UTXOSnapshotReader](reader)

	require.NoError(t, a.reseedUTXOSnapshot(ctx))

	// After reseed, the tracker should see two live UTXOs
	// and be flagged seeded.
	require.True(t, a.utxo.seeded)
	require.Len(t, a.utxo.prev, 2)
	require.Equal(
		t, btcutil.Amount(10_000), a.utxo.prev[makeOutpoint(1)],
	)
	require.Equal(
		t, btcutil.Amount(25_000), a.utxo.prev[makeOutpoint(2)],
	)

	// Simulate the post-restart block: a NEW UTXO arrives
	// alongside the two already-known ones. The diff should
	// see only the new UTXO as created (the two existing
	// ones were rehydrated into prev) and write exactly one
	// audit row.
	lister.set([]WalletUTXO{
		{Outpoint: makeOutpoint(1), Amount: 10_000},
		{Outpoint: makeOutpoint(2), Amount: 25_000},
		{Outpoint: makeOutpoint(9), Amount: 50_000},
	})
	require.NoError(
		t,
		a.handleBlockEpoch(
			ctx, &BlockEpochMsg{
				BlockHeight: 800_001,
			},
		),
	)

	// No ledger entries.
	require.Empty(t, ledgerStore.getEntries())

	// One audit row for the new UTXO only -- rehydration
	// kept the pre-existing ones out of the diff.
	rows := audit.get()
	require.Len(t, rows, 1)
	require.Equal(t, UTXOAuditCreated, rows[0].Event)
	require.Equal(t, makeOutpoint(9), rows[0].Outpoint)
	require.Equal(t, btcutil.Amount(50_000), rows[0].Amount)
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

	require.False(t, a.utxo.seeded)
	require.Empty(t, a.utxo.prev)

	// First real block still behaves like a baseline pass:
	// audit rows yes, no ledger booking.
	lister.set([]WalletUTXO{
		{Outpoint: makeOutpoint(1), Amount: 10_000},
	})
	require.NoError(
		t,
		a.handleBlockEpoch(
			ctx, &BlockEpochMsg{
				BlockHeight: 800_000,
			},
		),
	)
	require.Empty(t, ledgerStore.getEntries())
}

// TestReseedUTXOSnapshotEmptyLiveWithHistorySeeds covers a
// long-running deployment whose wallet is currently empty
// (everything swept or pending boarding) but whose audit log
// holds historical rows. A non-zero audit-row count flips
// seeded=true with an empty snapshot so `seeded` remains an
// accurate liveness signal (the audit log is active, not a
// fresh install). Ledger entries are not written by the UTXO
// diff (audit-only until the classifier lands).
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
	require.True(
		t, a.utxo.seeded, "seeded must be true when history exists "+
			"even if live set is empty",
	)
	require.Empty(t, a.utxo.prev)

	// First post-restart block observes a new UTXO; the diff
	// records an audit row. No ledger entry.
	lister.set([]WalletUTXO{
		{Outpoint: makeOutpoint(7), Amount: 75_000},
	})
	require.NoError(
		t,
		a.handleBlockEpoch(
			ctx, &BlockEpochMsg{
				BlockHeight: 900_000,
			},
		),
	)

	require.Empty(t, ledgerStore.getEntries())

	rows := audit.get()
	require.Len(t, rows, 1)
	require.Equal(t, UTXOAuditCreated, rows[0].Event)
	require.Equal(t, makeOutpoint(7), rows[0].Outpoint)
	require.Equal(t, btcutil.Amount(75_000), rows[0].Amount)
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

	require.False(t, a.utxo.seeded)
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
