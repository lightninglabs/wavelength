package db

import (
	"database/sql"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/db/sqlc"
	"github.com/lightninglabs/wavelength/ledger"
	"github.com/stretchr/testify/require"
)

// newUTXOAuditStoreForTest creates a UTXOAuditStoreDB backed by
// a fresh test database with all migrations applied.
func newUTXOAuditStoreForTest(t *testing.T) *UTXOAuditStoreDB {
	t.Helper()

	store, _ := newUTXOAuditStoreAndDBForTest(t)

	return store
}

// newUTXOAuditStoreAndDBForTest creates a UTXOAuditStoreDB and returns its
// backing database so tests can exercise storage-layer edge cases directly.
func newUTXOAuditStoreAndDBForTest(t *testing.T) (*UTXOAuditStoreDB, *BaseDB) {
	t.Helper()

	db := NewTestDB(t)

	txExec := NewTransactionExecutor(
		db.BaseDB,
		func(tx *sql.Tx) *sqlc.Queries {
			return db.WithTx(tx)
		},
		btclog.Disabled,
	)

	return &UTXOAuditStoreDB{
		TransactionExecutor: txExec,
	}, db.BaseDB
}

// makeOutpoint returns a deterministic 32-byte outpoint hash
// seeded with the given byte so tests can construct distinct
// outpoints without collisions.
func makeOutpoint(seed byte) []byte {
	buf := make([]byte, 32)
	for i := range buf {
		buf[i] = seed
	}

	return buf
}

// TestUTXOAuditEnumsSeeded verifies that the migration seeds the
// classification and event enum tables with the expected values.
func TestUTXOAuditEnumsSeeded(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newUTXOAuditStoreForTest(t)

	// Insert an entry using each seeded classification and
	// event to verify all 5 x 2 = 10 pairs are accepted by
	// the FK constraints.
	classifications := []string{
		"deposit", "sweep_return", "round_funding",
		"change", "unknown",
	}
	events := []string{"created", "spent"}

	now := time.Now().Unix()
	seed := byte(0)

	for _, classification := range classifications {
		for _, event := range events {
			seed++
			err := store.InsertUTXOAuditEntry(
				ctx, ledger.UTXOAuditEntry{
					OutpointHash:  makeOutpoint(seed),
					OutpointIndex: 0,
					AmountSat:     10_000,
					Event:         event,
					BlockHeight:   int32(seed),
					ClassifiedAs:  classification,
					CreatedAt:     now + int64(seed),
				},
			)
			require.NoError(
				t, err, "expected seeded enums to accept "+
					"classification=%q event=%q",
				classification, event,
			)
		}
	}

	// All 10 rows should have landed.
	count, err := store.CountUTXOAuditEntries(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(10), count)
}

// TestUTXOAuditEntryIDsDoNotReuseAfterDelete verifies wallet UTXO audit entry
// IDs remain monotonic even if the current maximum row is deleted.
func TestUTXOAuditEntryIDsDoNotReuseAfterDelete(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, db := newUTXOAuditStoreAndDBForTest(t)
	now := time.Now().Unix()

	first := ledger.UTXOAuditEntry{
		OutpointHash:  makeOutpoint(1),
		OutpointIndex: 0,
		AmountSat:     10_000,
		Event:         "created",
		BlockHeight:   100,
		ClassifiedAs:  "deposit",
		CreatedAt:     now,
	}
	require.NoError(t, store.InsertUTXOAuditEntry(ctx, first))

	entries, err := store.ListUTXOAuditEntries(ctx, 10, 0)
	require.NoError(t, err)
	require.Len(t, entries, 1)

	firstID := entries[0].EntryID
	// There is intentionally no production delete query for this
	// append-only log. The raw SQL keeps this regression test limited to
	// the impossible-in-production row deletion shape that triggers ROWID
	// reuse.
	query := "DELETE FROM wallet_utxo_log WHERE entry_id = ?"
	if db.Backend() == sqlc.BackendTypePostgres {
		query = "DELETE FROM wallet_utxo_log WHERE entry_id = $1"
	}

	_, err = db.ExecContext(ctx, query, firstID)
	require.NoError(t, err)

	second := ledger.UTXOAuditEntry{
		OutpointHash:  makeOutpoint(2),
		OutpointIndex: 0,
		AmountSat:     20_000,
		Event:         "created",
		BlockHeight:   101,
		ClassifiedAs:  "deposit",
		CreatedAt:     now + 1,
	}
	require.NoError(t, store.InsertUTXOAuditEntry(ctx, second))

	entries, err = store.ListUTXOAuditEntries(ctx, 10, 0)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, firstID+1, entries[0].EntryID)
}

