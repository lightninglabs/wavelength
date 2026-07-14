//go:build walletdkrpc && swapruntime

package swapwallet

import (
	"testing"

	"github.com/lightninglabs/wavelength/rpc/walletdkrpc"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
)

// TestListViewActivityDefault confirms the default ListView (UNSPECIFIED)
// is treated as ACTIVITY so legacy callers that omit the field keep
// getting the merged WalletEntry stream.
func TestListViewActivityDefault(t *testing.T) {
	t.Parallel()

	h, _, _ := newHistoryFixture(t)
	resp, err := h.List(t.Context(), &walletdkrpc.ListRequest{})
	require.NoError(t, err)
	require.NotNil(
		t, resp.GetActivity(),
		"UNSPECIFIED view must populate Activity",
	)
	require.Nil(t, resp.GetVtxos())
	require.Nil(t, resp.GetOnchain())
}

// TestListViewVTXOsHidesTerminalStates confirms LIST_VIEW_VTXOS returns
// only live + still-actionable VTXOs; terminal internal states
// (forfeited / spent / failed) are filtered out so the wallet view stays
// focused on the VTXOs a user can still act on.
func TestListViewVTXOsHidesTerminalStates(t *testing.T) {
	t.Parallel()

	h, _, rpc := newHistoryFixture(t)
	rpc.listVTXOsResp = &waverpc.ListVTXOsResponse{
		Vtxos: []*waverpc.VTXO{
			{
				Outpoint:  "live1:0",
				AmountSat: 1_000,
				Status:    waverpc.VTXOStatus_VTXO_STATUS_LIVE,
			},
			{
				Outpoint:  "spent1:0",
				AmountSat: 2_000,
				Status:    waverpc.VTXOStatus_VTXO_STATUS_SPENT,
			},
			{
				Outpoint:  "forfeited1:0",
				AmountSat: 3_000,
				Status: waverpc.
					VTXOStatus_VTXO_STATUS_FORFEITED,
			},
			{
				Outpoint:  "live2:0",
				AmountSat: 4_000,
				Status:    waverpc.VTXOStatus_VTXO_STATUS_LIVE,
			},
		},
	}

	resp, err := h.List(t.Context(), &walletdkrpc.ListRequest{
		View: walletdkrpc.ListView_LIST_VIEW_VTXOS,
	})
	require.NoError(t, err)

	inv := resp.GetVtxos()
	require.NotNil(t, inv)
	require.Equal(
		t, uint32(2), inv.GetTotal(),
		"only live VTXOs should appear in the wallet view",
	)
	require.Len(t, inv.GetVtxos(), 2)
	require.Equal(t, "live1:0", inv.GetVtxos()[0].GetOutpoint())
	require.Equal(t, "live", inv.GetVtxos()[0].GetStatus())
	require.Equal(t, "live2:0", inv.GetVtxos()[1].GetOutpoint())
}

// TestListViewVTXOsPagination confirms offset+limit slice the inventory
// without losing the underlying total.
func TestListViewVTXOsPagination(t *testing.T) {
	t.Parallel()

	h, _, rpc := newHistoryFixture(t)
	rpc.listVTXOsResp = &waverpc.ListVTXOsResponse{
		Vtxos: []*waverpc.VTXO{
			{
				Outpoint: "a:0",
				Status:   waverpc.VTXOStatus_VTXO_STATUS_LIVE,
			},
			{
				Outpoint: "b:0",
				Status:   waverpc.VTXOStatus_VTXO_STATUS_LIVE,
			},
			{
				Outpoint: "c:0",
				Status:   waverpc.VTXOStatus_VTXO_STATUS_LIVE,
			},
		},
	}

	resp, err := h.List(t.Context(), &walletdkrpc.ListRequest{
		View:   walletdkrpc.ListView_LIST_VIEW_VTXOS,
		Limit:  2,
		Offset: 1,
	})
	require.NoError(t, err)
	inv := resp.GetVtxos()
	require.Equal(t, uint32(3), inv.GetTotal())
	require.Len(t, inv.GetVtxos(), 2)
	require.Equal(t, "b:0", inv.GetVtxos()[0].GetOutpoint())
	require.Equal(t, "c:0", inv.GetVtxos()[1].GetOutpoint())
}

