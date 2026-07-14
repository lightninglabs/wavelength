//go:build wavewalletrpc && swapruntime

package swapwallet

import (
	"context"
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/rpc/swapclientrpc"
	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
)

// newDualWriteFixture wires the history merger and the projector over the same
// fake sources and a real in-memory activity store, so a backfill can be
// compared against the legacy derive-on-read merge.
func newDualWriteFixture(t *testing.T) (*Runtime, *history, *fakeSwapService,
	*db.ActivityPersistenceStore) {

	t.Helper()

	testDB := db.NewTestDB(t)
	store := db.NewStore(
		testDB.DB, testDB.Queries, testDB.Backend(), btclog.Disabled,
	).NewActivityStore(clock.NewDefaultClock())

	swap := &fakeSwapService{}
	rpc := &fakeRPCServer{}
	deps := &Deps{
		SwapService:   swap,
		RPCServer:     rpc,
		ActivityStore: store,
	}
	runtime := newRuntime(t.Context(), deps)
	t.Cleanup(runtime.stop)

	return runtime, newHistory(deps, runtime), swap, store
}

// TestBackfillMirrorsLegacyMerge verifies the startup backfill projects the
// stable-id rows the legacy merge produces, with matching status — the
// foundation's dual-write contract. EXIT/DEPOSIT id divergences are out of
// scope here (handled by the later daemon stable-id hooks).
func TestBackfillMirrorsLegacyMerge(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	runtime, h, swap, store := newDualWriteFixture(t)

	// Two swap rows carry stable Lightning payment_hash ids today, so the
	// store must mirror the merge exactly for them.
	swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{
		Swaps: []*swapclientrpc.SwapSummary{
			{
				PaymentHash: "hash1",
				Direction: swapclientrpc.
					SwapDirection_SWAP_DIRECTION_PAY,
				State: swapclientrpc.
					SwapState_SWAP_STATE_COMPLETED,
				AmountSat:     1000,
				UpdatedAtUnix: 100,
			},
			{
				PaymentHash: "hash2",
				Direction: swapclientrpc.
					SwapDirection_SWAP_DIRECTION_RECEIVE,
				State: swapclientrpc.
					SwapState_SWAP_STATE_WAITING_FOR_CLAIM,
				AmountSat:     2000,
				UpdatedAtUnix: 200,
			},
		},
	}

	// The derive-on-read merge is the comparison oracle — it is what the
	// backfill seeds the store from (listActivity now reads the store).
	merged, err := h.deriveActivity(
		ctx, &wavewalletrpc.ListRequest{
			Limit: 100,
		},
	)
	require.NoError(t, err)
	wantStatus := make(map[string]int64)
	for _, e := range merged.GetEntries() {
		wantStatus[e.GetId()] = int64(e.GetStatus())
	}
	require.Len(t, wantStatus, 2)

	// Seed the store from the same sources.
	runtime.backfillActivity(ctx)

	stored, err := store.ListEntries(ctx, 0, "", 100)
	require.NoError(t, err)

	gotStatus := make(map[string]int64)
	for _, row := range stored {
		gotStatus[row.CanonicalID] = row.Status
	}
	require.Equal(
		t, wantStatus, gotStatus,
		"store must mirror the legacy merge for stable-id rows",
	)

	// Every projected row also produced a transition event.
	events, err := store.PullEvents(ctx, 0, 100)
	require.NoError(t, err)
	require.Len(t, events, len(wantStatus))
}

// TestBackfillIsIdempotent verifies re-running the backfill does not change the
// current-state rows (upsert keyed on canonical_id).
func TestBackfillIsIdempotent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	runtime, _, swap, store := newDualWriteFixture(t)

	swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{
		Swaps: []*swapclientrpc.SwapSummary{
			{
				PaymentHash: "hash1",
				Direction: swapclientrpc.
					SwapDirection_SWAP_DIRECTION_PAY,
				State: swapclientrpc.
					SwapState_SWAP_STATE_COMPLETED,
				AmountSat:     1000,
				UpdatedAtUnix: 100,
			},
		},
	}

	runtime.backfillActivity(ctx)
	runtime.backfillActivity(ctx)

	stored, err := store.ListEntries(ctx, 0, "", 100)
	require.NoError(t, err)
	require.Len(t, stored, 1, "re-running backfill must not duplicate rows")
}
