//go:build walletrpc && swapruntime

package swapwallet

import (
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/ledger"
	"github.com/lightninglabs/darepo-client/rpc/swapclientrpc"
	"github.com/lightninglabs/darepo-client/rpc/walletrpc"
	"github.com/stretchr/testify/require"
)

// newHistoryFixture wires a history merger with fake swap and RPC sources.
func newHistoryFixture(t *testing.T) (*history, *fakeSwapService,
	*fakeRPCServer) {

	t.Helper()

	swap := &fakeSwapService{}
	rpc := &fakeRPCServer{}
	deps := &Deps{
		SwapService: swap,
		RPCServer:   rpc,
	}
	runtime := newRuntime(t.Context(), deps)
	t.Cleanup(runtime.stop)

	return newHistory(deps, runtime), swap, rpc
}

// testBytes returns deterministic byte slices for session and round ids.
func testBytes(length int, seed byte) []byte {
	out := make([]byte, length)
	for i := range out {
		out[i] = seed + byte(i)
	}

	return out
}

// testSessionString returns the production display form for raw OOR session
// bytes.
func testSessionString(t *testing.T, raw []byte) string {
	t.Helper()

	hash, err := chainhash.NewHash(raw)
	require.NoError(t, err)

	return hash.String()
}

// TestHistoryListMergesSwapAndLedgerSources confirms the merger combines
// rows from both backends and normalizes them into the flat WalletEntry
// shape.
func TestHistoryListMergesSwapAndLedgerSources(t *testing.T) {
	t.Parallel()

	h, swap, rpc := newHistoryFixture(t)
	swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{
		Swaps: []*swapclientrpc.SwapSummary{
			{
				PaymentHash: "hash1",
				Direction: swapclientrpc.
					SwapDirection_SWAP_DIRECTION_PAY,
				State: swapclientrpc.
					SwapState_SWAP_STATE_COMPLETED,
				AmountSat:     10_000,
				UpdatedAtUnix: 200,
			},
			{
				PaymentHash: "hash2",
				Direction: swapclientrpc.
					SwapDirection_SWAP_DIRECTION_RECEIVE,
				Pending:       true,
				AmountSat:     5_000,
				UpdatedAtUnix: 300,
			},
		},
	}
	rpc.listTxResp = &daemonrpc.ListTransactionsResponse{
		Transactions: []*daemonrpc.TransactionHistoryEntry{
			{
				Type:               "boarding",
				ConfirmationStatus: "confirmed",
				AmountSat:          50_000,
				Txid:               "txid_deposit",
				CreatedAtUnixS:     100,
			},
			{
				Type:               "sweep",
				ConfirmationStatus: "confirmed",
				AmountSat:          15_000,
				Txid:               "txid_exit",
				CreatedAtUnixS:     250,
			},
		},
	}

	resp, err := h.List(t.Context(), &walletrpc.ListRequest{})
	require.NoError(t, err)
	require.Len(t, resp.GetActivity().GetEntries(), 4)

	// Sort order is by updated_at descending: hash2(300), exit(250),
	// hash1(200), deposit(100).
	require.Equal(t, "hash2", resp.GetActivity().GetEntries()[0].GetId())
	require.Equal(
		t, "txid_exit", resp.GetActivity().GetEntries()[1].GetId(),
	)
	require.Equal(t, "hash1", resp.GetActivity().GetEntries()[2].GetId())
	require.Equal(
		t, "txid_deposit", resp.GetActivity().GetEntries()[3].GetId(),
	)

	// Kinds and statuses normalize correctly.
	require.Equal(
		t, walletrpc.EntryKind_ENTRY_KIND_RECV,
		resp.GetActivity().GetEntries()[0].GetKind(),
	)
	require.Equal(
		t, walletrpc.EntryStatus_ENTRY_STATUS_PENDING,
		resp.GetActivity().GetEntries()[0].GetStatus(),
	)
	require.Equal(
		t, walletrpc.EntryKind_ENTRY_KIND_EXIT,
		resp.GetActivity().GetEntries()[1].GetKind(),
	)
	require.Equal(
		t, walletrpc.EntryStatus_ENTRY_STATUS_COMPLETE,
		resp.GetActivity().GetEntries()[1].GetStatus(),
	)
	require.Equal(
		t, walletrpc.EntryKind_ENTRY_KIND_DEPOSIT,
		resp.GetActivity().GetEntries()[3].GetKind(),
	)
}

