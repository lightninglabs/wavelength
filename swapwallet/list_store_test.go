//go:build walletdkrpc && swapruntime

package swapwallet

import (
	"context"
	"fmt"
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/rpc/walletdkrpc"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// newStoreListFixture wires a history reader over a real in-memory activity
// store so the store-backed List read path can be exercised end to end.
func newStoreListFixture(t *testing.T) (*history,
	*db.ActivityPersistenceStore) {

	t.Helper()

	testDB := db.NewTestDB(t)
	store := db.NewStore(
		testDB.DB, testDB.Queries, testDB.Backend(), btclog.Disabled,
	).NewActivityStore(clock.NewDefaultClock())

	deps := &Deps{ActivityStore: store}
	runtime := newRuntime(t.Context(), deps)
	t.Cleanup(runtime.stop)

	return newHistory(deps, runtime), store
}

// seedActivity projects one entry into the store via the production mapping.
func seedActivity(t *testing.T, store *db.ActivityPersistenceStore, id string,
	kind walletdkrpc.EntryKind, status walletdkrpc.EntryStatus,
	created int64) {

	t.Helper()

	proj, err := entryToProjection(&walletdkrpc.WalletEntry{
		Id:            id,
		Kind:          kind,
		Status:        status,
		AmountSat:     1000,
		CreatedAtUnix: created,
		UpdatedAtUnix: created,
	})
	require.NoError(t, err)

	_, err = store.ProjectEntry(context.Background(), proj)
	require.NoError(t, err)
}

func activityIDs(list *walletdkrpc.ActivityList) []string {
	out := make([]string, 0, len(list.GetEntries()))
	for _, e := range list.GetEntries() {
		out = append(out, e.GetId())
	}

	return out
}

// TestListActivityReadsStore verifies List pages the store newest-first and
// resumes via next_cursor with a correct has_more.
func TestListActivityReadsStore(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h, store := newStoreListFixture(t)

	seedActivity(
		t, store, "a", walletdkrpc.EntryKind_ENTRY_KIND_SEND,
		walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE, 100,
	)
	seedActivity(
		t, store, "b", walletdkrpc.EntryKind_ENTRY_KIND_RECV,
		walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE, 200,
	)
	seedActivity(
		t, store, "c", walletdkrpc.EntryKind_ENTRY_KIND_DEPOSIT,
		walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE, 300,
	)

	page1, err := h.listActivity(ctx, &walletdkrpc.ListRequest{Limit: 2})
	require.NoError(t, err)
	require.Equal(t, []string{"c", "b"}, activityIDs(page1))
	require.True(t, page1.GetHasMore())
	require.NotEmpty(t, page1.GetNextCursor())

	page2, err := h.listActivity(ctx, &walletdkrpc.ListRequest{
		Limit:  2,
		Cursor: page1.GetNextCursor(),
	})
	require.NoError(t, err)
	require.Equal(t, []string{"a"}, activityIDs(page2))
	require.False(t, page2.GetHasMore())
	require.Empty(t, page2.GetNextCursor())
}

// TestCountPendingReflectsFullFeed verifies countPending returns the full
// number of pending rows rather than the single-page total the paginated read
// path reports. This is the store-backed count behind the wallet status
// summary's pending count.
func TestCountPendingReflectsFullFeed(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h, store := newStoreListFixture(t)

	// Three pending rows plus one terminal row.
	seedActivity(
		t, store, "p1", walletdkrpc.EntryKind_ENTRY_KIND_SEND,
		walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING, 100,
	)
	seedActivity(
		t, store, "p2", walletdkrpc.EntryKind_ENTRY_KIND_SEND,
		walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING, 200,
	)
	seedActivity(
		t, store, "p3", walletdkrpc.EntryKind_ENTRY_KIND_RECV,
		walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING, 300,
	)
	seedActivity(
		t, store, "done", walletdkrpc.EntryKind_ENTRY_KIND_SEND,
		walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE, 400,
	)

	// A single-page pending read caps its total at the page size, so it
	// cannot stand in for the pending count.
	page, err := h.listActivity(ctx, &walletdkrpc.ListRequest{
		Limit:       1,
		PendingOnly: true,
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, page.GetTotal())
	require.True(t, page.GetHasMore())

	// countPending reports every pending row regardless of page size.
	count, err := h.countPending(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 3, count)
}

// TestListActivityStablePaginationUnderInsert verifies the #781 acceptance
// criterion: a row inserted between page fetches never causes an existing row
// to be skipped or duplicated, because the cursor is an immutable keyset.
func TestListActivityStablePaginationUnderInsert(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h, store := newStoreListFixture(t)

	seedActivity(
		t, store, "a", walletdkrpc.EntryKind_ENTRY_KIND_SEND,
		walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE, 100,
	)
	seedActivity(
		t, store, "b", walletdkrpc.EntryKind_ENTRY_KIND_SEND,
		walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE, 200,
	)
	seedActivity(
		t, store, "c", walletdkrpc.EntryKind_ENTRY_KIND_SEND,
		walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE, 300,
	)

	page1, err := h.listActivity(ctx, &walletdkrpc.ListRequest{Limit: 2})
	require.NoError(t, err)
	require.Equal(t, []string{"c", "b"}, activityIDs(page1))

	// A new op lands between page fetches, newer than the page-1 cursor.
	seedActivity(
		t, store, "d", walletdkrpc.EntryKind_ENTRY_KIND_SEND,
		walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE, 250,
	)

	page2, err := h.listActivity(ctx, &walletdkrpc.ListRequest{
		Limit:  2,
		Cursor: page1.GetNextCursor(),
	})
	require.NoError(t, err)

	// Page 2 continues strictly older than the cursor: "a" is returned
	// once, "b"/"c" are not duplicated, and the newer "d" is simply above
	// this pagination pass (a fresh read would surface it at the top).
	require.Equal(t, []string{"a"}, activityIDs(page2))
}

// TestListActivityFilters verifies pending_only and kind filters apply over the
// store keyset scan.
func TestListActivityFilters(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h, store := newStoreListFixture(t)

	seedActivity(
		t, store, "send", walletdkrpc.EntryKind_ENTRY_KIND_SEND,
		walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING, 100,
	)
	seedActivity(
		t, store, "recv", walletdkrpc.EntryKind_ENTRY_KIND_RECV,
		walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE, 200,
	)
	seedActivity(
		t, store, "exit", walletdkrpc.EntryKind_ENTRY_KIND_EXIT,
		walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING, 300,
	)

	pending, err := h.listActivity(ctx, &walletdkrpc.ListRequest{
		Limit:       10,
		PendingOnly: true,
	})
	require.NoError(t, err)
	require.Equal(t, []string{"exit", "send"}, activityIDs(pending))

	recvOnly, err := h.listActivity(ctx, &walletdkrpc.ListRequest{
		Limit: 10,
		Kinds: []walletdkrpc.EntryKind{
			walletdkrpc.EntryKind_ENTRY_KIND_RECV,
		},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"recv"}, activityIDs(recvOnly))
}

// TestListActivityRejectsBadCursor verifies a malformed cursor is a clean
// error.
func TestListActivityRejectsBadCursor(t *testing.T) {
	t.Parallel()

	h, _ := newStoreListFixture(t)

	_, err := h.listActivity(context.Background(), &walletdkrpc.ListRequest{
		Cursor: "!!!not-base64!!!",
	})
	require.ErrorIs(t, err, errInvalidActivityCursor)
}

// TestListActivityRejectsNonPositiveCursor verifies a cursor whose timestamp is
// zero or negative is rejected rather than silently colliding with the
// return-all sentinel and restarting paging from the newest row.
func TestListActivityRejectsNonPositiveCursor(t *testing.T) {
	t.Parallel()

	h, _ := newStoreListFixture(t)

	for _, created := range []int64{0, -1} {
		cursor := encodeActivityCursor(created, "x")
		_, err := h.listActivity(
			context.Background(), &walletdkrpc.ListRequest{
				Cursor: cursor,
			},
		)
		require.ErrorIs(t, err, errInvalidActivityCursor)
	}
}

// TestListActivityBoundsFilteredScan verifies a selective filter over a large
// non-matching table does not scan the whole table in one request: the call
// stops at the scan budget and returns an empty page plus a cursor to resume,
// instead of decoding every row (the H-2 amplification cliff).
func TestListActivityBoundsFilteredScan(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h, store := newStoreListFixture(t)

	// Seed far more terminal rows than one page's scan budget
	// (limit * activityScanBudgetFactor), none of which match --pending.
	const rows = 60
	for i := 0; i < rows; i++ {
		seedActivity(
			t, store, fmt.Sprintf("c%02d", i),
			walletdkrpc.EntryKind_ENTRY_KIND_SEND,
			walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE,
			int64(100+i),
		)
	}

	page, err := h.listActivity(ctx, &walletdkrpc.ListRequest{
		Limit:       2,
		PendingOnly: true,
	})
	require.NoError(t, err)

	// The budget-bounded scan returns no matches but signals more work with
	// a resume cursor, rather than draining the table (which would report
	// has_more=false).
	require.Empty(t, page.GetEntries())
	require.True(t, page.GetHasMore())
	require.NotEmpty(t, page.GetNextCursor())
}

// TestRowToWalletEntryRoundTrip verifies a WalletEntry survives the
// project → store → row → rowToWalletEntry round trip unchanged.
func TestRowToWalletEntryRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	_, store := newStoreListFixture(t)

	entry := sampleWalletEntry()

	proj, err := entryToProjection(entry)
	require.NoError(t, err)

	_, err = store.ProjectEntry(ctx, proj)
	require.NoError(t, err)

	row, err := store.GetEntry(ctx, entry.GetId())
	require.NoError(t, err)

	got, err := rowToWalletEntry(row)
	require.NoError(t, err)
	require.True(
		t, proto.Equal(entry, got),
		"reconstructed entry must equal the original",
	)
}