// TestUTXOAuditFKRejection verifies that the classification and
// event FK constraints reject unseeded values.
func TestUTXOAuditFKRejection(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newUTXOAuditStoreForTest(t)

	now := time.Now().Unix()

	// Unknown classification should be rejected.
	err := store.InsertUTXOAuditEntry(
		ctx, ledger.UTXOAuditEntry{
			OutpointHash:  makeOutpoint(1),
			OutpointIndex: 0,
			AmountSat:     1000,
			Event:         "created",
			BlockHeight:   100,
			ClassifiedAs:  "teleported_from_mars",
			CreatedAt:     now,
		},
	)
	require.Error(
		t, err,
		"FK on utxo_classifications should reject unseeded value",
	)

	// Unknown event should be rejected.
	err = store.InsertUTXOAuditEntry(
		ctx, ledger.UTXOAuditEntry{
			OutpointHash:  makeOutpoint(2),
			OutpointIndex: 0,
			AmountSat:     1000,
			Event:         "incinerated",
			BlockHeight:   100,
			ClassifiedAs:  "deposit",
			CreatedAt:     now,
		},
	)
	require.Error(
		t, err, "FK on utxo_events should reject unseeded value",
	)
}

// TestUTXOAuditInsertAndList verifies round-trip insertion and
// paginated listing.
func TestUTXOAuditInsertAndList(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newUTXOAuditStoreForTest(t)

	now := time.Now().Unix()

	// Insert three entries at ascending timestamps.
	entries := []ledger.UTXOAuditEntry{
		{
			OutpointHash:  makeOutpoint(1),
			OutpointIndex: 0,
			AmountSat:     50_000,
			Event:         "created",
			BlockHeight:   100,
			ClassifiedAs:  "deposit",
			CreatedAt:     now,
		},
		{
			OutpointHash:  makeOutpoint(2),
			OutpointIndex: 1,
			AmountSat:     25_000,
			Event:         "created",
			BlockHeight:   100,
			ClassifiedAs:  "change",
			CreatedAt:     now + 1,
		},
		{
			OutpointHash:  makeOutpoint(1),
			OutpointIndex: 0,
			AmountSat:     50_000,
			Event:         "spent",
			BlockHeight:   105,
			ClassifiedAs:  "round_funding",
			CreatedAt:     now + 2,
		},
	}

	for _, e := range entries {
		err := store.InsertUTXOAuditEntry(ctx, e)
		require.NoError(t, err)
	}

	// List all entries (descending by created_at, so the
	// spend at now+2 should come first).
	all, err := store.ListUTXOAuditEntries(ctx, 100, 0)
	require.NoError(t, err)
	require.Len(t, all, 3)
	require.Equal(t, "spent", all[0].Event)
	require.Equal(t, "round_funding", all[0].ClassifiedAs)
	require.Equal(t, int64(50_000), all[0].AmountSat)
	require.Equal(t, "change", all[1].ClassifiedAs)
	require.Equal(t, "deposit", all[2].ClassifiedAs)

	// Count should match.
	count, err := store.CountUTXOAuditEntries(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(3), count)
}