// TestHistoryPendingFilterDropsTerminal confirms pending_only=true
// drops COMPLETE and FAILED rows from both sources.
func TestHistoryPendingFilterDropsTerminal(t *testing.T) {
	t.Parallel()

	h, swap, rpc := newHistoryFixture(t)
	swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{
		Swaps: []*swapclientrpc.SwapSummary{
			{
				PaymentHash: "live",
				Direction: swapclientrpc.
					SwapDirection_SWAP_DIRECTION_PAY,
				Pending:       true,
				UpdatedAtUnix: 100,
			},
			{
				PaymentHash: "done",
				Direction: swapclientrpc.
					SwapDirection_SWAP_DIRECTION_PAY,
				State: swapclientrpc.
					SwapState_SWAP_STATE_COMPLETED,
				UpdatedAtUnix: 200,
			},
		},
	}
	rpc.listTxResp = &daemonrpc.ListTransactionsResponse{}

	resp, err := h.List(t.Context(), &walletrpc.ListRequest{
		PendingOnly: true,
	})
	require.NoError(t, err)
	require.Len(t, resp.GetActivity().GetEntries(), 1)
	require.Equal(t, "live", resp.GetActivity().GetEntries()[0].GetId())
}

// TestHistorySurfacesPendingBoardingBalance confirms unconfirmed boarding
// funds show up even before ListTransactions has a confirmed ledger row.
func TestHistorySurfacesPendingBoardingBalance(t *testing.T) {
	t.Parallel()

	h, swap, rpc := newHistoryFixture(t)
	swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{}
	rpc.listTxResp = &daemonrpc.ListTransactionsResponse{}
	rpc.getBalanceResp = &daemonrpc.GetBalanceResponse{
		BoardingUnconfirmedSat: 12345,
	}

	resp, err := h.List(t.Context(), &walletrpc.ListRequest{})
	require.NoError(t, err)

	entries := resp.GetActivity().GetEntries()
	require.Len(t, entries, 1)
	require.Equal(t, "boarding-unconfirmed", entries[0].GetId())
	require.Equal(
		t, walletrpc.EntryKind_ENTRY_KIND_DEPOSIT, entries[0].GetKind(),
	)
	require.Equal(
		t, walletrpc.EntryStatus_ENTRY_STATUS_PENDING,
		entries[0].GetStatus(),
	)
	require.Equal(t, int64(12345), entries[0].GetAmountSat())
	require.Equal(
		t,
		walletrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_WAITING_FOR_CONFIRMATION,
		entries[0].GetProgress().GetPhase(),
	)
	require.Equal(
		t, "waiting_for_confirmation",
		entries[0].GetProgress().GetPhaseLabel(),
	)
}

// TestHistoryHidesReceiveClaimOORSend confirms wallet activity does not show
// the outgoing OOR claim input that is paired with an incoming materialized
// VTXO for the same receive-swap claim.
func TestHistoryHidesReceiveClaimOORSend(t *testing.T) {
	t.Parallel()

	h, swap, rpc := newHistoryFixture(t)

	sessionID := []byte{
		0x82, 0x0b, 0x18, 0x9f, 0xdf, 0xa9, 0x68, 0xde,
		0x96, 0x6b, 0xe2, 0xf9, 0xa5, 0xad, 0xc0, 0xa5,
		0x64, 0xff, 0xbb, 0x98, 0x49, 0xc7, 0x12, 0x0e,
		0xac, 0x5b, 0xc4, 0x0d, 0x7b, 0x64, 0xcd, 0x51,
	}
	sessionHex := testSessionString(t, sessionID)

	swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{
		Swaps: []*swapclientrpc.SwapSummary{
			{
				PaymentHash: "payment-hash",
				Direction: swapclientrpc.
					SwapDirection_SWAP_DIRECTION_RECEIVE,
				State: swapclientrpc.
					SwapState_SWAP_STATE_COMPLETED,
				AmountSat:      1_000,
				UpdatedAtUnix:  200,
				ClaimSessionId: sessionHex,
			},
		},
	}
	rpc.listTxResp = &daemonrpc.ListTransactionsResponse{
		Transactions: []*daemonrpc.TransactionHistoryEntry{
			{
				Type:           "oor",
				Subtype:        ledger.EventVTXOSent,
				AmountSat:      1_000,
				DebitAccount:   ledger.AccountTransfersOut,
				CreditAccount:  ledger.AccountVTXOBalance,
				SessionId:      sessionID,
				EntryId:        3,
				CreatedAtUnixS: 100,
			},
			{
				Type:           "round",
				Subtype:        ledger.EventVTXOReceived,
				AmountSat:      1_000,
				DebitAccount:   ledger.AccountVTXOBalance,
				CreditAccount:  ledger.AccountTransfersIn,
				Txid:           sessionHex,
				OutputIndex:    0,
				EntryId:        4,
				CreatedAtUnixS: 101,
			},
		},
	}

	resp, err := h.List(t.Context(), &walletrpc.ListRequest{})
	require.NoError(t, err)

	entries := resp.GetActivity().GetEntries()
	require.Len(t, entries, 1)
	require.Equal(t, "payment-hash", entries[0].GetId())
	require.Equal(
		t, walletrpc.EntryKind_ENTRY_KIND_RECV, entries[0].GetKind(),
	)
}

