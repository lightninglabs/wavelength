package db

import (
	"testing"
	"time"

	"github.com/lightninglabs/darepo/db/sqlc"
	"github.com/stretchr/testify/require"
)

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

// TestWalletUTXOLogEnumsSeeded verifies that migration 000011
// seeds the classification and event enum tables with the
// expected values.
func TestWalletUTXOLogEnumsSeeded(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	// Insert an entry using each seeded classification and event
	// to indirectly verify all 5 × 2 = 10 pairs are accepted by
	// the FKs. If any enum row is missing, the FK constraint will
	// reject the insert.
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
			_, err := store.InsertWalletUTXOLog(
				ctx, sqlc.InsertWalletUTXOLogParams{
					OutpointHash:  makeOutpoint(seed),
					OutpointIndex: 0,
					AmountSat:     10_000,
					Event:         event,
					BlockHeight:   int32(seed),
					ClassifiedAs:  classification,
					CreatedAt:     now + int64(seed),
				},
			)
			require.NoError(t, err,
				"expected seeded enums to accept "+
					"classification=%q event=%q",
				classification, event)
		}
	}

	// All 10 rows should have landed.
	count, err := store.CountWalletUTXOLog(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(10), count)
}

// TestWalletUTXOLogFKRejection verifies that the classification
// and event FK constraints reject unseeded values.
func TestWalletUTXOLogFKRejection(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	now := time.Now().Unix()

	// Unknown classification should be rejected.
	_, err := store.InsertWalletUTXOLog(
		ctx, sqlc.InsertWalletUTXOLogParams{
			OutpointHash:  makeOutpoint(1),
			OutpointIndex: 0,
			AmountSat:     1000,
			Event:         "created",
			BlockHeight:   100,
			ClassifiedAs:  "teleported_from_mars",
			CreatedAt:     now,
		},
	)
	require.Error(t, err,
		"FK on utxo_classifications should reject unseeded value")

	// Unknown event should be rejected.
	_, err = store.InsertWalletUTXOLog(
		ctx, sqlc.InsertWalletUTXOLogParams{
			OutpointHash:  makeOutpoint(2),
			OutpointIndex: 0,
			AmountSat:     1000,
			Event:         "incinerated",
			BlockHeight:   100,
			ClassifiedAs:  "deposit",
			CreatedAt:     now,
		},
	)
	require.Error(t, err,
		"FK on utxo_events should reject unseeded value")
}

// TestInsertAndListWalletUTXOLog verifies round-trip insertion
// and paginated listing.
func TestInsertAndListWalletUTXOLog(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	now := time.Now().Unix()

	// Insert three entries at ascending timestamps.
	entries := []sqlc.InsertWalletUTXOLogParams{
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
		_, err := store.InsertWalletUTXOLog(ctx, e)
		require.NoError(t, err)
	}

	// List all entries — descending by created_at, so the spend
	// at now+2 should come first.
	all, err := store.ListWalletUTXOLog(
		ctx, sqlc.ListWalletUTXOLogParams{
			Limit:  100,
			Offset: 0,
		},
	)
	require.NoError(t, err)
	require.Len(t, all, 3)
	require.Equal(t, "spent", all[0].Event)
	require.Equal(t, "round_funding", all[0].ClassifiedAs)
	require.Equal(t, int64(50_000), all[0].AmountSat)
	require.Equal(t, "change", all[1].ClassifiedAs)
	require.Equal(t, "deposit", all[2].ClassifiedAs)

	// Count should match.
	count, err := store.CountWalletUTXOLog(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(3), count)
}

