package db

import (
	"context"
	"database/sql"
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// newActivityStoreForTest builds an ActivityPersistenceStore backed by a fresh
// in-memory test database.
func newActivityStoreForTest(t *testing.T) *ActivityPersistenceStore {
	t.Helper()

	testDB := NewTestDB(t)

	activityDB := NewTransactionExecutor(
		testDB.BaseDB,
		func(tx *sql.Tx) ActivityStore {
			return testDB.WithTx(tx)
		},
		btclog.Disabled,
	)

	return NewActivityPersistenceStore(activityDB, clock.NewDefaultClock())
}

// projected discards the event_seq returned by ProjectEntry so a call slots
// straight into require.NoError / require.Error.
func projected(_ int64, err error) error {
	return err
}

// sampleProjection returns a populated projection for the given canonical id.
func sampleProjection(id string) ActivityProjection {
	return ActivityProjection{
		CanonicalID:   id,
		Kind:          1, // send
		Status:        1, // pending
		AmountSat:     -1000,
		FeeSat:        10,
		Counterparty:  "cp",
		Note:          "note",
		Phase:         2,
		PhaseLabel:    "funding",
		PendingStatus: 1,
		PaymentHash: []byte{
			0xaa,
			0xbb,
			0xcc,
		},
		RequestJSON:   `{"lightningInvoice":{"invoice":"lnbc1"}}`,
		EntryJSON:     `{"id":"` + id + `"}`,
		CreatedAtUnix: 100,
		UpdatedAtUnix: 100,
	}
}

// TestActivityStoreProjectInsertsEntryAndEvent verifies a first projection
// writes the current-state row and exactly one transition event.
func TestActivityStoreProjectInsertsEntryAndEvent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := newActivityStoreForTest(t)

	require.NoError(
		t,
		projected(
			store.ProjectEntry(
				ctx, sampleProjection("a"),
			),
		),
	)

	entry, err := store.GetEntry(ctx, "a")
	require.NoError(t, err)
	require.EqualValues(t, 1, entry.Status)
	require.EqualValues(t, 2, entry.Phase)
	require.Equal(t, int64(-1000), entry.AmountSat)
	require.Equal(t, []byte{0xaa, 0xbb, 0xcc}, entry.PaymentHash)
	require.Equal(t, int64(100), entry.CreatedAtUnix)

	events, err := store.PullEvents(ctx, 0, 10)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, "a", events[0].CanonicalID)
	require.EqualValues(t, 1, events[0].Status)
}

// TestActivityStoreRejectsTerminalRegression verifies a stale pending write
// cannot overwrite a terminal row or append a backward lifecycle event.
func TestActivityStoreRejectsTerminalRegression(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newActivityStoreForTest(t)

	pending := sampleProjection("ordered")
	_, err := store.ProjectEntry(ctx, pending)
	require.NoError(t, err)

	complete := pending
	complete.Status = 2
	complete.Phase = 3
	complete.EntryJSON = `{"id":"ordered","status":"complete"}`
	complete.UpdatedAtUnix = 200
	_, err = store.ProjectEntry(ctx, complete)
	require.NoError(t, err)

	staleSeq, err := store.ProjectEntry(ctx, pending)
	require.NoError(t, err)
	require.Zero(t, staleSeq)

	row, err := store.GetEntry(ctx, "ordered")
	require.NoError(t, err)
	require.EqualValues(t, 2, row.Status)
	require.EqualValues(t, 3, row.Phase)

	events, err := store.PullEvents(ctx, 0, 10)
	require.NoError(t, err)
	require.Len(t, events, 2)
}

