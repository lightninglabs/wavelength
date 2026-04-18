package ledger

import (
	"context"
	"encoding/binary"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo/fees"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// mockUTXOLister is a test-only WalletUTXOLister whose
// ListUnspent return value is driven by a setter. Tests queue
// up the sequence of snapshots they want processed block by
// block.
type mockUTXOLister struct {
	mu       sync.Mutex
	snapshot []WalletUTXO
	err      error
}

// ListUnspent returns the snapshot currently configured by
// the test.
func (m *mockUTXOLister) ListUnspent(_ context.Context) (
	[]WalletUTXO, error) {

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.err != nil {
		return nil, m.err
	}

	out := make([]WalletUTXO, len(m.snapshot))
	copy(out, m.snapshot)

	return out, nil
}

// set installs the next snapshot the next ListUnspent call
// will return.
func (m *mockUTXOLister) set(utxos []WalletUTXO) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.snapshot = utxos
}

// mockAuditStore records every InsertWalletUTXOLog call so
// tests can assert the shape of the audit trail.
type mockAuditStore struct {
	mu      sync.Mutex
	entries []WalletUTXOLogEntry
}

// InsertWalletUTXOLog appends the entry to the in-memory
// record.
func (m *mockAuditStore) InsertWalletUTXOLog(_ context.Context,
	entry WalletUTXOLogEntry) error {

	m.mu.Lock()
	defer m.mu.Unlock()

	m.entries = append(m.entries, entry)

	return nil
}

// get returns a copy of all recorded audit entries.
func (m *mockAuditStore) get() []WalletUTXOLogEntry {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]WalletUTXOLogEntry, len(m.entries))
	copy(out, m.entries)

	return out
}

// makeOutpoint constructs a stable OutPoint from a single seed
// byte so test UTXOs are easy to read in failure messages.
func makeOutpoint(seed byte) wire.OutPoint {
	var h chainhash.Hash
	h[0] = seed

	return wire.OutPoint{Hash: h, Index: uint32(seed)}
}

// newDiffTestActor wires a LedgerActor with the UTXO diff
// subsystem enabled via mocks. Unlike newTestActor, this
// configuration sets WalletUTXOLister and UTXOAuditStore so
// the per-block diff path exercises the full flow.
func newDiffTestActor(t *testing.T) (
	*LedgerActor, *mockLedgerStore,
	*mockUTXOLister, *mockAuditStore) {

	t.Helper()

	a, ledger := newTestActor(t)
	lister := &mockUTXOLister{}
	audit := &mockAuditStore{}

	a.cfg.WalletUTXOLister = fn.Some[WalletUTXOLister](lister)
	a.cfg.UTXOAuditStore = fn.Some[UTXOAuditStore](audit)

	return a, ledger, lister, audit
}

// TestUTXODiffSeedsWithoutLedgerEntries verifies that the first
// BlockEpoch after startup populates the audit log but does not
// write external_deposit ledger entries: those UTXOs predate
// the actor's snapshot and have prior origin elsewhere.
func TestUTXODiffSeedsWithoutLedgerEntries(t *testing.T) {
	t.Parallel()

	a, ledger, lister, audit := newDiffTestActor(t)
	ctx := context.Background()

	lister.set([]WalletUTXO{
		{Outpoint: makeOutpoint(1), Amount: 10_000},
		{Outpoint: makeOutpoint(2), Amount: 25_000},
	})

	err := a.handleBlockEpoch(ctx, &BlockEpochMsg{
		BlockHeight: 800_000,
	})
	require.NoError(t, err)

	// Audit rows for both UTXOs land under 'created' with
	// the deposit classification.
	rows := audit.get()
	require.Len(t, rows, 2)
	for _, r := range rows {
		require.Equal(t, UTXOAuditCreated, r.Event)
		require.Equal(t, UTXOClassDeposit, r.Classification)
		require.Equal(t, int64(800_000), r.BlockHeight)
	}

	// No ledger entries on the seeding pass.
	require.Empty(t, ledger.getEntries(),
		"initial snapshot must not double-book deposits")
}