// TestUTXOAuditListByBlock verifies the block_height filter and
// its ordering contract (ORDER BY entry_id).
func TestUTXOAuditListByBlock(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newUTXOAuditStoreForTest(t)

	now := time.Now().Unix()

	// Block 100 gets 2 entries, block 200 gets 1.
	inserts := []ledger.UTXOAuditEntry{
		{
			OutpointHash:  makeOutpoint(1),
			OutpointIndex: 0,
			AmountSat:     1000,
			Event:         "created",
			BlockHeight:   100,
			ClassifiedAs:  "deposit",
			CreatedAt:     now,
		},
		{
			OutpointHash:  makeOutpoint(2),
			OutpointIndex: 0,
			AmountSat:     2000,
			Event:         "created",
			BlockHeight:   200,
			ClassifiedAs:  "deposit",
			CreatedAt:     now + 1,
		},
		{
			OutpointHash:  makeOutpoint(3),
			OutpointIndex: 0,
			AmountSat:     3000,
			Event:         "created",
			BlockHeight:   100,
			ClassifiedAs:  "change",
			CreatedAt:     now + 2,
		},
	}

	for _, e := range inserts {
		require.NoError(t, store.InsertUTXOAuditEntry(ctx, e))
	}

	// Block 100: two entries in entry_id order.
	block100, err := store.ListUTXOAuditEntriesByBlock(ctx, 100)
	require.NoError(t, err)
	require.Len(t, block100, 2)
	require.Equal(t, int64(1000), block100[0].AmountSat)
	require.Equal(t, int64(3000), block100[1].AmountSat)

	// Block 200: one entry.
	block200, err := store.ListUTXOAuditEntriesByBlock(ctx, 200)
	require.NoError(t, err)
	require.Len(t, block200, 1)
	require.Equal(t, int64(2000), block200[0].AmountSat)

	// Block 999: no entries (empty, not an error).
	empty, err := store.ListUTXOAuditEntriesByBlock(ctx, 999)
	require.NoError(t, err)
	require.Empty(t, empty)
}

// TestUTXOAuditListByClassification verifies the classification
// filter and pagination.
func TestUTXOAuditListByClassification(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newUTXOAuditStoreForTest(t)

	now := time.Now().Unix()

	// Insert 5 entries: 3 deposits, 1 change, 1 unknown.
	for i, c := range []string{
		"deposit", "deposit", "change", "deposit", "unknown",
	} {
		err := store.InsertUTXOAuditEntry(
			ctx, ledger.UTXOAuditEntry{
				OutpointHash:  makeOutpoint(byte(i + 1)),
				OutpointIndex: 0,
				AmountSat:     int64((i + 1) * 1000),
				Event:         "created",
				BlockHeight:   int32(100 + i),
				ClassifiedAs:  c,
				CreatedAt:     now + int64(i),
			},
		)
		require.NoError(t, err)
	}

	// All 3 deposits in one page.
	deposits, err := store.ListUTXOAuditEntriesByClassification(
		ctx, "deposit", 10, 0,
	)
	require.NoError(t, err)
	require.Len(t, deposits, 3)

	// Paginate: first 2, then last 1.
	page1, err := store.ListUTXOAuditEntriesByClassification(
		ctx, "deposit", 2, 0,
	)
	require.NoError(t, err)
	require.Len(t, page1, 2)

	page2, err := store.ListUTXOAuditEntriesByClassification(
		ctx, "deposit", 2, 2,
	)
	require.NoError(t, err)
	require.Len(t, page2, 1)

	// Pages should not overlap.
	require.NotEqual(t, page1[0].EntryID, page2[0].EntryID)
	require.NotEqual(t, page1[1].EntryID, page2[0].EntryID)

	// Single-classification filters still work.
	changeEntries, err := store.ListUTXOAuditEntriesByClassification(
		ctx, "change", 10, 0,
	)
	require.NoError(t, err)
	require.Len(t, changeEntries, 1)
}