// TestActivityStoreRequestOnlyEnrichment verifies immutable request context is
// allowed to enrich an otherwise unchanged sparse row exactly once.
func TestActivityStoreRequestOnlyEnrichment(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newActivityStoreForTest(t)

	sparse := sampleProjection("request-context")
	sparse.RequestJSON = ""
	_, err := store.ProjectEntry(ctx, sparse)
	require.NoError(t, err)

	rich := sparse
	rich.RequestJSON = `{"lightningInvoice":{"invoice":"lnbc1"}}`
	rich.EntryJSON = `{"id":"request-context","request":` +
		`{"lightningInvoice":{"invoice":"lnbc1"}}}`
	richSeq, err := store.ProjectEntry(ctx, rich)
	require.NoError(t, err)
	require.Positive(t, richSeq)

	row, err := store.GetEntry(ctx, "request-context")
	require.NoError(t, err)
	require.JSONEq(t, rich.RequestJSON, row.RequestJson)

	semanticallyEqual := rich
	semanticallyEqual.RequestJSON = `{ "lightningInvoice": {` +
		`"invoice": "lnbc1" } }`
	equalSeq, err := store.ProjectEntry(ctx, semanticallyEqual)
	require.NoError(t, err)
	require.Zero(t, equalSeq)

	events, err := store.PullEvents(ctx, 0, 10)
	require.NoError(t, err)
	require.Len(t, events, 2)
}

// TestActivityStoreCountByStatus verifies CountByStatus returns a full,
// unpaginated count of the rows in a given status — the primitive the wallet
// status summary's pending count relies on.
func TestActivityStoreCountByStatus(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := newActivityStoreForTest(t)

	// Two pending rows (status 1) and one complete row (status 2).
	complete := sampleProjection("c1")
	complete.Status = 2
	require.NoError(
		t,
		projected(
			store.ProjectEntry(
				ctx, sampleProjection("p1"),
			),
		),
	)
	require.NoError(
		t,
		projected(
			store.ProjectEntry(
				ctx, sampleProjection("p2"),
			),
		),
	)
	require.NoError(t, projected(store.ProjectEntry(ctx, complete)))

	pending, err := store.CountByStatus(ctx, 1)
	require.NoError(t, err)
	require.EqualValues(t, 2, pending)

	completed, err := store.CountByStatus(ctx, 2)
	require.NoError(t, err)
	require.EqualValues(t, 1, completed)

	failed, err := store.CountByStatus(ctx, 3)
	require.NoError(t, err)
	require.EqualValues(t, 0, failed)
}

// TestActivityStoreProjectSuppressesUnchanged verifies that re-projecting an
// identical state appends no new event, so the backfill and the swap monitor's
// replay do not accumulate duplicate transitions in the append-only log.
func TestActivityStoreProjectSuppressesUnchanged(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := newActivityStoreForTest(t)

	require.NoError(
		t,
		projected(
			store.ProjectEntry(
				ctx, sampleProjection("a"),
			),
		),
	)
	require.NoError(
		t,
		projected(
			store.ProjectEntry(
				ctx, sampleProjection("a"),
			),
		),
	)

	events, err := store.PullEvents(ctx, 0, 10)
	require.NoError(t, err)
	require.Len(
		t, events, 1, "unchanged re-projection must append no event",
	)
}

// TestActivityStoreReProjectUpdatesInPlace verifies a second projection of the
// same canonical id advances status/phase/updated_at, preserves created_at and
// the previously-recorded payment hash, and appends a higher-seq event.
func TestActivityStoreReProjectUpdatesInPlace(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := newActivityStoreForTest(t)

	require.NoError(
		t,
		projected(
			store.ProjectEntry(
				ctx, sampleProjection("a"),
			),
		),
	)

	// Advance the row: complete, new phase, later update time, and a nil
	// payment hash + zero created time that must NOT clobber the originals.
	next := sampleProjection("a")
	next.Status = 2 // complete
	next.Phase = 5
	next.UpdatedAtUnix = 200
	next.CreatedAtUnix = 0
	next.PaymentHash = nil
	next.Txid = []byte{0x11, 0x22}
	next.Note = ""
	next.RequestJSON = ""
	require.NoError(t, projected(store.ProjectEntry(ctx, next)))

	entry, err := store.GetEntry(ctx, "a")
	require.NoError(t, err)
	require.EqualValues(t, 2, entry.Status)
	require.EqualValues(t, 5, entry.Phase)
	require.Equal(t, int64(200), entry.UpdatedAtUnix)
	require.Equal(
		t, int64(100), entry.CreatedAtUnix, "created_at preserved",
	)
	require.Equal(
		t, []byte{0xaa, 0xbb, 0xcc}, entry.PaymentHash,
		"nil payment hash must not clobber the stored value",
	)
	require.Equal(t, []byte{0x11, 0x22}, entry.Txid)
	require.Equal(t, "note", entry.Note)
	require.Equal(
		t, `{"lightningInvoice":{"invoice":"lnbc1"}}`,
		entry.RequestJson,
	)

	events, err := store.PullEvents(ctx, 0, 10)
	require.NoError(t, err)
	require.Len(t, events, 2)
	require.Greater(t, events[1].EventSeq, events[0].EventSeq)
	require.EqualValues(t, 2, events[1].Status)
}