// TestUTXODiffDetectsNewDeposit verifies the steady-state case:
// after a snapshot is seeded, a subsequent block that adds a
// UTXO produces a 'created' audit row AND an external_deposit
// ledger entry keyed on the outpoint.
func TestUTXODiffDetectsNewDeposit(t *testing.T) {
	t.Parallel()

	a, ledger, lister, audit := newDiffTestActor(t)
	ctx := context.Background()

	// Seeding block.
	seed := []WalletUTXO{
		{Outpoint: makeOutpoint(1), Amount: 10_000},
	}
	lister.set(seed)
	require.NoError(t, a.handleBlockEpoch(ctx, &BlockEpochMsg{
		BlockHeight: 800_000,
	}))

	// Clear records so the next assertion is clean.
	ledgerBefore := len(ledger.getEntries())
	auditBefore := len(audit.get())
	require.Equal(t, 0, ledgerBefore)
	require.Equal(t, 1, auditBefore)

	// Next block: new UTXO appears alongside the existing
	// one.
	lister.set([]WalletUTXO{
		{Outpoint: makeOutpoint(1), Amount: 10_000},
		{Outpoint: makeOutpoint(9), Amount: 50_000},
	})
	require.NoError(t, a.handleBlockEpoch(ctx, &BlockEpochMsg{
		BlockHeight: 800_001,
	}))

	// One new audit row, one new ledger entry.
	entries := ledger.getEntries()
	require.Len(t, entries, 1)
	require.Equal(t, fees.LedgerEventExternalDeposit,
		entries[0].EventType)
	require.Equal(t, btcutil.Amount(50_000), entries[0].Amount)
	require.Equal(t, fees.AccountTreasuryWallet,
		entries[0].DebitAccount)
	require.Equal(t, fees.AccountExternalFunding,
		entries[0].CreditAccount)

	// Idempotency key is outpoint-derived so a replay of the
	// same diff does not double-book.
	wantKey := outpointKey(makeOutpoint(9))
	require.Equal(t, wantKey, entries[0].IdempotencyKey)

	// Audit now has two rows total (one from the seed, one
	// from this block).
	require.Len(t, audit.get(), 2)
}

// TestUTXODiffDetectsSpend verifies that a disappeared UTXO
// surfaces as a 'spent' audit row AND an external_withdrawal
// ledger entry.
func TestUTXODiffDetectsSpend(t *testing.T) {
	t.Parallel()

	a, ledger, lister, audit := newDiffTestActor(t)
	ctx := context.Background()

	// Seed.
	lister.set([]WalletUTXO{
		{Outpoint: makeOutpoint(1), Amount: 10_000},
		{Outpoint: makeOutpoint(2), Amount: 25_000},
	})
	require.NoError(t, a.handleBlockEpoch(ctx, &BlockEpochMsg{
		BlockHeight: 800_000,
	}))
	require.Empty(t, ledger.getEntries())

	// Next block: only one UTXO remains.
	lister.set([]WalletUTXO{
		{Outpoint: makeOutpoint(1), Amount: 10_000},
	})
	require.NoError(t, a.handleBlockEpoch(ctx, &BlockEpochMsg{
		BlockHeight: 800_001,
	}))

	// Spent entry on the ledger for outpoint 2.
	entries := ledger.getEntries()
	require.Len(t, entries, 1)
	require.Equal(t, fees.LedgerEventExternalWithdrawal,
		entries[0].EventType)
	require.Equal(t, btcutil.Amount(25_000), entries[0].Amount)
	require.Equal(t, fees.AccountExternalFunding,
		entries[0].DebitAccount)
	require.Equal(t, fees.AccountTreasuryWallet,
		entries[0].CreditAccount)

	// Audit now has 3 rows (2 created on seed + 1 spent).
	rows := audit.get()
	require.Len(t, rows, 3)
	var spent int
	for _, r := range rows {
		if r.Event == UTXOAuditSpent {
			spent++
		}
	}
	require.Equal(t, 1, spent)
}

