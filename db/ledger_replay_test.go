package db

import (
	"testing"
	"time"

	"github.com/lightninglabs/darepo/db/sqlc"
	"github.com/stretchr/testify/require"
)

// TestLedgerReplayIdempotentWithKey verifies that inserting the
// exact same (idempotency_key, event_type, debit_account,
// credit_account) quadruple twice silently no-ops the second
// insert via the partial unique index on the ledger_entries
// table. This is the load-bearing property that keeps durable
// mailbox replay safe: at-least-once redelivery of a Record*
// message must never produce a duplicate ledger row.
func TestLedgerReplayIdempotentWithKey(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	now := time.Unix(1_700_000_000, 0)
	key := []byte("round-idem-key-001")

	params := sqlc.InsertLedgerEntryParams{
		DebitAccount:   "deployed_capital",
		CreditAccount:  "boarding_fee_revenue",
		AmountSat:      400,
		RoundID:        []byte("round-replay-001"),
		IdempotencyKey: key,
		EventType:      "boarding_fee",
		Description:    "first insert",
		CreatedAt:      now.Unix(),
	}

	// First insert: must commit.
	rows, err := store.InsertLedgerEntry(ctx, params)
	require.NoError(t, err, "first insert")
	require.Equal(t, int64(1), rows,
		"first insert must return rowcount=1")

	// Second insert with the same key: must silently dedupe.
	// Change the description to prove we're testing dedup (not
	// an actual duplicate of every field): the partial unique
	// index keys on (idempotency_key, event_type,
	// debit_account, credit_account), so the description /
	// amount are free to differ.
	params.Description = "replayed insert"
	rows, err = store.InsertLedgerEntry(ctx, params)
	require.NoError(t, err, "second insert must not error")
	require.Equal(t, int64(0), rows,
		"second insert must return rowcount=0 (deduped)")

	// Verify only ONE row exists.
	count, err := store.CountLedgerEntries(ctx)
	require.NoError(t, err)
	require.Equal(
		t, int64(1), count,
		"exactly one row must exist after replay",
	)

	// The surviving row must be the first insert's
	// description, not the second insert's.
	entries, err := store.ListLedgerEntries(
		ctx, sqlc.ListLedgerEntriesParams{
			Limit: 10, Offset: 0,
		},
	)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "first insert", entries[0].Description,
		"first insert must be the surviving row")
}

// TestLedgerReplayAllowsDifferentEventTypesSameRoundID verifies
// that the partial unique index discriminates on event_type: a
// single round can book two distinct legs (e.g. refresh forfeit
// and refresh fee) with the SAME idempotency_key because their
// event_types differ. Without this, the two-leg refresh path
// would incorrectly dedup the fee leg behind the forfeit leg.
func TestLedgerReplayAllowsDifferentEventTypesSameRoundID(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	now := time.Unix(1_700_000_000, 0)
	key := []byte("round-idem-key-002")
	roundID := []byte("round-refresh-dualleg")

	// Leg 1: refresh_forfeit.
	forfeit := sqlc.InsertLedgerEntryParams{
		DebitAccount:   "user_vtxo_claims",
		CreditAccount:  "deployed_capital",
		AmountSat:      50_000,
		RoundID:        roundID,
		IdempotencyKey: key,
		EventType:      "refresh_forfeit",
		Description:    "retire old claim",
		CreatedAt:      now.Unix(),
	}
	rows, err := store.InsertLedgerEntry(ctx, forfeit)
	require.NoError(t, err)
	require.Equal(t, int64(1), rows)

	// Leg 2: refresh_fee with the SAME idempotency_key but a
	// DIFFERENT event_type. Must still commit.
	fee := sqlc.InsertLedgerEntryParams{
		DebitAccount:   "user_vtxo_claims",
		CreditAccount:  "refresh_fee_revenue",
		AmountSat:      500,
		RoundID:        roundID,
		IdempotencyKey: key,
		EventType:      "refresh_fee",
		Description:    "operator fee on refresh",
		CreatedAt:      now.Unix() + 1,
	}
	rows, err = store.InsertLedgerEntry(ctx, fee)
	require.NoError(
		t, err, "different event_type must not be deduped",
	)
	require.Equal(t, int64(1), rows)

	count, err := store.CountLedgerEntries(ctx)
	require.NoError(t, err)
	require.Equal(
		t, int64(2), count,
		"forfeit + fee must coexist under the same key",
	)
}

// TestLedgerReplayIdempotentWithoutKeyIsNotDeduped verifies the
// partial-index contract: entries WITHOUT an idempotency_key
// (NULL) are outside the partial unique index and ALWAYS
// insert, even when every other column matches. This matches
// the schema's
// `UNIQUE(idempotency_key, event_type, debit_account, credit_account)
//  WHERE idempotency_key IS NOT NULL`
// constraint.
func TestLedgerReplayIdempotentWithoutKeyIsNotDeduped(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	now := time.Unix(1_700_000_000, 0)
	params := sqlc.InsertLedgerEntryParams{
		DebitAccount:  "mining_fees",
		CreditAccount: "treasury_wallet",
		AmountSat:     2500,
		RoundID:       []byte("round-nokey"),
		EventType:     "mining_fee",
		Description:   "round tx mining fee",
		CreatedAt:     now.Unix(),
		// IdempotencyKey deliberately nil.
	}

	rows, err := store.InsertLedgerEntry(ctx, params)
	require.NoError(t, err)
	require.Equal(t, int64(1), rows)

	// Second insert, same shape, no key: must also commit.
	rows, err = store.InsertLedgerEntry(ctx, params)
	require.NoError(t, err)
	require.Equal(
		t, int64(1), rows,
		"nil-key inserts must always commit (no dedup)",
	)

	count, err := store.CountLedgerEntries(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(2), count)
}
