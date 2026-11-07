package db

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCurrentActivityBatchPagesFrozenRange proves a finite replay cannot lose
// entries beyond the latest UI page, chase new traffic forever, or return an
// internal projection that reconciliation removed.
func TestCurrentActivityBatchPagesFrozenRange(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := newActivityStoreForTest(t)
	baseline, err := store.EventHighWater(ctx)
	require.NoError(t, err)
	require.Zero(t, baseline)
	for i := 0; i < 300; i++ {
		require.NoError(
			t,
			projected(
				store.ProjectEntry(
					ctx,
					sampleProjection(
						fmt.Sprintf("entry-%03d", i),
					),
				),
			),
		)
	}
	first, err := store.ReadCurrentBatch(ctx, 0, 0, 37)
	require.NoError(t, err)
	require.True(t, first.More)
	require.EqualValues(t, 300, first.HighWater)
	require.Len(t, first.Entries, 37)

	// New events cannot extend this replay's ceiling. Projection values
	// remain current, including when a later transition changes a row.
	require.NoError(
		t,
		projected(
			store.ProjectEntry(
				ctx, sampleProjection("after-ceiling"),
			),
		),
	)
	updated := sampleProjection("entry-000")
	updated.Status = 2
	require.NoError(t, projected(store.ProjectEntry(ctx, updated)))
	require.NoError(t, store.RemoveEntry(ctx, "entry-001"))
	require.NoError(t, store.RemoveEntry(ctx, "entry-299"))

	seen := make(map[string]int64)
	var cursor int64
	for {
		batch, err := store.ReadCurrentBatch(
			ctx, cursor, first.HighWater, 37,
		)
		require.NoError(t, err)
		require.Equal(t, first.HighWater, batch.HighWater)
		require.Greater(t, batch.NextCursor, cursor)
		require.LessOrEqual(t, batch.NextCursor, first.HighWater)
		for _, entry := range batch.Entries {
			require.Greater(t, entry.Cursor, cursor)
			require.LessOrEqual(t, entry.Cursor, first.HighWater)
			seen[entry.Entry.CanonicalID] = entry.Entry.Status
		}
		cursor = batch.NextCursor
		if !batch.More {
			break
		}
	}
	require.Len(t, seen, 298)
	require.NotContains(t, seen, "after-ceiling")
	require.NotContains(t, seen, "entry-001")
	require.NotContains(t, seen, "entry-299")
	require.EqualValues(t, 2, seen["entry-000"])
	require.Equal(
		t, first.HighWater, cursor,
		"deleted tail must not trap the cursor",
	)

	later, err := store.ReadCurrentBatch(ctx, cursor, 0, 37)
	require.NoError(t, err)
	require.False(t, later.More)
	require.Len(t, later.Entries, 2)
	require.Equal(t, "after-ceiling", later.Entries[0].Entry.CanonicalID)
	require.Equal(t, "entry-000", later.Entries[1].Entry.CanonicalID)

	// Removing the newest retained events can lower MAX(event_seq). The
	// consumer's position still never moves backwards or skips a new event.
	require.NoError(t, store.RemoveEntry(ctx, "after-ceiling"))
	require.NoError(t, store.RemoveEntry(ctx, "entry-000"))
	empty, err := store.ReadCurrentBatch(ctx, later.NextCursor, 0, 37)
	require.NoError(t, err)
	require.Empty(t, empty.Entries)
	require.Equal(t, later.NextCursor, empty.NextCursor)
	require.NoError(
		t,
		projected(
			store.ProjectEntry(
				ctx, sampleProjection("new"),
			),
		),
	)
	next, err := store.ReadCurrentBatch(ctx, empty.NextCursor, 0, 37)
	require.NoError(t, err)
	require.Len(t, next.Entries, 1)
	require.Equal(t, "new", next.Entries[0].Entry.CanonicalID)
}

// TestCurrentActivityBatchBounds rejects unbounded or contradictory reads at
// the owning store boundary before allocating a query result.
func TestCurrentActivityBatchBounds(t *testing.T) {
	t.Parallel()
	store := newActivityStoreForTest(t)
	for _, bounds := range []struct {
		after, through int64
		limit          int32
	}{
		{
			-1,
			0,
			1,
		}, {
			0,
			-1,
			1,
		}, {
			2,
			1,
			1,
		}, {
			0,
			0,
			0,
		},
		{
			0,
			0,
			MaxActivityBatchSize + 1,
		},
	} {
		_, err := store.ReadCurrentBatch(
			t.Context(), bounds.after, bounds.through, bounds.limit,
		)
		require.Error(t, err)
	}
}