// TestUTXODiffNoopWithoutLister verifies that when
// WalletUTXOLister is None, handleBlockEpoch is a log-only
// no-op. No audit rows, no ledger entries.
func TestUTXODiffNoopWithoutLister(t *testing.T) {
	t.Parallel()

	a, ledger := newTestActor(t)
	ctx := context.Background()

	require.NoError(t, a.handleBlockEpoch(ctx, &BlockEpochMsg{
		BlockHeight: 800_000,
	}))
	require.Empty(t, ledger.getEntries())
}

// TestUTXODiffNoopWithoutAuditStore verifies that audit-store-
// absent deployments still book ledger entries. Audit log is
// an observability layer; ledger accounting must not depend on
// it being wired.
func TestUTXODiffNoopWithoutAuditStore(t *testing.T) {
	t.Parallel()

	a, ledger := newTestActor(t)
	lister := &mockUTXOLister{}
	a.cfg.WalletUTXOLister = fn.Some[WalletUTXOLister](lister)

	ctx := context.Background()

	// Seed.
	lister.set([]WalletUTXO{
		{Outpoint: makeOutpoint(1), Amount: 10_000},
	})
	require.NoError(t, a.handleBlockEpoch(ctx, &BlockEpochMsg{
		BlockHeight: 800_000,
	}))

	// Deposit on block 2.
	lister.set([]WalletUTXO{
		{Outpoint: makeOutpoint(1), Amount: 10_000},
		{Outpoint: makeOutpoint(2), Amount: 30_000},
	})
	require.NoError(t, a.handleBlockEpoch(ctx, &BlockEpochMsg{
		BlockHeight: 800_001,
	}))

	entries := ledger.getEntries()
	require.Len(t, entries, 1)
	require.Equal(t, fees.LedgerEventExternalDeposit,
		entries[0].EventType)
}

// TestUTXODiffListerErrorPreservesSnapshot verifies that when
// ListUnspent fails, the previous snapshot is NOT replaced. A
// transient wallet error must not drop state and cause the
// next successful diff to treat every UTXO as new.
func TestUTXODiffListerErrorPreservesSnapshot(t *testing.T) {
	t.Parallel()

	a, ledger, lister, _ := newDiffTestActor(t)
	ctx := context.Background()

	// Seed.
	lister.set([]WalletUTXO{
		{Outpoint: makeOutpoint(1), Amount: 10_000},
	})
	require.NoError(t, a.handleBlockEpoch(ctx, &BlockEpochMsg{
		BlockHeight: 800_000,
	}))

	// Simulate a wallet error on the next block.
	lister.err = context.DeadlineExceeded
	err := a.handleBlockEpoch(ctx, &BlockEpochMsg{
		BlockHeight: 800_001,
	})
	require.Error(t, err)

	// Recover: wallet works again, returning the same
	// snapshot. No new entries should be produced.
	lister.err = nil
	require.NoError(t, a.handleBlockEpoch(ctx, &BlockEpochMsg{
		BlockHeight: 800_002,
	}))

	// No ledger activity because the set is unchanged.
	require.Empty(t, ledger.getEntries())
}

// TestOutpointKeyShape pins the wire format of the outpoint-
// derived idempotency key: 32 bytes of hash + 4 bytes of
// little-endian index. This is the shape that the partial
// unique index sees, so breaking it would silently let dupes
// land in the ledger.
func TestOutpointKeyShape(t *testing.T) {
	t.Parallel()

	var h chainhash.Hash
	for i := range h {
		h[i] = byte(i + 1)
	}
	op := wire.OutPoint{Hash: h, Index: 0xdeadbeef}

	key := outpointKey(op)
	require.Len(t, key, 36)
	require.Equal(t, h[:], key[:32])
	require.Equal(
		t, uint32(0xdeadbeef),
		binary.LittleEndian.Uint32(key[32:]),
	)
}