// TestActivityStoreEnumForeignKey verifies the kind/status foreign keys reject
// any value that is not a defined wire enum.
func TestActivityStoreEnumForeignKey(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := newActivityStoreForTest(t)

	badKind := sampleProjection("a")
	badKind.Kind = 99
	require.Error(t, projected(store.ProjectEntry(ctx, badKind)))

	badStatus := sampleProjection("b")
	badStatus.Status = 99
	require.Error(t, projected(store.ProjectEntry(ctx, badStatus)))
}

// TestActivityStoreNilBlobStaysNull verifies an empty hash handle is stored as
// NULL, not a zero-length blob.
func TestActivityStoreNilBlobStaysNull(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := newActivityStoreForTest(t)

	p := sampleProjection("a")
	p.PaymentHash = nil
	p.Txid = nil
	require.NoError(t, projected(store.ProjectEntry(ctx, p)))

	entry, err := store.GetEntry(ctx, "a")
	require.NoError(t, err)
	require.Nil(t, entry.PaymentHash)
	require.Nil(t, entry.Txid)
}

// TestActivityStoreListKeyset verifies entries page newest-first by the
// immutable created cursor, honor the limit, and resume after the cursor.
func TestActivityStoreListKeyset(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := newActivityStoreForTest(t)

	for id, created := range map[string]int64{
		"old": 100, "mid": 200, "new": 300,
	} {
		p := sampleProjection(id)
		p.CreatedAtUnix = created
		p.UpdatedAtUnix = created
		require.NoError(t, projected(store.ProjectEntry(ctx, p)))
	}

	page, err := store.ListEntries(ctx, 0, "", 2)
	require.NoError(t, err)
	require.Len(t, page, 2)
	require.Equal(t, "new", page[0].CanonicalID)
	require.Equal(t, "mid", page[1].CanonicalID)

	last := page[len(page)-1]
	next, err := store.ListEntries(
		ctx, last.CreatedAtUnix, last.CanonicalID, 2,
	)
	require.NoError(t, err)
	require.Len(t, next, 1)
	require.Equal(t, "old", next[0].CanonicalID)
}

// TestActivityStorePullEventsAfterCursor verifies the event log replays only
// rows strictly after the cursor, in ascending event_seq order.
func TestActivityStorePullEventsAfterCursor(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := newActivityStoreForTest(t)

	require.NoError(
		t,
		projected(
			store.ProjectEntry(
				ctx, sampleProjection("a"),
			),
		),
	)
	require.NoError(
		t,
		projected(
			store.ProjectEntry(
				ctx, sampleProjection("b"),
			),
		),
	)

	all, err := store.PullEvents(ctx, 0, 10)
	require.NoError(t, err)
	require.Len(t, all, 2)

	after, err := store.PullEvents(ctx, all[0].EventSeq, 10)
	require.NoError(t, err)
	require.Len(t, after, 1)
	require.Equal(t, all[1].EventSeq, after[0].EventSeq)
}
