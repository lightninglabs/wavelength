//go:build wavewalletrpc && swapruntime

package wavewalletdk

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// TestActivityBatchConvertsCurrentRows proves the facade reads the current
// canonical status and amount, rather than an older serialized transition.
// Both the store query and the wallet's real projection decoder participate.
func TestActivityBatchConvertsCurrentRows(t *testing.T) {
	ctx := t.Context()
	database := db.NewTestDB(t)
	store := db.NewStore(database.DB, database.Queries, database.Backend(),
		btclog.Disabled).NewActivityStore(clock.NewDefaultClock())
	client := &Client{
		readActivityBatch: store.ReadCurrentBatch,
		activityCursor:    store.EventHighWater,
	}
	defer func() {
		require.NoError(t, client.CloseContext(context.Background()))
	}()
	baseline, err := client.ActivityCursor(ctx)
	require.NoError(t, err)
	require.Zero(t, baseline)
	projection := db.ActivityProjection{
		CanonicalID: "receive", Kind: int64(
			wavewalletrpc.EntryKind_ENTRY_KIND_RECV,
		),
		Status: int64(
			wavewalletrpc.EntryStatus_ENTRY_STATUS_PENDING,
		),
		PendingStatus: int64(
			wavewalletrpc.EntryStatus_ENTRY_STATUS_PENDING,
		),
		AmountSat: 100_000, CreatedAtUnix: 100, UpdatedAtUnix: 100,
		EntryJSON: `{"id":"receive","status":"ENTRY_STATUS_PENDING"}`,
	}
	first, err := store.ProjectEntry(ctx, projection)
	require.NoError(t, err)
	projection.Status = int64(
		wavewalletrpc.EntryStatus_ENTRY_STATUS_COMPLETE,
	)
	projection.EntryJSON = `{"id":"receive",` +
		`"status":"ENTRY_STATUS_COMPLETE"}`
	last, err := store.ProjectEntry(ctx, projection)
	require.NoError(t, err)
	require.Greater(t, last, first)
	// Freeze at the older transition. The returned row still describes the
	// canonical state now, not that transition's obsolete pending snapshot.
	page, err := client.ReadActivityBatch(ctx, ActivityBatchRequest{
		AfterCursor: baseline, ThroughCursor: first, Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, page.Entries, 1)
	require.Equal(t, EntryKindReceive, page.Entries[0].Kind)
	require.Equal(t, EntryStatusComplete, page.Entries[0].Status)
	require.EqualValues(t, 100_000, page.Entries[0].AmountSat)
	require.Equal(t, first, page.Entries[0].Cursor)
	require.Equal(t, first, page.NextCursor)
	require.False(t, page.HasMore)
	// The mobile facade uses encoding/json, including numeric zero cursors
	// and an empty JSON array. Do not substitute protojson fixtures.
	empty, err := client.ReadActivityBatch(ctx, ActivityBatchRequest{
		AfterCursor: last, ThroughCursor: last, Limit: 10,
	})
	require.NoError(t, err)
	encoded, err := json.Marshal(empty) //nolint:musttag // Entry ABI.
	require.NoError(t, err)
	require.JSONEq(
		t,
		`{"high_water":2,"next_cursor":2,`+
			`"has_more":false,"entries":[]}`,
		string(encoded),
	)
}

// TestActivityBatchRejectsRemoteHandle keeps the direct store API local. It
// cannot turn an existing remote transport into an implicit private-store read.
func TestActivityBatchRejectsRemoteHandle(t *testing.T) {
	client := &Client{}
	_, err := client.ReadActivityBatch(
		t.Context(), ActivityBatchRequest{
			Limit: 1,
		},
	)
	require.ErrorIs(t, err, ErrActivityReplayUnavailable)
	_, err = client.ActivityCursor(t.Context())
	require.ErrorIs(t, err, ErrActivityReplayUnavailable)
}