// TestHistoryKeepsUnpairedOORSend confirms ordinary OOR sends remain visible
// when there is no matching incoming materialization row.
func TestHistoryKeepsUnpairedOORSend(t *testing.T) {
	t.Parallel()

	h, swap, rpc := newHistoryFixture(t)
	swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{}
	rpc.listTxResp = &daemonrpc.ListTransactionsResponse{
		Transactions: []*daemonrpc.TransactionHistoryEntry{
			{
				Type:           "oor",
				Subtype:        ledger.EventVTXOSent,
				AmountSat:      1_000,
				DebitAccount:   ledger.AccountTransfersOut,
				CreditAccount:  ledger.AccountVTXOBalance,
				SessionId:      testBytes(32, 0x11),
				EntryId:        3,
				CreatedAtUnixS: 100,
			},
		},
	}

	resp, err := h.List(t.Context(), &walletrpc.ListRequest{})
	require.NoError(t, err)

	entries := resp.GetActivity().GetEntries()
	require.Len(t, entries, 1)
	require.Equal(
		t, walletrpc.EntryKind_ENTRY_KIND_SEND, entries[0].GetKind(),
	)
	require.Equal(t, int64(-1_000), entries[0].GetAmountSat())
}

// TestHistoryHidesPayFundingOORInput confirms wallet activity does not show
// the full input consumed to fund a pay-swap vHTLC when the input's change
// came back to the wallet and the delta is already represented by the swap
// SEND row.
func TestHistoryHidesPayFundingOORInput(t *testing.T) {
	t.Parallel()

	h, swap, rpc := newHistoryFixture(t)

	sessionID := testBytes(32, 0x21)
	sessionHex := testSessionString(t, sessionID)

	swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{
		Swaps: []*swapclientrpc.SwapSummary{
			{
				PaymentHash: "payment-hash",
				Direction: swapclientrpc.
					SwapDirection_SWAP_DIRECTION_PAY,
				Pending:          true,
				AmountSat:        1_234,
				UpdatedAtUnix:    200,
				FundingSessionId: sessionHex,
			},
		},
	}
	rpc.listTxResp = &daemonrpc.ListTransactionsResponse{
		Transactions: []*daemonrpc.TransactionHistoryEntry{
			{
				Type:           "oor",
				Subtype:        ledger.EventVTXOSent,
				AmountSat:      999_745,
				DebitAccount:   ledger.AccountTransfersOut,
				CreditAccount:  ledger.AccountVTXOBalance,
				SessionId:      sessionID,
				EntryId:        13,
				CreatedAtUnixS: 100,
			},
			{
				Type:           "round",
				Subtype:        ledger.EventVTXOReceived,
				AmountSat:      998_511,
				DebitAccount:   ledger.AccountVTXOBalance,
				CreditAccount:  ledger.AccountTransfersIn,
				Txid:           sessionHex,
				OutputIndex:    1,
				EntryId:        14,
				CreatedAtUnixS: 101,
			},
		},
	}

	resp, err := h.List(t.Context(), &walletrpc.ListRequest{})
	require.NoError(t, err)

	entries := resp.GetActivity().GetEntries()
	require.Len(t, entries, 1)
	require.Equal(t, "payment-hash", entries[0].GetId())
	require.Equal(
		t, walletrpc.EntryKind_ENTRY_KIND_SEND, entries[0].GetKind(),
	)
	require.Equal(t, int64(-1_234), entries[0].GetAmountSat())
}