// TestUTXOAuditSpendLifecycle documents and verifies the common
// "deposit -> spend" UTXO lifecycle: a UTXO is created by one
// entry, then later marked spent by another entry that carries
// the same outpoint. Both rows remain in the log (append-only).
func TestUTXOAuditSpendLifecycle(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newUTXOAuditStoreForTest(t)

	now := time.Now().Unix()
	outpoint := makeOutpoint(42)

	// Create.
	err := store.InsertUTXOAuditEntry(
		ctx, ledger.UTXOAuditEntry{
			OutpointHash:  outpoint,
			OutpointIndex: 3,
			AmountSat:     1_000_000,
			Event:         "created",
			BlockHeight:   1000,
			ClassifiedAs:  "deposit",
			CreatedAt:     now,
		},
	)
	require.NoError(t, err)

	// Spend some blocks later.
	err = store.InsertUTXOAuditEntry(
		ctx, ledger.UTXOAuditEntry{
			OutpointHash:  outpoint,
			OutpointIndex: 3,
			AmountSat:     1_000_000,
			Event:         "spent",
			BlockHeight:   1050,
			ClassifiedAs:  "round_funding",
			CreatedAt:     now + 3600,
		},
	)
	require.NoError(t, err)

	// Both rows present (append-only).
	count, err := store.CountUTXOAuditEntries(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(2), count)

	// Block 1000 has the create.
	created, err := store.ListUTXOAuditEntriesByBlock(ctx, 1000)
	require.NoError(t, err)
	require.Len(t, created, 1)
	require.Equal(t, "created", created[0].Event)
	require.Equal(t, "deposit", created[0].ClassifiedAs)

	// Block 1050 has the spend.
	spent, err := store.ListUTXOAuditEntriesByBlock(ctx, 1050)
	require.NoError(t, err)
	require.Len(t, spent, 1)
	require.Equal(t, "spent", spent[0].Event)
	require.Equal(t, "round_funding", spent[0].ClassifiedAs)
}

// TestInsertWalletUTXOLogIdempotent verifies that re-inserting
// the same (outpoint, event) pair is a silent no-op. This
// protects the audit log from duplicate rows when the durable
// ledger actor replays unprocessed messages via RestartMessage
// after a crash between the DB write and the mailbox ack.
func TestInsertWalletUTXOLogIdempotent(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newUTXOAuditStoreForTest(t)

	entry := ledger.UTXOAuditEntry{
		OutpointHash:  makeOutpoint(0x11),
		OutpointIndex: 0,
		AmountSat:     50_000,
		Event:         "created",
		BlockHeight:   2000,
		ClassifiedAs:  "deposit",
		CreatedAt:     time.Now().Unix(),
	}

	// First insert succeeds.
	require.NoError(t, store.InsertUTXOAuditEntry(ctx, entry))

	// Second insert with identical (outpoint, event) is a
	// no-op: no error returned and only one row exists.
	require.NoError(t, store.InsertUTXOAuditEntry(ctx, entry))

	count, err := store.CountUTXOAuditEntries(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1), count)
}

// TestInsertWalletUTXOLogCreatedAndSpent verifies that the same
// outpoint can appear twice in the audit log when the events
// differ ('created' then 'spent'); the UNIQUE index is scoped
// to (outpoint, event) not outpoint alone.
func TestInsertWalletUTXOLogCreatedAndSpent(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newUTXOAuditStoreForTest(t)

	outpoint := makeOutpoint(0x22)
	now := time.Now().Unix()

	created := ledger.UTXOAuditEntry{
		OutpointHash:  outpoint,
		OutpointIndex: 1,
		AmountSat:     75_000,
		Event:         "created",
		BlockHeight:   3000,
		ClassifiedAs:  "deposit",
		CreatedAt:     now,
	}
	require.NoError(t, store.InsertUTXOAuditEntry(ctx, created))

	spent := created
	spent.Event = "spent"
	spent.BlockHeight = 3100
	spent.ClassifiedAs = "round_funding"
	spent.CreatedAt = now + 1
	require.NoError(t, store.InsertUTXOAuditEntry(ctx, spent))

	count, err := store.CountUTXOAuditEntries(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(2), count)
}