// TestListViewVTXOsRequiresRPCServer confirms an unconfigured RPC backend
// returns ErrSwapBackendUnavailable rather than a panic.
func TestListViewVTXOsRequiresRPCServer(t *testing.T) {
	t.Parallel()

	deps := &Deps{}
	runtime := newRuntime(t.Context(), deps)
	t.Cleanup(runtime.stop)

	h := newHistory(deps, runtime)
	_, err := h.List(t.Context(), &walletdkrpc.ListRequest{
		View: walletdkrpc.ListView_LIST_VIEW_VTXOS,
	})
	require.ErrorIs(t, err, ErrSwapBackendUnavailable)
}

// TestListViewOnchainFlattensLedgerRows confirms LIST_VIEW_ONCHAIN
// projects daemon TransactionHistoryEntry rows onto the wallet-facing
// OnchainTx shape and preserves has_more.
func TestListViewOnchainFlattensLedgerRows(t *testing.T) {
	t.Parallel()

	h, _, rpc := newHistoryFixture(t)
	rpc.listTxResp = &waverpc.ListTransactionsResponse{
		Transactions: []*waverpc.TransactionHistoryEntry{
			{
				Type:               "boarding",
				ConfirmationStatus: "confirmed",
				AmountSat:          25_000,
				FeeSat:             0,
				Txid:               "boarding-txid",
				ConfirmationHeight: 1234,
				CreatedAtUnixS:     500,
				Description:        "boarding deposit",
			},
			{
				Type:               "sweep",
				ConfirmationStatus: "broadcast",
				AmountSat:          15_000,
				FeeSat:             1_000,
				Txid:               "sweep-txid",
				CreatedAtUnixS:     600,
				Description:        "boarding sweep",
			},
		},
		HasMore: true,
	}

	resp, err := h.List(t.Context(), &walletdkrpc.ListRequest{
		View:  walletdkrpc.ListView_LIST_VIEW_ONCHAIN,
		Limit: 50,
	})
	require.NoError(t, err)

	hist := resp.GetOnchain()
	require.NotNil(t, hist)
	require.Len(t, hist.GetTxs(), 2)
	require.True(t, hist.GetHasMore())

	first := hist.GetTxs()[0]
	require.Equal(t, "boarding-txid", first.GetTxid())
	require.Equal(t, "boarding", first.GetKind())
	require.Equal(t, int64(25_000), first.GetAmountSat())
	require.Equal(t, "confirmed", first.GetStatus())
	require.Equal(t, int32(1234), first.GetConfirmationHeight())
	require.Equal(t, "boarding deposit", first.GetDescription())

	second := hist.GetTxs()[1]
	require.Equal(t, "sweep-txid", second.GetTxid())
	require.Equal(t, int64(1_000), second.GetFeeSat())
}

// TestListViewActivityForwardsLegacyFlags confirms the activity dispatch
// still honours pending_only and kinds, which are activity-only filters.
func TestListViewActivityForwardsLegacyFlags(t *testing.T) {
	t.Parallel()

	h, _, rpc := newHistoryFixture(t)
	rpc.listTxResp = &waverpc.ListTransactionsResponse{
		Transactions: []*waverpc.TransactionHistoryEntry{
			{
				Type:               "boarding",
				ConfirmationStatus: "confirmed",
				AmountSat:          5_000,
				Txid:               "txid-confirmed",
				CreatedAtUnixS:     100,
			},
			{
				Type:               "boarding",
				ConfirmationStatus: "pending",
				AmountSat:          7_000,
				Txid:               "txid-pending",
				CreatedAtUnixS:     200,
			},
		},
	}

	resp, err := h.List(t.Context(), &walletdkrpc.ListRequest{
		View:        walletdkrpc.ListView_LIST_VIEW_ACTIVITY,
		PendingOnly: true,
		Kinds: []walletdkrpc.EntryKind{
			walletdkrpc.EntryKind_ENTRY_KIND_DEPOSIT,
		},
	})
	require.NoError(t, err)

	activity := resp.GetActivity()
	require.NotNil(t, activity)
	require.Len(t, activity.GetEntries(), 1)
	require.Equal(
		t, "txid-pending", activity.GetEntries()[0].GetId(),
	)
}