// TestHistoryKeepsSameAmountUnmatchedFundingInput confirms pay-swap funding
// legs are hidden by funding session, not by amount alone.
func TestHistoryKeepsSameAmountUnmatchedFundingInput(t *testing.T) {
	t.Parallel()

	h, swap, rpc := newHistoryFixture(t)

	matchedSession := testBytes(32, 0x41)
	matchedHex := testSessionString(t, matchedSession)
	otherSession := testBytes(32, 0x42)
	otherHex := testSessionString(t, otherSession)

	swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{
		Swaps: []*swapclientrpc.SwapSummary{
			{
				PaymentHash: "payment-hash",
				Direction: swapclientrpc.
					SwapDirection_SWAP_DIRECTION_PAY,
				Pending:          true,
				AmountSat:        1_234,
				UpdatedAtUnix:    200,
				FundingSessionId: matchedHex,
			},
		},
	}
	rpc.listTxResp = &daemonrpc.ListTransactionsResponse{
		Transactions: []*daemonrpc.TransactionHistoryEntry{
			{
				Type:           "oor",
				Subtype:        ledger.EventVTXOSent,
				AmountSat:      999_745,
				DebitAccount:   ledger.AccountTransfersOut,
				CreditAccount:  ledger.AccountVTXOBalance,
				SessionId:      matchedSession,
				EntryId:        13,
				CreatedAtUnixS: 100,
			},
			{
				Type:          "round",
				Subtype:       ledger.EventVTXOReceived,
				AmountSat:     998_511,
				DebitAccount:  ledger.AccountVTXOBalance,
				CreditAccount: ledger.AccountTransfersIn,
				Txid:          matchedHex,
				OutputIndex:   1,
				EntryId:       14,
			},
			{
				Type:           "oor",
				Subtype:        ledger.EventVTXOSent,
				AmountSat:      999_745,
				DebitAccount:   ledger.AccountTransfersOut,
				CreditAccount:  ledger.AccountVTXOBalance,
				SessionId:      otherSession,
				EntryId:        15,
				CreatedAtUnixS: 99,
			},
			{
				Type:          "round",
				Subtype:       ledger.EventVTXOReceived,
				AmountSat:     998_511,
				DebitAccount:  ledger.AccountVTXOBalance,
				CreditAccount: ledger.AccountTransfersIn,
				Txid:          otherHex,
				OutputIndex:   1,
				EntryId:       16,
			},
		},
	}

	resp, err := h.List(t.Context(), &walletrpc.ListRequest{})
	require.NoError(t, err)

	entries := resp.GetActivity().GetEntries()
	require.Len(t, entries, 2)
	require.Equal(t, "payment-hash", entries[0].GetId())
	require.Equal(t, "ledger-15", entries[1].GetId())
	require.Equal(t, int64(-999_745), entries[1].GetAmountSat())
}

// TestHistoryKeepsOORSendWithChangeWithoutSwap confirms the change-pairing
// heuristic is anchored to a visible swap SEND, so ordinary OOR sends are not
// hidden just because they return change to this wallet.
func TestHistoryKeepsOORSendWithChangeWithoutSwap(t *testing.T) {
	t.Parallel()

	h, swap, rpc := newHistoryFixture(t)

	sessionID := testBytes(32, 0x31)
	sessionHex := testSessionString(t, sessionID)

	swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{}
	rpc.listTxResp = &daemonrpc.ListTransactionsResponse{
		Transactions: []*daemonrpc.TransactionHistoryEntry{
			{
				Type:           "oor",
				Subtype:        ledger.EventVTXOSent,
				AmountSat:      999_745,
				DebitAccount:   ledger.AccountTransfersOut,
				CreditAccount:  ledger.AccountVTXOBalance,
				SessionId:      sessionID,
				EntryId:        13,
				CreatedAtUnixS: 100,
			},
			{
				Type:           "round",
				Subtype:        ledger.EventVTXOReceived,
				AmountSat:      998_511,
				DebitAccount:   ledger.AccountVTXOBalance,
				CreditAccount:  ledger.AccountTransfersIn,
				Txid:           sessionHex,
				OutputIndex:    1,
				EntryId:        14,
				CreatedAtUnixS: 101,
			},
		},
	}

	resp, err := h.List(t.Context(), &walletrpc.ListRequest{})
	require.NoError(t, err)

	entries := resp.GetActivity().GetEntries()
	require.Len(t, entries, 1)
	require.Equal(
		t, walletrpc.EntryKind_ENTRY_KIND_SEND, entries[0].GetKind(),
	)
	require.Equal(t, int64(-999_745), entries[0].GetAmountSat())
}