// TestListWalletUTXOLogByBlock verifies the block_height filter
// and its ordering contract (ORDER BY entry_id).
func TestListWalletUTXOLogByBlock(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	now := time.Now().Unix()

	// Block 100 gets 2 entries, block 200 gets 1.
	inserts := []sqlc.InsertWalletUTXOLogParams{
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
		_, err := store.InsertWalletUTXOLog(ctx, e)
		require.NoError(t, err)
	}

	// Block 100: two entries in entry_id order (insertion order).
	block100, err := store.ListWalletUTXOLogByBlock(ctx, 100)
	require.NoError(t, err)
	require.Len(t, block100, 2)
	require.Equal(t, int64(1000), block100[0].AmountSat)
	require.Equal(t, int64(3000), block100[1].AmountSat)

	// Block 200: one entry.
	block200, err := store.ListWalletUTXOLogByBlock(ctx, 200)
	require.NoError(t, err)
	require.Len(t, block200, 1)
	require.Equal(t, int64(2000), block200[0].AmountSat)

	// Block 999: no entries (empty, not an error).
	empty, err := store.ListWalletUTXOLogByBlock(ctx, 999)
	require.NoError(t, err)
	require.Empty(t, empty)
}

// TestListWalletUTXOLogByClassification verifies the
// classification filter and pagination.
func TestListWalletUTXOLogByClassification(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	now := time.Now().Unix()

	// Insert 5 entries: 3 deposits, 1 change, 1 unknown.
	for i, c := range []string{
		"deposit", "deposit", "change", "deposit", "unknown",
	} {
		_, err := store.InsertWalletUTXOLog(
			ctx, sqlc.InsertWalletUTXOLogParams{
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
	deposits, err := store.ListWalletUTXOLogByClassification(
		ctx, sqlc.ListWalletUTXOLogByClassificationParams{
			ClassifiedAs: "deposit",
			Limit:        10,
			Offset:       0,
		},
	)
	require.NoError(t, err)
	require.Len(t, deposits, 3)

	// Paginate: first 2, then last 1.
	page1, err := store.ListWalletUTXOLogByClassification(
		ctx, sqlc.ListWalletUTXOLogByClassificationParams{
			ClassifiedAs: "deposit",
			Limit:        2,
			Offset:       0,
		},
	)
	require.NoError(t, err)
	require.Len(t, page1, 2)

	page2, err := store.ListWalletUTXOLogByClassification(
		ctx, sqlc.ListWalletUTXOLogByClassificationParams{
			ClassifiedAs: "deposit",
			Limit:        2,
			Offset:       2,
		},
	)
	require.NoError(t, err)
	require.Len(t, page2, 1)

	// Pages should not overlap.
	require.NotEqual(t, page1[0].EntryID, page2[0].EntryID)
	require.NotEqual(t, page1[1].EntryID, page2[0].EntryID)

	// Single-classification filters still work.
	change, err := store.ListWalletUTXOLogByClassification(
		ctx, sqlc.ListWalletUTXOLogByClassificationParams{
			ClassifiedAs: "change",
			Limit:        10,
			Offset:       0,
		},
	)
	require.NoError(t, err)
	require.Len(t, change, 1)
}

// TestWalletUTXOSpendLifecycle documents and verifies the
// common "deposit → spend" UTXO lifecycle: a UTXO is created by
// one entry, then later marked spent by another entry that
// carries the same outpoint. Both rows remain in the log; the
// log is append-only.
func TestWalletUTXOSpendLifecycle(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	now := time.Now().Unix()
	outpoint := makeOutpoint(42)

	// Create.
	_, err := store.InsertWalletUTXOLog(
		ctx, sqlc.InsertWalletUTXOLogParams{
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
	_, err = store.InsertWalletUTXOLog(
		ctx, sqlc.InsertWalletUTXOLogParams{
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

	// Both rows present — append-only.
	count, err := store.CountWalletUTXOLog(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(2), count)

	// Block 1000 has the create.
	created, err := store.ListWalletUTXOLogByBlock(ctx, 1000)
	require.NoError(t, err)
	require.Len(t, created, 1)
	require.Equal(t, "created", created[0].Event)
	require.Equal(t, "deposit", created[0].ClassifiedAs)

	// Block 1050 has the spend.
	spent, err := store.ListWalletUTXOLogByBlock(ctx, 1050)
	require.NoError(t, err)
	require.Len(t, spent, 1)
	require.Equal(t, "spent", spent[0].Event)
	require.Equal(t, "round_funding", spent[0].ClassifiedAs)
}
