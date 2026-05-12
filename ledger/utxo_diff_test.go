package ledger

import (
	"context"
	"encoding/binary"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
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
func (m *mockUTXOLister) ListUnspent(_ context.Context) ([]WalletUTXO, error) {
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
// tests can assert the shape of the audit trail. The store is
// keyed on (outpoint, event) so a second InsertWalletUTXOLog
// call with the same key is a silent no-op that returns
// rowcount zero, matching the production ON CONFLICT DO
// NOTHING semantics the classifier relies on.
type mockAuditStore struct {
	mu      sync.Mutex
	entries []WalletUTXOLogEntry
	seen    map[mockAuditKey]struct{}
}

// mockAuditKey keys the mock store on the same (outpoint,
// event) triple the production schema's UNIQUE constraint uses.
type mockAuditKey struct {
	outpoint wire.OutPoint
	event    UTXOAuditEvent
}

// InsertWalletUTXOLog appends the entry to the in-memory record
// and returns 1 for new rows, 0 for duplicates that hit the
// (outpoint, event) uniqueness guard.
func (m *mockAuditStore) InsertWalletUTXOLog(_ context.Context,
	entry WalletUTXOLogEntry) (int64, error) {

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.seen == nil {
		m.seen = make(map[mockAuditKey]struct{})
	}

	key := mockAuditKey{
		outpoint: entry.Outpoint,
		event:    entry.Event,
	}
	if _, ok := m.seen[key]; ok {
		return 0, nil
	}

	m.seen[key] = struct{}{}
	m.entries = append(m.entries, entry)

	return 1, nil
}

// PromotePendingWalletUTXOLog promotes every in-memory row
// classified as UTXOClassPending whose block_height is strictly
// below the watermark. Mirrors the production sqlc query so
// unit tests can exercise the reconciliation pass without
// wiring a database.
func (m *mockAuditStore) PromotePendingWalletUTXOLog(_ context.Context,
	watermark int64) ([]WalletUTXOLogEntry, error) {

	m.mu.Lock()
	defer m.mu.Unlock()

	var promoted []WalletUTXOLogEntry
	for i := range m.entries {
		entry := &m.entries[i]
		if entry.Classification != UTXOClassPending {
			continue
		}
		if entry.BlockHeight >= watermark {
			continue
		}

		switch entry.Event {
		case UTXOAuditCreated:
			entry.Classification = UTXOClassDeposit

		case UTXOAuditSpent:
			entry.Classification = UTXOClassWithdrawal
		}

		promoted = append(promoted, *entry)
	}

	return promoted, nil
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
func newDiffTestActor(t *testing.T) (*LedgerActor, *mockLedgerStore,
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
// write external_deposit ledger entries on the seeding pass:
// those UTXOs predate the actor's snapshot, and the classifier
// books them only after the one-block grace window elapses
// (covered by TestUTXODiffPromotesPendingDeposits below). The
// classifier's default is to insert rows as 'pending' so a
// concurrent round / sweep handler has a chance to attribute
// them before the next block's reconciliation promotes them.
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
	// the pending classification. Reconciliation on the
	// next block promotes them.
	rows := audit.get()
	require.Len(t, rows, 2)
	for _, r := range rows {
		require.Equal(t, UTXOAuditCreated, r.Event)
		require.Equal(t, UTXOClassPending, r.Classification)
		require.Equal(t, int64(800_000), r.BlockHeight)
	}

	// No ledger entries on the seeding pass: the classifier's
	// grace window has not elapsed.
	require.Empty(
		t, ledger.getEntries(),
		"initial snapshot must not book deposits until reconciliation",
	)
}

// TestUTXODiffDetectsNewDeposit verifies the steady-state case:
// after a snapshot is seeded, a subsequent block that adds a
// UTXO produces a 'created' audit row classified as pending.
// The next block's reconciliation pass promotes the seed's
// pending row and books an external_deposit ledger leg; the
// row inserted this block stays pending until the block after.
func TestUTXODiffDetectsNewDeposit(t *testing.T) {
	t.Parallel()

	a, ledger, lister, audit := newDiffTestActor(t)
	ctx := context.Background()

	// Seeding block.
	seed := []WalletUTXO{
		{
			Outpoint: makeOutpoint(1),
			Amount:   10_000,
		},
	}
	lister.set(seed)
	require.NoError(
		t,
		a.handleBlockEpoch(
			ctx, &BlockEpochMsg{
				BlockHeight: 800_000,
			},
		),
	)

	// After seeding, no ledger entries (grace window) and one
	// pending audit row.
	require.Equal(t, 0, len(ledger.getEntries()))
	require.Equal(t, 1, len(audit.get()))

	// Next block: new UTXO appears alongside the existing
	// one. Reconciliation promotes the seed's pending row to
	// deposit and books an external_deposit ledger leg; the
	// new UTXO goes in as pending.
	lister.set([]WalletUTXO{
		{Outpoint: makeOutpoint(1), Amount: 10_000},
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

	// One ledger entry: the promoted seed row's external
	// deposit. The new pending row has not yet reconciled.
	entries := ledger.getEntries()
	require.Len(t, entries, 1)
	require.Equal(t, "external_deposit",
		string(entries[0].EventType))

	// Audit now has two rows total. The seed row is promoted
	// to 'deposit'; the newly-created outpoint 9 is pending.
	rows := audit.get()
	require.Len(t, rows, 2)

	var seedRow, newRow *WalletUTXOLogEntry
	for i, r := range rows {
		switch r.Outpoint {
		case makeOutpoint(1):
			seedRow = &rows[i]

		case makeOutpoint(9):
			newRow = &rows[i]
		}
	}
	require.NotNil(t, seedRow)
	require.NotNil(t, newRow)
	require.Equal(t, UTXOClassDeposit, seedRow.Classification)
	require.Equal(t, UTXOAuditCreated, newRow.Event)
	require.Equal(t, UTXOClassPending, newRow.Classification)
	require.Equal(t, btcutil.Amount(50_000), newRow.Amount)
	require.Equal(t, int64(800_001), newRow.BlockHeight)
}

// TestUTXODiffDetectsSpend verifies that a disappeared UTXO
// surfaces as a 'spent' audit row in the 'pending'
// classification on the same block, and that the next block's
// reconciliation pass promotes both the seed's 'created'
// pending rows (external_deposit) and the 'spent' pending row
// (external_withdrawal) into terminal states with matching
// external_* ledger legs. Only unattributed spends land this
// way: a round / sweep handler that pre-inserts its
// attribution rows pre-empts the classifier.
func TestUTXODiffDetectsSpend(t *testing.T) {
	t.Parallel()

	a, ledger, lister, audit := newDiffTestActor(t)
	ctx := context.Background()

	// Seed.
	lister.set([]WalletUTXO{
		{Outpoint: makeOutpoint(1), Amount: 10_000},
		{Outpoint: makeOutpoint(2), Amount: 25_000},
	})
	require.NoError(
		t,
		a.handleBlockEpoch(
			ctx, &BlockEpochMsg{
				BlockHeight: 800_000,
			},
		),
	)
	require.Empty(t, ledger.getEntries())

	// Next block: only one UTXO remains. Reconciliation
	// promotes the 2 seed 'created' rows to 'deposit' and
	// books external_deposit for each. The freshly-spent
	// outpoint 2 lands as a pending spent row.
	lister.set([]WalletUTXO{
		{Outpoint: makeOutpoint(1), Amount: 10_000},
	})
	require.NoError(
		t,
		a.handleBlockEpoch(
			ctx, &BlockEpochMsg{
				BlockHeight: 800_001,
			},
		),
	)

	// Two external_deposit entries from seed promotion.
	entries := ledger.getEntries()
	require.Len(t, entries, 2)
	for _, e := range entries {
		require.Equal(t, "external_deposit",
			string(e.EventType))
	}

	// Audit has 3 rows total (2 created on seed, now
	// promoted; 1 spent, still pending).
	rows := audit.get()
	require.Len(t, rows, 3)
	var spent int
	var spentRow WalletUTXOLogEntry
	for _, r := range rows {
		if r.Event == UTXOAuditSpent {
			spent++
			spentRow = r
		}
	}
	require.Equal(t, 1, spent)
	require.Equal(t, makeOutpoint(2), spentRow.Outpoint)
	require.Equal(t, UTXOClassPending, spentRow.Classification)
	require.Equal(t, btcutil.Amount(25_000), spentRow.Amount)
	require.Equal(t, int64(800_001), spentRow.BlockHeight)

	// One more block drains the pending spent row into
	// 'withdrawal' and books the matching
	// external_withdrawal leg.
	require.NoError(
		t,
		a.handleBlockEpoch(
			ctx, &BlockEpochMsg{
				BlockHeight: 800_002,
			},
		),
	)
	entries = ledger.getEntries()
	require.Len(t, entries, 3)
	require.Equal(t, "external_withdrawal",
		string(entries[2].EventType))
}

// TestUTXODiffNoopWithoutLister verifies that when
// WalletUTXOLister is None, handleBlockEpoch is a log-only
// no-op. No audit rows, no ledger entries.
func TestUTXODiffNoopWithoutLister(t *testing.T) {
	t.Parallel()

	a, ledger := newTestActor(t)
	ctx := context.Background()

	require.NoError(
		t,
		a.handleBlockEpoch(
			ctx, &BlockEpochMsg{
				BlockHeight: 800_000,
			},
		),
	)
	require.Empty(t, ledger.getEntries())
}

// TestUTXODiffNoopWithoutAuditStore verifies that an absent
// audit store does not break the diff loop: the in-memory
// snapshot still tracks forward across blocks so that when an
// audit store is later wired, subsequent diffs start from the
// right baseline. No ledger entries are written regardless
// (the diff is audit-only).
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
	require.NoError(
		t,
		a.handleBlockEpoch(
			ctx, &BlockEpochMsg{
				BlockHeight: 800_000,
			},
		),
	)

	// Movement on block 2.
	lister.set([]WalletUTXO{
		{Outpoint: makeOutpoint(1), Amount: 10_000},
		{Outpoint: makeOutpoint(2), Amount: 30_000},
	})
	require.NoError(
		t,
		a.handleBlockEpoch(
			ctx, &BlockEpochMsg{
				BlockHeight: 800_001,
			},
		),
	)

	require.Empty(t, ledger.getEntries())

	// Snapshot advanced to include both UTXOs.
	require.Len(t, a.utxo.prev, 2)
	require.True(t, a.utxo.seeded)
}

// TestUTXODiffListerErrorPreservesSnapshot verifies that when
// ListUnspent fails, the previous snapshot is NOT replaced. A
// transient wallet error must not drop state and cause the
// next successful diff to treat every UTXO as new. The error
// block still runs reconciliation first, which promotes the
// seed's pending row and books a single external_deposit --
// that happens before the lister failure surfaces, and the
// retry on the recovery block produces no additional entries
// (the set is unchanged and the pending row is gone).
func TestUTXODiffListerErrorPreservesSnapshot(t *testing.T) {
	t.Parallel()

	a, ledger, lister, _ := newDiffTestActor(t)
	ctx := context.Background()

	// Seed.
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

	// Simulate a wallet error on the next block. The
	// reconciliation pass runs BEFORE the lister call, so
	// the seed's pending row still gets promoted to deposit
	// and an external_deposit lands in the ledger before
	// the lister error propagates.
	lister.err = context.DeadlineExceeded
	err := a.handleBlockEpoch(ctx, &BlockEpochMsg{
		BlockHeight: 800_001,
	})
	require.Error(t, err)

	// Recover: wallet works again, returning the same
	// snapshot. No new entries should be produced since the
	// pending row was already drained on the failed block.
	lister.err = nil
	require.NoError(
		t,
		a.handleBlockEpoch(
			ctx, &BlockEpochMsg{
				BlockHeight: 800_002,
			},
		),
	)

	// Exactly one external_deposit from the reconciliation
	// pass; the set is otherwise unchanged across all three
	// blocks.
	entries := ledger.getEntries()
	require.Len(t, entries, 1)
	require.Equal(t, "external_deposit",
		string(entries[0].EventType))
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
		t, uint32(0xdeadbeef), binary.LittleEndian.Uint32(key[32:]),
	)
}

// TestRoundConfirmedAttributesSuppressExternalLegs wires the
// happy path: a round handler pre-inserts a round_change audit
// row for a given outpoint BEFORE the matching BlockEpochMsg
// drains; the diff loop then observes that outpoint as a new
// wallet UTXO, tries to insert a 'pending' row, finds the
// attribution row already there, and the next block's
// reconciliation pass does NOT promote anything because no
// pending rows exist. Net effect: zero external_* ledger legs
// for a round-attributed outpoint, proving the classifier's
// double-counting guard holds under the intended producer -
// consumer ordering.
func TestRoundConfirmedAttributesSuppressExternalLegs(t *testing.T) {
	t.Parallel()

	a, ledger, lister, audit := newDiffTestActor(t)
	ctx := context.Background()

	roundChangeOp := makeOutpoint(77)
	roundID := [16]byte{0xAB, 0xCD}

	// Step 1: round handler drains first and pre-inserts a
	// round_change audit row for the change output.
	require.NoError(
		t,
		a.handleRoundConfirmed(
			ctx, &RoundConfirmedMsg{
				RoundID:            roundID,
				TotalVTXOAmountSat: 100_000,
				VTXOCount:          1,
				BlockHeight:        800_000,
				ChangeOutpoints: []wire.OutPoint{
					roundChangeOp,
				},
				BoardingNewSat: 100_000,
			},
		),
	)

	rows := audit.get()
	require.Len(t, rows, 1)
	require.Equal(t, UTXOClassRoundChange, rows[0].Classification)
	require.Equal(t, roundID[:], rows[0].SourceID)

	// Step 2: the block epoch fires and the wallet now lists
	// the change output as a new UTXO. The diff loop attempts
	// to insert a 'pending' row but hits the pre-attributed
	// round_change row and its insert is a silent no-op.
	lister.set([]WalletUTXO{
		{Outpoint: roundChangeOp, Amount: 15_000},
	})
	require.NoError(
		t,
		a.handleBlockEpoch(
			ctx, &BlockEpochMsg{
				BlockHeight: 800_001,
			},
		),
	)

	// Step 3: another block drains. Because no pending row
	// exists for the round-attributed outpoint, reconciliation
	// has nothing to promote and the ledger stays at zero
	// external_* legs from this path. (The capital-committed
	// leg from step 1 is a different account; we scope the
	// assertion to external_funding movements.)
	require.NoError(
		t,
		a.handleBlockEpoch(
			ctx, &BlockEpochMsg{
				BlockHeight: 800_002,
			},
		),
	)

	for _, e := range ledger.getEntries() {
		require.NotEqual(t, "external_deposit",
			string(e.EventType))
		require.NotEqual(t, "external_withdrawal",
			string(e.EventType))
	}

	// The audit log has exactly one row: the handler's
	// round_change pre-insert. The diff loop's pending insert
	// was deduped by UNIQUE(hash, index, event).
	require.Len(t, audit.get(), 1)
}

// TestSweepCompletedAttributesSuppressExternalLegs is the
// sweep-side analogue of the above test: a sweep handler
// pre-inserts a sweep_consumption row for a consumed input
// and a sweep_return row for the return output, and the diff
// loop that later observes those outpoints short-circuits on
// the existing rows without booking external_* legs.
func TestSweepCompletedAttributesSuppressExternalLegs(t *testing.T) {
	t.Parallel()

	a, ledger, lister, _ := newDiffTestActor(t)
	ctx := context.Background()

	consumedOp := makeOutpoint(55)
	returnOp := makeOutpoint(56)
	batchID := [16]byte{0xFE, 0xED}

	// Seed the tracker so the consumed outpoint has a live
	// baseline; otherwise the diff would never consider it
	// "spent".
	lister.set([]WalletUTXO{
		{Outpoint: consumedOp, Amount: 50_000},
	})
	require.NoError(
		t,
		a.handleBlockEpoch(
			ctx, &BlockEpochMsg{
				BlockHeight: 800_000,
			},
		),
	)

	// Promote the seed's pending row first so the assertion
	// below only counts sweep-path entries. (The seed rows
	// get reconciled to deposit and book a single
	// external_deposit.)
	require.NoError(
		t,
		a.handleBlockEpoch(
			ctx, &BlockEpochMsg{
				BlockHeight: 800_001,
			},
		),
	)

	preSweepEntries := len(ledger.getEntries())
	require.Equal(t, 1, preSweepEntries)

	// Step 1: sweep handler drains first and pre-inserts
	// sweep_consumption + sweep_return rows.
	require.NoError(
		t,
		a.handleSweepCompleted(
			ctx, &SweepCompletedMsg{
				BatchID:            batchID,
				ReclaimedAmountSat: 50_000,
				Count:              1,
				BlockHeight:        800_002,
				FeeRateSatVB:       20,
				ConsumedOutpoints:  []wire.OutPoint{consumedOp},
				ReturnOutpoints:    []wire.OutPoint{returnOp},
			},
		),
	)

	// Step 2: the wallet state now reflects the sweep: the
	// consumed outpoint disappeared, the return outpoint
	// appeared.
	lister.set([]WalletUTXO{
		{Outpoint: returnOp, Amount: 49_000},
	})
	require.NoError(
		t,
		a.handleBlockEpoch(
			ctx, &BlockEpochMsg{
				BlockHeight: 800_002,
			},
		),
	)

	// Step 3: another block drains so any lingering pending
	// row gets reconciled. None should exist for either the
	// consumed or return outpoint.
	require.NoError(
		t,
		a.handleBlockEpoch(
			ctx, &BlockEpochMsg{
				BlockHeight: 800_003,
			},
		),
	)

	// No new external_* entries beyond the seed's promotion
	// from before the sweep.
	require.Equal(
		t, preSweepEntries, countExternalEntries(ledger),
	)
}

// countExternalEntries returns the number of external_* ledger
// legs in the mock store. Keeps the assertions in the
// classifier tests tight to the event types under test.
func countExternalEntries(store *mockLedgerStore) int {
	var n int
	for _, e := range store.getEntries() {
		switch string(e.EventType) {
		case "external_deposit", "external_withdrawal":
			n++
		}
	}

	return n
}