// TestHistoryPendingFilterIncludesPendingBoarding confirms --pending includes
// the synthetic unconfirmed boarding row.
func TestHistoryPendingFilterIncludesPendingBoarding(t *testing.T) {
	t.Parallel()

	h, swap, rpc := newHistoryFixture(t)
	swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{}
	rpc.listTxResp = &daemonrpc.ListTransactionsResponse{
		Transactions: []*daemonrpc.TransactionHistoryEntry{
			{
				Type:               "boarding",
				ConfirmationStatus: "confirmed",
				AmountSat:          50_000,
				Txid:               "confirmed-deposit",
				CreatedAtUnixS:     100,
			},
		},
	}
	rpc.getBalanceResp = &daemonrpc.GetBalanceResponse{
		BoardingUnconfirmedSat: 12345,
	}

	resp, err := h.List(t.Context(), &walletrpc.ListRequest{
		PendingOnly: true,
	})
	require.NoError(t, err)

	entries := resp.GetActivity().GetEntries()
	require.Len(t, entries, 1)
	require.Equal(t, "boarding-unconfirmed", entries[0].GetId())
}

// TestHistoryKindFilter confirms the kinds filter narrows the result.
func TestHistoryKindFilter(t *testing.T) {
	t.Parallel()

	h, swap, rpc := newHistoryFixture(t)
	swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{
		Swaps: []*swapclientrpc.SwapSummary{
			{
				PaymentHash: "send-row",
				Direction: swapclientrpc.
					SwapDirection_SWAP_DIRECTION_PAY,
				State: swapclientrpc.
					SwapState_SWAP_STATE_COMPLETED,
				UpdatedAtUnix: 100,
			},
		},
	}
	rpc.listTxResp = &daemonrpc.ListTransactionsResponse{
		Transactions: []*daemonrpc.TransactionHistoryEntry{
			{
				Type:               "boarding",
				ConfirmationStatus: "confirmed",
				Txid:               "deposit-row",
				CreatedAtUnixS:     50,
			},
		},
	}

	resp, err := h.List(t.Context(), &walletrpc.ListRequest{
		Kinds: []walletrpc.EntryKind{
			walletrpc.EntryKind_ENTRY_KIND_DEPOSIT,
		},
	})
	require.NoError(t, err)
	require.Len(t, resp.GetActivity().GetEntries(), 1)
	require.Equal(
		t, "deposit-row", resp.GetActivity().GetEntries()[0].GetId(),
	)
}

// TestHistoryKindFilterRejectsUnsupportedKind confirms raw RPC callers cannot
// silently filter on ENTRY_KIND_UNSPECIFIED and receive an empty page.
func TestHistoryKindFilterRejectsUnsupportedKind(t *testing.T) {
	t.Parallel()

	h, _, _ := newHistoryFixture(t)
	_, err := h.List(t.Context(), &walletrpc.ListRequest{
		Kinds: []walletrpc.EntryKind{
			walletrpc.EntryKind_ENTRY_KIND_UNSPECIFIED,
		},
	})
	require.ErrorIs(t, err, ErrUnsupportedKind)
}

