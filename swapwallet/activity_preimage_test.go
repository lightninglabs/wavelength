//go:build wavewalletrpc && swapruntime

package swapwallet

import (
	"context"
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/credit"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/rpc/swapclientrpc"
	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// activityPreimageHex is a 32-byte preimage for the persisted-list tests.
const activityPreimageHex = "0102030405060708090a0b0c0d0e0f10" +
	"1112131415161718191a1b1c1d1e1f20"

// newPreimageFixture wires a runtime over a real in-memory activity store with
// the credit registry and swap service fakes the send paths need.
func newPreimageFixture(t *testing.T) (*Runtime, *history, *InspectionService,
	*fakeCreditRegistry, *fakeSwapService) {

	t.Helper()

	testDB := db.NewTestDB(t)
	store := db.NewStore(
		testDB.DB, testDB.Queries, testDB.Backend(), btclog.Disabled,
	).NewActivityStore(clock.NewDefaultClock())

	reg := &fakeCreditRegistry{
		listResp: &credit.ListCreditOpsResponse{
			Ops: []credit.CreditOpSummary{{
				OpID:       "op-pay",
				OpKey:      "pay:" + payHashHex,
				Kind:       credit.KindPay,
				State:      credit.StateCompleted,
				CreditOnly: true,
				AmountSat:  20,
			}},
		},
	}
	swap := &fakeSwapService{}
	deps := &Deps{
		ActivityStore:  store,
		CreditRegistry: reg,
		SwapService:    swap,
		RPCServer:      &fakeRPCServer{},
	}
	runtime := newRuntime(t.Context(), deps)
	t.Cleanup(runtime.stop)

	return runtime, newHistory(deps, runtime),
		newInspectionService(deps, runtime), reg, swap
}

// listedEntry returns the entry with the given id from the persisted list.
func listedEntry(t *testing.T, h *history,
	id string) *wavewalletrpc.WalletEntry {

	t.Helper()

	list, err := h.listActivity(
		context.Background(), &wavewalletrpc.ListRequest{
			Limit: 50,
		},
	)
	require.NoError(t, err)

	for _, e := range list.GetEntries() {
		if e.GetId() == id {
			return e
		}
	}

	require.Failf(t, "entry not listed", "id %s", id)

	return nil
}

// TestCreditSendPreimageInPersistedList verifies a credit-only send's preimage
// reaches the persisted activity list and InspectActivity, not just the live
// stream.
func TestCreditSendPreimageInPersistedList(t *testing.T) {
	t.Parallel()

	runtime, h, inspection, _, swap := newPreimageFixture(t)
	swap.getSwapResp = &swapclientrpc.GetSwapResponse{
		Swap: &swapclientrpc.SwapSummary{
			PaymentHash: payHashHex,
			Preimage:    activityPreimageHex,
		},
	}

	runtime.pollCreditOps(make(map[string]credit.State))

	got := listedEntry(t, h, payHashHex)
	require.Equal(
		t, wavewalletrpc.EntryStatus_ENTRY_STATUS_COMPLETE,
		got.GetStatus(),
	)
	require.Equal(t, activityPreimageHex, got.GetProgress().GetPreimage())

	resp, err := inspection.InspectActivity(
		t.Context(), &wavewalletrpc.InspectActivityRequest{
			Id: payHashHex,
		},
	)
	require.NoError(t, err)
	require.Equal(
		t, activityPreimageHex, resp.GetEntry().GetProgress().
			GetPreimage(),
	)
}

// TestSwapSendPreimageInPersistedList verifies a swap-backed send's preimage,
// set by progressFromSwapSummary, is stored and returned by List and
// InspectActivity.
func TestSwapSendPreimageInPersistedList(t *testing.T) {
	t.Parallel()

	runtime, h, inspection, _, swap := newPreimageFixture(t)

	summary := &swapclientrpc.SwapSummary{
		PaymentHash: payHashHex,
		Direction:   swapclientrpc.SwapDirection_SWAP_DIRECTION_PAY,
		State:       swapclientrpc.SwapState_SWAP_STATE_COMPLETED,
		AmountSat:   5_000,
		Preimage:    activityPreimageHex,
	}
	swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{
		Swaps: []*swapclientrpc.SwapSummary{
			summary,
		},
	}

	require.NoError(
		t,
		runtime.fanOutSwapUpdate(
			&swapclientrpc.SubscribeSwapsResponse{
				Swap: summary,
			},
		),
	)

	got := listedEntry(t, h, payHashHex)
	require.Equal(t, activityPreimageHex, got.GetProgress().GetPreimage())

	resp, err := inspection.InspectActivity(
		t.Context(), &wavewalletrpc.InspectActivityRequest{
			Id: payHashHex,
		},
	)
	require.NoError(t, err)
	require.Equal(
		t, activityPreimageHex, resp.GetEntry().GetProgress().
			GetPreimage(),
	)
}

// TestCreditSendPreimageBackfill verifies the first poll after an upgrade
// re-projects a completed credit-only send that was stored without a
// preimage: the row gains it and a new transition is emitted.
func TestCreditSendPreimageBackfill(t *testing.T) {
	t.Parallel()

	runtime, h, _, _, swap := newPreimageFixture(t)

	// Before the upgrade the row was stored without a preimage.
	swap.getSwapErr = status.Error(codes.NotFound, "swap not found")
	runtime.pollCreditOps(make(map[string]credit.State))
	require.Empty(
		t, listedEntry(t, h, payHashHex).GetProgress().GetPreimage(),
	)

	// After the upgrade the swap row resolves, and a fresh poll (the
	// per-process projected map starts empty) backfills the row.
	swap.getSwapErr = nil
	swap.getSwapResp = &swapclientrpc.GetSwapResponse{
		Swap: &swapclientrpc.SwapSummary{
			PaymentHash: payHashHex,
			Preimage:    activityPreimageHex,
		},
	}
	sub := runtime.subscribe()
	runtime.pollCreditOps(make(map[string]credit.State))

	emitted := drainEntries(sub)
	require.Len(t, emitted, 1)
	require.Equal(
		t, activityPreimageHex, emitted[0].GetProgress().GetPreimage(),
	)
	require.Equal(
		t, activityPreimageHex,
		listedEntry(t, h, payHashHex).GetProgress().GetPreimage(),
	)

	// A further poll changes nothing and emits nothing.
	runtime.pollCreditOps(make(map[string]credit.State))
	require.Empty(t, drainEntries(sub))
}

// TestSwapSendPreimageBackfill verifies the monitor's include-existing replay
// of a completed swap-backed send stored without a preimage saves and emits it
// once the summary carries the preimage.
func TestSwapSendPreimageBackfill(t *testing.T) {
	t.Parallel()

	runtime, h, _, _, _ := newPreimageFixture(t)

	summary := &swapclientrpc.SwapSummary{
		PaymentHash: payHashHex,
		Direction:   swapclientrpc.SwapDirection_SWAP_DIRECTION_PAY,
		State:       swapclientrpc.SwapState_SWAP_STATE_COMPLETED,
		AmountSat:   5_000,
	}
	require.NoError(
		t,
		runtime.fanOutSwapUpdate(
			&swapclientrpc.SubscribeSwapsResponse{
				Swap: summary,
			},
		),
	)
	require.Empty(
		t, listedEntry(t, h, payHashHex).GetProgress().GetPreimage(),
	)

	sub := runtime.subscribe()
	summary.Preimage = activityPreimageHex
	require.NoError(
		t,
		runtime.fanOutSwapUpdate(
			&swapclientrpc.SubscribeSwapsResponse{
				Swap: summary,
			},
		),
	)

	require.Len(t, drainEntries(sub), 1)
	require.Equal(
		t, activityPreimageHex,
		listedEntry(t, h, payHashHex).GetProgress().GetPreimage(),
	)
}

// TestLaterProjectionKeepsStoredPreimage verifies a projection that carries no
// preimage, even one that advances the row, neither erases the stored
// preimage nor drops it from the entry that is emitted.
func TestLaterProjectionKeepsStoredPreimage(t *testing.T) {
	t.Parallel()

	runtime, h, _, _, _ := newPreimageFixture(t)

	pending := &swapclientrpc.SwapSummary{
		PaymentHash: payHashHex,
		Direction:   swapclientrpc.SwapDirection_SWAP_DIRECTION_PAY,
		State:       swapclientrpc.SwapState_SWAP_STATE_COMPLETED,
		AmountSat:   5_000,
		Preimage:    activityPreimageHex,
	}
	require.NoError(
		t,
		runtime.fanOutSwapUpdate(
			&swapclientrpc.SubscribeSwapsResponse{
				Swap: pending,
			},
		),
	)

	// A later summary of the same swap without the preimage, with a
	// different fee so the row genuinely changes.
	sub := runtime.subscribe()
	later := &swapclientrpc.SwapSummary{
		PaymentHash: payHashHex,
		Direction:   swapclientrpc.SwapDirection_SWAP_DIRECTION_PAY,
		State:       swapclientrpc.SwapState_SWAP_STATE_COMPLETED,
		AmountSat:   5_000,
		FeeSat:      7,
	}
	require.NoError(
		t,
		runtime.fanOutSwapUpdate(
			&swapclientrpc.SubscribeSwapsResponse{
				Swap: later,
			},
		),
	)

	emitted := drainEntries(sub)
	require.Len(t, emitted, 1)
	require.Equal(
		t, activityPreimageHex, emitted[0].GetProgress().GetPreimage(),
	)

	got := listedEntry(t, h, payHashHex)
	require.Equal(t, int64(7), got.GetFeeSat())
	require.Equal(t, activityPreimageHex, got.GetProgress().GetPreimage())
}