// TestHistorySwapRowsIgnoreTimedOutOverlay confirms swap-backed rows use the
// swap FSM as their source of truth instead of wallet timeout overlays, even
// before the lazy swap summary has a populated direction.
func TestHistorySwapRowsIgnoreTimedOutOverlay(t *testing.T) {
	t.Parallel()

	h, swap, rpc := newHistoryFixture(t)
	swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{
		Swaps: []*swapclientrpc.SwapSummary{
			{
				PaymentHash:   "stuck",
				Pending:       true,
				UpdatedAtUnix: 100,
			},
		},
	}
	rpc.listTxResp = &daemonrpc.ListTransactionsResponse{}

	// Inject overlay directly.
	h.runtime.pendingMu.Lock()
	h.runtime.overlay["stuck"] = overlayStatus{
		status: walletrpc.
			EntryStatus_ENTRY_STATUS_FAILED,
		failureReason: "timed_out",
	}
	h.runtime.pendingMu.Unlock()

	resp, err := h.List(t.Context(), &walletrpc.ListRequest{})
	require.NoError(t, err)
	require.Len(t, resp.GetActivity().GetEntries(), 1)
	require.Equal(
		t, walletrpc.EntryStatus_ENTRY_STATUS_PENDING,
		resp.GetActivity().GetEntries()[0].GetStatus(),
	)
	require.Equal(
		t, walletrpc.EntryKind_ENTRY_KIND_UNSPECIFIED,
		resp.GetActivity().GetEntries()[0].GetKind(),
	)
	require.Empty(t, resp.GetActivity().GetEntries()[0].GetFailureReason())
}

// TestHistoryWalletRowsApplyTimedOutOverlay confirms wallet-local pending rows
// still surface the runtime's deadline projection in history.List.
func TestHistoryWalletRowsApplyTimedOutOverlay(t *testing.T) {
	t.Parallel()

	h, swap, rpc := newHistoryFixture(t)
	swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{}
	rpc.listTxResp = &daemonrpc.ListTransactionsResponse{
		Transactions: []*daemonrpc.TransactionHistoryEntry{
			{
				Txid:               "exit-txid",
				Type:               "sweep",
				ConfirmationStatus: "pending",
				CreatedAtUnixS:     100,
			},
		},
	}

	// Inject overlay directly.
	h.runtime.pendingMu.Lock()
	h.runtime.overlay["exit-txid"] = overlayStatus{
		status: walletrpc.
			EntryStatus_ENTRY_STATUS_FAILED,
		failureReason: "timed_out",
	}
	h.runtime.pendingMu.Unlock()

	resp, err := h.List(t.Context(), &walletrpc.ListRequest{})
	require.NoError(t, err)
	require.Len(t, resp.GetActivity().GetEntries(), 1)
	require.Equal(
		t, walletrpc.EntryStatus_ENTRY_STATUS_FAILED,
		resp.GetActivity().GetEntries()[0].GetStatus(),
	)
	require.Equal(
		t, "timed_out",
		resp.GetActivity().GetEntries()[0].GetFailureReason(),
	)
}

// TestHistorySwapRowIdIsPaymentHash confirms that a swap-side row
// surfaces in List under its payment_hash — the wallet-layer's stable
// canonical id for SEND-invoice and RECV across the entire lifecycle.
// EXIT and DEPOSIT correlation is a v2 task; see swapwallet/doc.go.
func TestHistorySwapRowIdIsPaymentHash(t *testing.T) {
	t.Parallel()

	h, swap, _ := newHistoryFixture(t)
	swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{
		Swaps: []*swapclientrpc.SwapSummary{
			{
				PaymentHash: "the-payment-hash",
				Invoice:     "lnbc1history",
				Direction: swapclientrpc.
					SwapDirection_SWAP_DIRECTION_PAY,
				State: swapclientrpc.
					SwapState_SWAP_STATE_COMPLETED,
				UpdatedAtUnix: 100,
			},
		},
	}

	resp, err := h.List(t.Context(), &walletrpc.ListRequest{})
	require.NoError(t, err)
	require.Len(t, resp.GetActivity().GetEntries(), 1)
	require.Equal(
		t, "the-payment-hash",
		resp.GetActivity().GetEntries()[0].GetId(),
		"swap row id must surface as payment_hash",
	)
	require.Equal(
		t, "lnbc1history",
		resp.GetActivity().GetEntries()[0].GetRequest().
			GetLightningInvoice().GetInvoice(),
	)
}

// TestHistoryPagination confirms offset+limit produce the expected slice
// and total tracks the pre-pagination count.
func TestHistoryPagination(t *testing.T) {
	t.Parallel()

	h, swap, rpc := newHistoryFixture(t)
	swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{}
	txns := make([]*daemonrpc.TransactionHistoryEntry, 0, 5)
	for i := 0; i < 5; i++ {
		txns = append(txns, &daemonrpc.TransactionHistoryEntry{
			Type:               "boarding",
			ConfirmationStatus: "confirmed",
			Txid:               "deposit-" + string(rune('a'+i)),
			CreatedAtUnixS:     int64(100 + i),
		})
	}
	rpc.listTxResp = &daemonrpc.ListTransactionsResponse{
		Transactions: txns,
	}

	resp, err := h.List(t.Context(), &walletrpc.ListRequest{
		Offset: 1,
		Limit:  2,
	})
	require.NoError(t, err)
	require.Equal(t, uint32(5), resp.GetActivity().GetTotal())
	require.Len(t, resp.GetActivity().GetEntries(), 2)
}

// TestHistoryClassifiesOORLedgerRows confirms OOR ledger rows are
// classified onto the right wallet kind and amount sign by inspecting
// the counterparty account. The ledger books OOR receives with
// transfers_in on the credit side and OOR sends with transfers_out on
// the debit side; the previous magic-string check ("wallet_in" /
// "wallet_out") never matched and silently dropped every OOR row.
func TestHistoryClassifiesOORLedgerRows(t *testing.T) {
	t.Parallel()

	h, swap, rpc := newHistoryFixture(t)
	swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{}
	rpc.listTxResp = &daemonrpc.ListTransactionsResponse{
		Transactions: []*daemonrpc.TransactionHistoryEntry{
			{
				Type:               "oor",
				ConfirmationStatus: "confirmed",
				AmountSat:          7_000,
				Txid:               "oor-recv-txid",
				CreatedAtUnixS:     100,
				DebitAccount:       ledger.AccountVTXOBalance,
				CreditAccount:      ledger.AccountTransfersIn,
			},
			{
				Type:               "oor",
				ConfirmationStatus: "confirmed",
				AmountSat:          3_000,
				Txid:               "oor-send-txid",
				CreatedAtUnixS:     200,
				DebitAccount:       ledger.AccountTransfersOut,
				CreditAccount:      ledger.AccountVTXOBalance,
			},
			{
				// Internal bookkeeping row with no
				// wallet-facing counterparty — must stay
				// hidden.
				Type:               "oor",
				ConfirmationStatus: "confirmed",
				AmountSat:          50,
				Txid:               "oor-bookkeeping",
				CreatedAtUnixS:     150,
				DebitAccount:       ledger.AccountOnchainFees,
				CreditAccount:      ledger.AccountVTXOBalance,
			},
		},
	}

	resp, err := h.List(t.Context(), &walletrpc.ListRequest{})
	require.NoError(t, err)
	require.Len(
		t, resp.GetActivity().GetEntries(), 2, "OOR send + OOR "+
			"recv must surface; bookkeeping row stays hidden",
	)

	// Sort is updated_at desc: send(200) before recv(100).
	send := resp.GetActivity().GetEntries()[0]
	require.Equal(t, "oor-send-txid", send.GetId())
	require.Equal(
		t, walletrpc.EntryKind_ENTRY_KIND_SEND, send.GetKind(),
	)
	require.Equal(
		t, int64(-3_000), send.GetAmountSat(),
		"SEND amount must be negative",
	)

	recv := resp.GetActivity().GetEntries()[1]
	require.Equal(t, "oor-recv-txid", recv.GetId())
	require.Equal(
		t, walletrpc.EntryKind_ENTRY_KIND_RECV, recv.GetKind(),
	)
	require.Equal(
		t, int64(7_000), recv.GetAmountSat(),
		"RECV amount must be positive",
	)
}

// TestHistoryDedupesByID confirms a single logical operation that
// surfaces from BOTH the swap subsystem (ListSwaps) and the ledger
// (ListTransactions) collapses to ONE WalletEntry when the two rows
// happen to share the same id. The dedupe keeps the most-recent
// updated_at (the ledger confirmation typically wins).
func TestHistoryDedupesByID(t *testing.T) {
	t.Parallel()

	h, swap, rpc := newHistoryFixture(t)
	swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{
		Swaps: []*swapclientrpc.SwapSummary{
			{
				PaymentHash: "shared-id",
				Direction: swapclientrpc.
					SwapDirection_SWAP_DIRECTION_PAY,
				State: swapclientrpc.
					SwapState_SWAP_STATE_COMPLETED,
				AmountSat:     10_000,
				UpdatedAtUnix: 100,
			},
		},
	}
	rpc.listTxResp = &daemonrpc.ListTransactionsResponse{
		Transactions: []*daemonrpc.TransactionHistoryEntry{
			{
				Type:               "oor",
				ConfirmationStatus: "confirmed",
				AmountSat:          10_000,
				Txid:               "shared-id",
				CreatedAtUnixS:     200,
				DebitAccount:       ledger.AccountTransfersOut,
				CreditAccount:      ledger.AccountVTXOBalance,
			},
		},
	}

	resp, err := h.List(t.Context(), &walletrpc.ListRequest{})
	require.NoError(t, err)
	require.Len(
		t, resp.GetActivity().GetEntries(), 1,
		"swap + ledger surfacing the same id must collapse to one row",
	)
	require.Equal(
		t, int64(200),
		resp.GetActivity().GetEntries()[0].GetUpdatedAtUnix(),
		"the more-recent row (ledger confirmation) must win",
	)
}

// TestHistoryPaginationOffsetPlumbedToLedger confirms that requesting
// page 2 (offset>=limit) of wallet history returns the expected ledger
// rows. Prior to the fix, collectLedgerEntries passed only Limit (not
// Offset) and the daemon's first-N rows were all that ever came back,
// so the in-memory paginate slice at [limit:] produced empty.
func TestHistoryPaginationOffsetPlumbedToLedger(t *testing.T) {
	t.Parallel()

	h, swap, rpc := newHistoryFixture(t)
	swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{}

	// 20 deposit rows so page 2 must be served from the ledger.
	txns := make([]*daemonrpc.TransactionHistoryEntry, 0, 20)
	for i := 0; i < 20; i++ {
		txns = append(txns, &daemonrpc.TransactionHistoryEntry{
			Type:               "boarding",
			ConfirmationStatus: "confirmed",
			Txid:               fmt.Sprintf("deposit-%02d", i),
			AmountSat:          1_000 + int64(i),
			CreatedAtUnixS:     int64(100 + i),
		})
	}
	rpc.listTxResp = &daemonrpc.ListTransactionsResponse{
		Transactions: txns,
	}

	resp, err := h.List(t.Context(), &walletrpc.ListRequest{
		Offset: 10,
		Limit:  5,
	})
	require.NoError(t, err)
	require.Equal(
		t, uint32(20), resp.GetActivity().GetTotal(),
		"total must reflect the full unfiltered set",
	)
	require.Len(
		t, resp.GetActivity().GetEntries(), 5,
		"page 2 must return a full window of 5 entries",
	)

	// The fake doesn't apply offset to its slice, so the daemon's
	// asserted Limit must be at least offset+limit = 15.
	require.GreaterOrEqual(
		t, rpc.listTxLastReq.GetLimit(), uint32(15),
		"ListTransactions must be called with Limit >= "+
			"offset+limit so the in-memory paginate has enough "+
			"rows to satisfy the requested page",
	)
}

// TestHistoryDepositCompletesOnChainConfirmation confirms that a DEPOSIT row
// mirrors the ledger confirmation status. More detailed boarding lifecycle
// information belongs in progress/inspection rather than an activity-table
// status override.
func TestHistoryDepositCompletesOnChainConfirmation(t *testing.T) {
	t.Parallel()

	h, swap, rpc := newHistoryFixture(t)
	swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{}
	rpc.listTxResp = &daemonrpc.ListTransactionsResponse{
		Transactions: []*daemonrpc.TransactionHistoryEntry{
			{
				Type:               "boarding",
				Subtype:            ledger.EventWalletUTXOCreated,
				ConfirmationStatus: "confirmed",
				AmountSat:          100_000,
				Txid:               "boarding-txid",
				CreatedAtUnixS:     100,
			},
		},
	}

	resp, err := h.List(t.Context(), &walletrpc.ListRequest{})
	require.NoError(t, err)
	require.Len(t, resp.GetActivity().GetEntries(), 1)

	entry := resp.GetActivity().GetEntries()[0]
	require.Equal(
		t, walletrpc.EntryKind_ENTRY_KIND_DEPOSIT, entry.GetKind(),
	)
	require.Equal(
		t, walletrpc.EntryStatus_ENTRY_STATUS_COMPLETE,
		entry.GetStatus(),
	)
	require.Equal(t, "confirmed", entry.GetProgress().GetPhaseLabel())
}
