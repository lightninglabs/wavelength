//go:build walletdkrpc && swapruntime

package swapwallet

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/ledger"
	"github.com/lightninglabs/darepo-client/rpc/swapclientrpc"
	"github.com/lightninglabs/darepo-client/rpc/walletdkrpc"
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

// setReceiveClaimByOutputTxidFixture wires the receive-claim shape where the
// ledger OOR session differs from the materialized claim output txid.
func setReceiveClaimByOutputTxidFixture(t *testing.T, swap *fakeSwapService,
	rpc *fakeRPCServer) {

	t.Helper()

	oorSession := testBytes(32, 0x19)
	claimSession := testBytes(32, 0x29)
	claimHex := testSessionString(t, claimSession)

	swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{
		Swaps: []*swapclientrpc.SwapSummary{
			{
				PaymentHash: "receive-payment",
				Direction: swapclientrpc.
					SwapDirection_SWAP_DIRECTION_RECEIVE,
				State: swapclientrpc.
					SwapState_SWAP_STATE_COMPLETED,
				AmountSat:      5_000,
				UpdatedAtUnix:  200,
				ClaimSessionId: claimHex,
			},
		},
	}
	rpc.listTxResp = &daemonrpc.ListTransactionsResponse{
		Transactions: []*daemonrpc.TransactionHistoryEntry{
			{
				Type:           "oor",
				Subtype:        ledger.EventVTXOSent,
				AmountSat:      5_000,
				DebitAccount:   ledger.AccountTransfersOut,
				CreditAccount:  ledger.AccountVTXOBalance,
				SessionId:      oorSession,
				EntryId:        13,
				CreatedAtUnixS: 100,
			},
			{
				Type:           "oor",
				Subtype:        ledger.EventVTXOReceived,
				AmountSat:      5_000,
				DebitAccount:   ledger.AccountVTXOBalance,
				CreditAccount:  ledger.AccountTransfersIn,
				SessionId:      oorSession,
				Txid:           claimHex,
				OutputIndex:    0,
				EntryId:        14,
				CreatedAtUnixS: 101,
			},
		},
	}
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

	resp, err := h.List(t.Context(), &walletdkrpc.ListRequest{})
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
		t, walletdkrpc.EntryKind_ENTRY_KIND_RECV,
		resp.GetActivity().GetEntries()[0].GetKind(),
	)
	require.Equal(
		t, walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING,
		resp.GetActivity().GetEntries()[0].GetStatus(),
	)
	require.Equal(
		t, walletdkrpc.EntryKind_ENTRY_KIND_EXIT,
		resp.GetActivity().GetEntries()[1].GetKind(),
	)
	require.Equal(
		t, walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE,
		resp.GetActivity().GetEntries()[1].GetStatus(),
	)
	require.Equal(
		t, walletdkrpc.EntryKind_ENTRY_KIND_DEPOSIT,
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

	resp, err := h.List(t.Context(), &walletdkrpc.ListRequest{
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

	resp, err := h.List(t.Context(), &walletdkrpc.ListRequest{})
	require.NoError(t, err)

	entries := resp.GetActivity().GetEntries()
	require.Len(t, entries, 1)
	require.Equal(t, "boarding-unconfirmed", entries[0].GetId())
	require.Equal(
		t, walletdkrpc.EntryKind_ENTRY_KIND_DEPOSIT,
		entries[0].GetKind(),
	)
	require.Equal(
		t, walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING,
		entries[0].GetStatus(),
	)
	require.Equal(t, int64(12345), entries[0].GetAmountSat())
	require.Equal(
		t,
		walletdkrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_WAITING_FOR_CONFIRMATION,
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
				Type:           "oor",
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

	resp, err := h.List(t.Context(), &walletdkrpc.ListRequest{})
	require.NoError(t, err)

	entries := resp.GetActivity().GetEntries()
	require.Len(t, entries, 1)
	require.Equal(t, "payment-hash", entries[0].GetId())
	require.Equal(
		t, walletdkrpc.EntryKind_ENTRY_KIND_RECV, entries[0].GetKind(),
	)
}

// TestHistoryPendingFilterHidesReceiveClaimByOutputTxid confirms --pending
// hides completed receive-swap OOR rows even when the ledger OOR session id is
// different from the materialized claim output txid stored in the swap summary.
func TestHistoryPendingFilterHidesReceiveClaimByOutputTxid(t *testing.T) {
	t.Parallel()

	h, swap, rpc := newHistoryFixture(t)
	setReceiveClaimByOutputTxidFixture(t, swap, rpc)

	resp, err := h.List(t.Context(), &walletdkrpc.ListRequest{
		PendingOnly: true,
	})
	require.NoError(t, err)
	require.Empty(t, resp.GetActivity().GetEntries())
	require.False(
		t, swap.listSwapsLast.GetPendingOnly(),
		"history must fetch all swaps so terminal receive swaps "+
			"can hide their internal OOR ledger legs before "+
			"--pending filters the visible rows",
	)
}

// TestHistoryHidesReceiveClaimByOutputTxid confirms normal activity shows the
// receive swap while hiding its internal OOR rows when the OOR session differs
// from the materialized claim output txid.
func TestHistoryHidesReceiveClaimByOutputTxid(t *testing.T) {
	t.Parallel()

	h, swap, rpc := newHistoryFixture(t)
	setReceiveClaimByOutputTxidFixture(t, swap, rpc)

	resp, err := h.List(t.Context(), &walletdkrpc.ListRequest{})
	require.NoError(t, err)

	entries := resp.GetActivity().GetEntries()
	require.Len(t, entries, 1)
	require.Equal(t, "receive-payment", entries[0].GetId())
	require.Equal(
		t, walletdkrpc.EntryKind_ENTRY_KIND_RECV, entries[0].GetKind(),
	)
	require.Equal(
		t, walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE,
		entries[0].GetStatus(),
	)
	require.Equal(t, int64(5_000), entries[0].GetAmountSat())
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

	resp, err := h.List(t.Context(), &walletdkrpc.ListRequest{})
	require.NoError(t, err)

	entries := resp.GetActivity().GetEntries()
	require.Len(t, entries, 1)
	require.Equal(
		t, walletdkrpc.EntryKind_ENTRY_KIND_SEND, entries[0].GetKind(),
	)
	require.Equal(t, int64(-1_000), entries[0].GetAmountSat())
}

// TestOORSendSessionIDRequiresHashSizedSession confirms malformed OOR session
// IDs are ignored instead of being normalized into correlation keys.
func TestOORSendSessionIDRequiresHashSizedSession(t *testing.T) {
	t.Parallel()

	row := &daemonrpc.TransactionHistoryEntry{
		Type:          "oor",
		Subtype:       ledger.EventVTXOSent,
		AmountSat:     1_000,
		DebitAccount:  ledger.AccountTransfersOut,
		CreditAccount: ledger.AccountVTXOBalance,
		SessionId: []byte{
			1,
			2,
			3,
		},
	}

	_, ok := oorSendSessionID(row)
	require.False(t, ok)

	sessionID := testBytes(chainhash.HashSize, 0x51)
	row.SessionId = sessionID

	session, ok := oorSendSessionID(row)
	require.True(t, ok)
	require.Equal(t, testSessionString(t, sessionID), session)
}

// TestOORReceiveRefRequiresOORType confirms the OOR change correlation path
// only consumes rows already classified as OOR history by the daemon.
func TestOORReceiveRefRequiresOORType(t *testing.T) {
	t.Parallel()

	row := &daemonrpc.TransactionHistoryEntry{
		Type:          "round",
		Subtype:       ledger.EventVTXOReceived,
		AmountSat:     1_000,
		DebitAccount:  ledger.AccountVTXOBalance,
		CreditAccount: ledger.AccountTransfersIn,
		Txid:          strings.Repeat("a", 64),
		OutputIndex:   0,
	}

	_, _, ok := oorReceiveRef(row)
	require.False(t, ok)

	row.Type = "oor"
	_, _, ok = oorReceiveRef(row)
	require.True(t, ok)
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
				Type:               "oor",
				Subtype:            ledger.EventVTXOSent,
				ConfirmationStatus: "recorded",
				AmountSat:          999_745,
				DebitAccount:       ledger.AccountTransfersOut,
				CreditAccount:      ledger.AccountVTXOBalance,
				SessionId:          sessionID,
				EntryId:            13,
				CreatedAtUnixS:     100,
			},
			{
				Type:               "oor",
				Subtype:            ledger.EventVTXOReceived,
				ConfirmationStatus: "recorded",
				AmountSat:          998_511,
				DebitAccount:       ledger.AccountVTXOBalance,
				CreditAccount:      ledger.AccountTransfersIn,
				Txid:               sessionHex,
				OutputIndex:        1,
				EntryId:            14,
				CreatedAtUnixS:     101,
			},
		},
	}

	resp, err := h.List(t.Context(), &walletdkrpc.ListRequest{})
	require.NoError(t, err)

	entries := resp.GetActivity().GetEntries()
	require.Len(t, entries, 1)
	require.Equal(t, "payment-hash", entries[0].GetId())
	require.Equal(
		t, walletdkrpc.EntryKind_ENTRY_KIND_SEND, entries[0].GetKind(),
	)
	require.Equal(t, int64(-1_234), entries[0].GetAmountSat())
}

// TestHistoryHidesPayFundingOORInputWithoutChange confirms wallet activity
// hides a pay-swap funding send even when the selected input exactly matches
// the vHTLC amount and therefore produces no wallet change row.
func TestHistoryHidesPayFundingOORInputWithoutChange(t *testing.T) {
	t.Parallel()

	h, swap, rpc := newHistoryFixture(t)

	sessionID := testBytes(32, 0x22)
	sessionHex := testSessionString(t, sessionID)

	swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{
		Swaps: []*swapclientrpc.SwapSummary{
			{
				PaymentHash: "payment-hash",
				Direction: swapclientrpc.
					SwapDirection_SWAP_DIRECTION_PAY,
				State: swapclientrpc.
					SwapState_SWAP_STATE_REFUNDED,
				AmountSat:        42_000,
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
				AmountSat:      42_000,
				DebitAccount:   ledger.AccountTransfersOut,
				CreditAccount:  ledger.AccountVTXOBalance,
				SessionId:      sessionID,
				EntryId:        15,
				CreatedAtUnixS: 100,
			},
		},
	}

	resp, err := h.List(t.Context(), &walletdkrpc.ListRequest{})
	require.NoError(t, err)

	entries := resp.GetActivity().GetEntries()
	require.Len(t, entries, 1)
	require.Equal(t, "payment-hash", entries[0].GetId())
	require.Equal(
		t, walletdkrpc.EntryKind_ENTRY_KIND_SEND, entries[0].GetKind(),
	)
	require.Equal(t, int64(-42_000), entries[0].GetAmountSat())
	require.Equal(
		t, walletdkrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_REFUNDED,
		entries[0].GetProgress().GetPhase(),
	)
}

// TestHistoryHidesPayRefundOORSession confirms the cooperative refund OOR
// created for a failed pay swap stays internal to the swap row. The wallet
// activity entry should be the refunded payment hash, not a synthetic
// ledger-N SEND for the refund self-transfer.
func TestHistoryHidesPayRefundOORSession(t *testing.T) {
	t.Parallel()

	h, swap, rpc := newHistoryFixture(t)

	refundSession := testBytes(32, 0x61)
	refundHex := testSessionString(t, refundSession)

	swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{
		Swaps: []*swapclientrpc.SwapSummary{
			{
				PaymentHash: "payment-hash",
				Direction: swapclientrpc.
					SwapDirection_SWAP_DIRECTION_PAY,
				State: swapclientrpc.
					SwapState_SWAP_STATE_REFUNDED,
				AmountSat:       1_000,
				UpdatedAtUnix:   200,
				RefundSessionId: refundHex,
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
				SessionId:      refundSession,
				EntryId:        17,
				CreatedAtUnixS: 100,
			},
			{
				Type:           "oor",
				Subtype:        ledger.EventVTXOReceived,
				AmountSat:      1_000,
				DebitAccount:   ledger.AccountVTXOBalance,
				CreditAccount:  ledger.AccountTransfersIn,
				Txid:           refundHex,
				OutputIndex:    0,
				EntryId:        18,
				CreatedAtUnixS: 101,
			},
		},
	}

	resp, err := h.List(t.Context(), &walletdkrpc.ListRequest{})
	require.NoError(t, err)

	entries := resp.GetActivity().GetEntries()
	require.Len(t, entries, 1)
	require.Equal(t, "payment-hash", entries[0].GetId())
	require.Equal(
		t, walletdkrpc.EntryKind_ENTRY_KIND_SEND, entries[0].GetKind(),
	)
	require.Equal(
		t, walletdkrpc.EntryStatus_ENTRY_STATUS_FAILED,
		entries[0].GetStatus(),
	)
	require.Equal(
		t, walletdkrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_REFUNDED,
		entries[0].GetProgress().GetPhase(),
	)
}

// TestHistoryPendingFilterKeepsTerminalSwapCorrelations confirms --pending
// still uses terminal swap metadata to hide internal OOR funding and refund
// ledger legs before filtering terminal swap rows out of the response.
func TestHistoryPendingFilterKeepsTerminalSwapCorrelations(t *testing.T) {
	t.Parallel()

	h, swap, rpc := newHistoryFixture(t)

	fundingSession := testBytes(32, 0x71)
	fundingHex := testSessionString(t, fundingSession)
	refundSession := testBytes(32, 0x81)
	refundHex := testSessionString(t, refundSession)

	swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{
		Swaps: []*swapclientrpc.SwapSummary{
			{
				PaymentHash: "completed-payment",
				Direction: swapclientrpc.
					SwapDirection_SWAP_DIRECTION_PAY,
				State: swapclientrpc.
					SwapState_SWAP_STATE_COMPLETED,
				AmountSat:        1_000,
				UpdatedAtUnix:    200,
				FundingSessionId: fundingHex,
			},
			{
				PaymentHash: "refunded-payment",
				Direction: swapclientrpc.
					SwapDirection_SWAP_DIRECTION_PAY,
				State: swapclientrpc.
					SwapState_SWAP_STATE_REFUNDED,
				AmountSat:       1_000,
				UpdatedAtUnix:   300,
				RefundSessionId: refundHex,
			},
		},
	}
	rpc.listTxResp = &daemonrpc.ListTransactionsResponse{
		Transactions: []*daemonrpc.TransactionHistoryEntry{
			{
				Type:           "oor",
				Subtype:        ledger.EventVTXOSent,
				AmountSat:      9_000,
				DebitAccount:   ledger.AccountTransfersOut,
				CreditAccount:  ledger.AccountVTXOBalance,
				SessionId:      fundingSession,
				EntryId:        21,
				CreatedAtUnixS: 100,
			},
			{
				Type:           "oor",
				Subtype:        ledger.EventVTXOReceived,
				AmountSat:      8_000,
				DebitAccount:   ledger.AccountVTXOBalance,
				CreditAccount:  ledger.AccountTransfersIn,
				Txid:           fundingHex,
				OutputIndex:    1,
				EntryId:        22,
				CreatedAtUnixS: 101,
			},
			{
				Type:           "oor",
				Subtype:        ledger.EventVTXOSent,
				AmountSat:      1_000,
				DebitAccount:   ledger.AccountTransfersOut,
				CreditAccount:  ledger.AccountVTXOBalance,
				SessionId:      refundSession,
				EntryId:        23,
				CreatedAtUnixS: 102,
			},
			{
				Type:           "oor",
				Subtype:        ledger.EventVTXOReceived,
				AmountSat:      1_000,
				DebitAccount:   ledger.AccountVTXOBalance,
				CreditAccount:  ledger.AccountTransfersIn,
				Txid:           refundHex,
				OutputIndex:    0,
				EntryId:        24,
				CreatedAtUnixS: 103,
			},
		},
	}

	resp, err := h.List(t.Context(), &walletdkrpc.ListRequest{
		PendingOnly: true,
	})
	require.NoError(t, err)
	require.Empty(t, resp.GetActivity().GetEntries())
	require.False(
		t, swap.listSwapsLast.GetPendingOnly(),
		"history must fetch all swaps so terminal rows can hide "+
			"their internal OOR ledger legs before --pending "+
			"filters the visible rows",
	)
}

// TestHistoryKeepsSameAmountUnmatchedFundingInput confirms pay-swap funding
// legs are hidden by funding session, not by amount alone. The unrelated raw
// OOR send remains visible with its net external-send amount.
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
				Type:          "oor",
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
				Type:          "oor",
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

	resp, err := h.List(t.Context(), &walletdkrpc.ListRequest{})
	require.NoError(t, err)

	entries := resp.GetActivity().GetEntries()
	require.Len(t, entries, 2)
	require.Equal(t, "payment-hash", entries[0].GetId())
	require.Equal(t, "ledger-15", entries[1].GetId())
	require.Equal(t, int64(-1_234), entries[1].GetAmountSat())
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
				Type:               "oor",
				Subtype:            ledger.EventVTXOSent,
				ConfirmationStatus: "recorded",
				AmountSat:          999_745,
				DebitAccount:       ledger.AccountTransfersOut,
				CreditAccount:      ledger.AccountVTXOBalance,
				SessionId:          sessionID,
				EntryId:            13,
				CreatedAtUnixS:     100,
			},
			{
				Type:               "oor",
				Subtype:            ledger.EventVTXOReceived,
				ConfirmationStatus: "recorded",
				AmountSat:          998_511,
				DebitAccount:       ledger.AccountVTXOBalance,
				CreditAccount:      ledger.AccountTransfersIn,
				Txid:               sessionHex,
				OutputIndex:        1,
				EntryId:            14,
				CreatedAtUnixS:     101,
			},
		},
	}

	resp, err := h.List(t.Context(), &walletdkrpc.ListRequest{})
	require.NoError(t, err)

	entries := resp.GetActivity().GetEntries()
	require.Len(t, entries, 1)
	require.Equal(
		t, walletdkrpc.EntryKind_ENTRY_KIND_SEND, entries[0].GetKind(),
	)
	require.Equal(
		t, walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE,
		entries[0].GetStatus(),
	)
	require.Equal(t, int64(-1_234), entries[0].GetAmountSat())
}

// TestHistoryKeepsZeroDeltaOORSendWithoutSwap covers a balanced OOR session
// that sends funds out and receives the same amount back to this wallet without
// any swap summary referencing the session. This shape is expected for ordinary
// wallet-local OOR activity and must not be mistaken for a swap refund or claim
// just because the net delta is zero. The history merger may hide the paired
// receive leg as same-session change, but it must keep the outgoing SEND row so
// user-initiated OOR activity remains visible in the wallet activity stream.
func TestHistoryKeepsZeroDeltaOORSendWithoutSwap(t *testing.T) {
	t.Parallel()

	h, swap, rpc := newHistoryFixture(t)

	sessionID := testBytes(32, 0x35)
	sessionHex := testSessionString(t, sessionID)

	swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{}
	rpc.listTxResp = &daemonrpc.ListTransactionsResponse{
		Transactions: []*daemonrpc.TransactionHistoryEntry{
			{
				Type:               "oor",
				Subtype:            ledger.EventVTXOSent,
				ConfirmationStatus: "recorded",
				AmountSat:          1_000,
				DebitAccount:       ledger.AccountTransfersOut,
				CreditAccount:      ledger.AccountVTXOBalance,
				SessionId:          sessionID,
				EntryId:            31,
				CreatedAtUnixS:     100,
			},
			{
				Type:               "oor",
				Subtype:            ledger.EventVTXOReceived,
				ConfirmationStatus: "recorded",
				AmountSat:          1_000,
				DebitAccount:       ledger.AccountVTXOBalance,
				CreditAccount:      ledger.AccountTransfersIn,
				Txid:               sessionHex,
				OutputIndex:        0,
				EntryId:            32,
				CreatedAtUnixS:     101,
			},
		},
	}

	resp, err := h.List(t.Context(), &walletdkrpc.ListRequest{})
	require.NoError(t, err)

	entries := resp.GetActivity().GetEntries()
	require.Len(t, entries, 1)
	require.Equal(t, "ledger-31", entries[0].GetId())
	require.Equal(
		t, walletdkrpc.EntryKind_ENTRY_KIND_SEND, entries[0].GetKind(),
	)
	require.Equal(
		t, walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE,
		entries[0].GetStatus(),
	)
	require.Equal(t, int64(-1_000), entries[0].GetAmountSat())
}

// TestHistoryCompletesRecordedOORReceive confirms materialized OOR receive
// rows are terminal in the user-facing wallet activity stream. The ledger uses
// "recorded" for durable local accounting rows, but an OOR receive row only
// appears after the VTXO is live in the wallet.
func TestHistoryCompletesRecordedOORReceive(t *testing.T) {
	t.Parallel()

	h, swap, rpc := newHistoryFixture(t)
	swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{}
	rpc.listTxResp = &daemonrpc.ListTransactionsResponse{
		Transactions: []*daemonrpc.TransactionHistoryEntry{
			{
				Type:               "oor",
				Subtype:            ledger.EventVTXOReceived,
				ConfirmationStatus: "recorded",
				AmountSat:          7_000,
				Txid:               "oor-recv-txid",
				CreatedAtUnixS:     100,
				DebitAccount:       ledger.AccountVTXOBalance,
				CreditAccount:      ledger.AccountTransfersIn,
			},
		},
	}

	resp, err := h.List(t.Context(), &walletdkrpc.ListRequest{})
	require.NoError(t, err)

	entries := resp.GetActivity().GetEntries()
	require.Len(t, entries, 1)
	require.Equal(
		t, walletdkrpc.EntryKind_ENTRY_KIND_RECV, entries[0].GetKind(),
	)
	require.Equal(
		t, walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE,
		entries[0].GetStatus(),
	)
	require.Equal(t, int64(7_000), entries[0].GetAmountSat())
	require.Equal(
		t, walletdkrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_CONFIRMED,
		entries[0].GetProgress().GetPhase(),
	)
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

	resp, err := h.List(t.Context(), &walletdkrpc.ListRequest{
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

	resp, err := h.List(t.Context(), &walletdkrpc.ListRequest{
		Kinds: []walletdkrpc.EntryKind{
			walletdkrpc.EntryKind_ENTRY_KIND_DEPOSIT,
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
	_, err := h.List(t.Context(), &walletdkrpc.ListRequest{
		Kinds: []walletdkrpc.EntryKind{
			walletdkrpc.EntryKind_ENTRY_KIND_UNSPECIFIED,
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
		status: walletdkrpc.
			EntryStatus_ENTRY_STATUS_FAILED,
		failureReason: "timed_out",
		failureCode:   timedOutCode,
	}
	h.runtime.pendingMu.Unlock()

	resp, err := h.List(t.Context(), &walletdkrpc.ListRequest{})
	require.NoError(t, err)
	require.Len(t, resp.GetActivity().GetEntries(), 1)
	require.Equal(
		t, walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING,
		resp.GetActivity().GetEntries()[0].GetStatus(),
	)
	require.Equal(
		t, walletdkrpc.EntryKind_ENTRY_KIND_UNSPECIFIED,
		resp.GetActivity().GetEntries()[0].GetKind(),
	)
	require.Empty(t, resp.GetActivity().GetEntries()[0].GetFailureReason())
	require.Equal(
		t, unspecCode,
		resp.GetActivity().GetEntries()[0].GetFailureCode(),
	)
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
		status: walletdkrpc.
			EntryStatus_ENTRY_STATUS_FAILED,
		failureReason: "timed_out",
		failureCode:   timedOutCode,
	}
	h.runtime.pendingMu.Unlock()

	resp, err := h.List(t.Context(), &walletdkrpc.ListRequest{})
	require.NoError(t, err)
	require.Len(t, resp.GetActivity().GetEntries(), 1)
	require.Equal(
		t, walletdkrpc.EntryStatus_ENTRY_STATUS_FAILED,
		resp.GetActivity().GetEntries()[0].GetStatus(),
	)
	require.Equal(
		t, "timed_out",
		resp.GetActivity().GetEntries()[0].GetFailureReason(),
	)
	require.Equal(
		t, timedOutCode,
		resp.GetActivity().GetEntries()[0].GetFailureCode(),
	)
}

// TestHistoryIncludesWalletLocalPendingExit confirms a cooperative leave row
// returned by Send remains visible in activity before any terminal ledger row
// exists. The regression in issue #612 was that Send returned an EXIT entry,
// but ListActivity only read swap/ledger sources and therefore dropped the
// wallet-local pending row immediately.
func TestHistoryIncludesWalletLocalPendingExit(t *testing.T) {
	t.Parallel()

	h, swap, rpc := newHistoryFixture(t)
	swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{}
	rpc.listTxResp = &daemonrpc.ListTransactionsResponse{}
	rpc.unrollStatusResp = &daemonrpc.GetUnrollStatusResponse{
		Found: false,
	}

	pending := leaveEntryStub(
		"", []string{
			"leave-outpoint:1",
		}, "bcrt1qdest",
		50_000, "user note",
	)
	h.runtime.trackPendingEntry(pending)

	resp, err := h.List(t.Context(), &walletdkrpc.ListRequest{})
	require.NoError(t, err)

	entries := resp.GetActivity().GetEntries()
	require.Len(t, entries, 1)
	require.Equal(t, "leave-outpoint:1", entries[0].GetId())
	require.Equal(
		t, walletdkrpc.EntryKind_ENTRY_KIND_EXIT, entries[0].GetKind(),
	)
	require.Equal(
		t, walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING,
		entries[0].GetStatus(),
	)
	require.Equal(t, int64(-50_000), entries[0].GetAmountSat())
	require.Equal(t, "user note", entries[0].GetNote())
}

// TestHistoryCompletesWalletLocalExitWhenVTXOForfeited confirms a
// cooperative-leave activity row stops showing as pending once the source VTXO
// has been terminally forfeited in the confirmed leave round. This is the
// issue #568 balance/activity edge: the VTXO state is authoritative for the
// in-process pending row even though v1 does not yet persist a durable leave
// job that links the original entry id to the commitment transaction.
func TestHistoryCompletesWalletLocalExitWhenVTXOForfeited(t *testing.T) {
	t.Parallel()

	h, swap, rpc := newHistoryFixture(t)
	swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{}
	rpc.listTxResp = &daemonrpc.ListTransactionsResponse{}
	rpc.unrollStatusResp = &daemonrpc.GetUnrollStatusResponse{
		Found: false,
	}
	rpc.listVTXOsByStatus = map[daemonrpc.VTXOStatus]*daemonrpc.
		ListVTXOsResponse{

		daemonrpc.VTXOStatus_VTXO_STATUS_UNILATERAL_EXIT: {},
		daemonrpc.VTXOStatus_VTXO_STATUS_FORFEITED: {
			Vtxos: []*daemonrpc.VTXO{
				{
					Outpoint: "leave-outpoint:1",
					Status: daemonrpc.
						VTXOStatus_VTXO_STATUS_FORFEITED,
				},
			},
		},
	}

	pending := leaveEntryStub(
		"", []string{
			"leave-outpoint:1",
		}, "bcrt1qdest",
		50_000, "user note",
	)
	h.runtime.trackPendingEntryWithoutTimeout(pending)

	resp, err := h.List(t.Context(), &walletdkrpc.ListRequest{})
	require.NoError(t, err)

	entries := resp.GetActivity().GetEntries()
	require.Len(t, entries, 1)
	require.Equal(t, "leave-outpoint:1", entries[0].GetId())
	require.Equal(
		t, walletdkrpc.EntryKind_ENTRY_KIND_EXIT, entries[0].GetKind(),
	)
	require.Equal(
		t, walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE,
		entries[0].GetStatus(),
	)
	require.Equal(
		t, walletdkrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_CONFIRMED,
		entries[0].GetProgress().GetPhase(),
	)
	require.Equal(t, "confirmed", entries[0].GetProgress().GetPhaseLabel())
	require.Equal(
		t, "leave-outpoint:1",
		entries[0].GetProgress().GetVtxoOutpoint(),
	)
	require.Equal(t, int64(-50_000), entries[0].GetAmountSat())
	require.Equal(t, "user note", entries[0].GetNote())

	pendingResp, err := h.List(t.Context(), &walletdkrpc.ListRequest{
		PendingOnly: true,
	})
	require.NoError(t, err)
	require.Empty(t, pendingResp.GetActivity().GetEntries())
}

// TestHistoryKeepsWalletLocalExitPendingForUnmatchedForfeitedVTXO confirms the
// cooperative-leave fallback only completes the wallet-local row when the
// forfeited VTXO is the same source outpoint tracked by the row. This protects
// the v1 heuristic from treating any terminal VTXO as proof that this specific
// leave completed.
func TestHistoryKeepsWalletLocalExitPendingForUnmatchedForfeitedVTXO(
	t *testing.T) {

	t.Parallel()

	h, swap, rpc := newHistoryFixture(t)
	swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{}
	rpc.listTxResp = &daemonrpc.ListTransactionsResponse{}
	rpc.unrollStatusResp = &daemonrpc.GetUnrollStatusResponse{
		Found: false,
	}
	rpc.listVTXOsByStatus = map[daemonrpc.VTXOStatus]*daemonrpc.
		ListVTXOsResponse{

		daemonrpc.VTXOStatus_VTXO_STATUS_UNILATERAL_EXIT: {},
		daemonrpc.VTXOStatus_VTXO_STATUS_FORFEITED: {
			Vtxos: []*daemonrpc.VTXO{
				{
					Outpoint: "other-leave-outpoint:0",
					Status: daemonrpc.
						VTXOStatus_VTXO_STATUS_FORFEITED,
				},
			},
		},
	}

	pending := leaveEntryStub(
		"", []string{
			"leave-outpoint:1",
		}, "bcrt1qdest",
		50_000, "user note",
	)
	h.runtime.trackPendingEntryWithoutTimeout(pending)

	resp, err := h.List(t.Context(), &walletdkrpc.ListRequest{})
	require.NoError(t, err)

	entries := resp.GetActivity().GetEntries()
	require.Len(t, entries, 1)
	require.Equal(t, "leave-outpoint:1", entries[0].GetId())
	require.Equal(
		t, walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING,
		entries[0].GetStatus(),
	)
	require.Equal(
		t,
		walletdkrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_REQUEST_CREATED,
		entries[0].GetProgress().GetPhase(),
	)
	require.Equal(
		t, "request_created", entries[0].GetProgress().GetPhaseLabel(),
	)
}

// TestHistoryScansForfeitedVTXOsOnceForWalletLocalExits confirms activity list
// batches the cooperative-leave terminal lookup. A wallet process can retain
// multiple runtime-local EXIT rows until restart; history should not scan the
// full forfeited VTXO set once per row.
func TestHistoryScansForfeitedVTXOsOnceForWalletLocalExits(t *testing.T) {
	t.Parallel()

	h, swap, rpc := newHistoryFixture(t)
	swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{}
	rpc.listTxResp = &daemonrpc.ListTransactionsResponse{}
	rpc.unrollStatusResp = &daemonrpc.GetUnrollStatusResponse{
		Found: false,
	}
	rpc.listVTXOsByStatus = map[daemonrpc.VTXOStatus]*daemonrpc.
		ListVTXOsResponse{

		daemonrpc.VTXOStatus_VTXO_STATUS_UNILATERAL_EXIT: {},
		daemonrpc.VTXOStatus_VTXO_STATUS_FORFEITED: {
			Vtxos: []*daemonrpc.VTXO{
				{
					Outpoint: "leave-outpoint:1",
					Status: daemonrpc.
						VTXOStatus_VTXO_STATUS_FORFEITED,
				},
			},
		},
	}

	h.runtime.trackPendingEntryWithoutTimeout(
		leaveEntryStub(
			"", []string{
				"leave-outpoint:1",
			}, "bcrt1qdest",
			50_000, "first note",
		),
	)
	h.runtime.trackPendingEntryWithoutTimeout(
		leaveEntryStub(
			"", []string{
				"other-leave-outpoint:0",
			}, "bcrt1qdest",
			25_000, "second note",
		),
	)

	resp, err := h.List(t.Context(), &walletdkrpc.ListRequest{})
	require.NoError(t, err)

	byID := make(map[string]*walletdkrpc.WalletEntry)
	for _, entry := range resp.GetActivity().GetEntries() {
		byID[entry.GetId()] = entry
	}
	require.Equal(
		t, walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE,
		byID["leave-outpoint:1"].GetStatus(),
	)
	require.Equal(
		t, walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING,
		byID["other-leave-outpoint:0"].GetStatus(),
	)
	require.Equal(t, 2, rpc.listVTXOsCalls)
	require.Equal(
		t, daemonrpc.VTXOStatus_VTXO_STATUS_FORFEITED,
		rpc.listVTXOsLastReq.GetStatusFilter(),
	)
}

// TestHistoryKeepsCSVPendingUnilateralExitPendingAfterDeadline confirms the
// wallet-local timeout overlay cannot clobber the unroll subsystem's
// authoritative non-terminal status. Unilateral exits normally wait through a
// CSV delay that exceeds the generic wallet deadline, so a CSV_PENDING job must
// remain PENDING instead of being projected to FAILED.
func TestHistoryKeepsCSVPendingUnilateralExitPendingAfterDeadline(
	t *testing.T) {

	t.Parallel()

	h, swap, rpc := newHistoryFixture(t)
	swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{}
	rpc.listTxResp = &daemonrpc.ListTransactionsResponse{}
	rpc.listVTXOsResp = &daemonrpc.ListVTXOsResponse{
		Vtxos: []*daemonrpc.VTXO{
			{
				Outpoint:  "exit-outpoint:0",
				AmountSat: 7_000,
				Status: daemonrpc.
					VTXOStatus_VTXO_STATUS_UNILATERAL_EXIT,
			},
		},
	}
	rpc.unrollStatusResp = &daemonrpc.GetUnrollStatusResponse{
		Found:  true,
		Status: daemonrpc.UnrollJobStatus_UNROLL_JOB_STATUS_CSV_PENDING,
	}

	pending := unilateralExitEntryStub("exit-outpoint:0")
	pending.CreatedAtUnix = 123
	pending.UpdatedAtUnix = 124
	h.runtime.trackPendingEntryWithoutTimeout(pending)
	h.runtime.applyDeadlines(time.Now().Add(2 * defaultWalletDeadline))

	resp, err := h.List(t.Context(), &walletdkrpc.ListRequest{})
	require.NoError(t, err)

	entries := resp.GetActivity().GetEntries()
	require.Len(t, entries, 1)
	require.Equal(t, "exit-outpoint:0", entries[0].GetId())
	require.Equal(
		t, walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING,
		entries[0].GetStatus(),
	)
	require.Equal(
		t, "csv_pending", entries[0].GetProgress().GetPhaseLabel(),
	)
	require.Equal(t, int64(123), entries[0].GetCreatedAtUnix())
	require.Equal(t, int64(124), entries[0].GetUpdatedAtUnix())
	require.Empty(t, entries[0].GetFailureReason())
}

// TestHistoryExitKindFilterGatesWalletLocalPendingRows confirms filtering out
// EXIT also skips wallet-local pending rows before they issue per-row
// ExitStatus lookups. The filterEntries pass would drop them eventually, but
// gating at collection avoids useless unroll-status RPCs for other views.
func TestHistoryExitKindFilterGatesWalletLocalPendingRows(t *testing.T) {
	t.Parallel()

	h, swap, rpc := newHistoryFixture(t)
	swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{}
	rpc.listTxResp = &daemonrpc.ListTransactionsResponse{}

	h.runtime.trackPendingEntryWithoutTimeout(
		leaveEntryStub(
			"", []string{
				"leave-outpoint:1",
			}, "bcrt1qdest",
			50_000, "",
		),
	)

	resp, err := h.List(t.Context(), &walletdkrpc.ListRequest{
		Kinds: []walletdkrpc.EntryKind{
			walletdkrpc.EntryKind_ENTRY_KIND_DEPOSIT,
		},
	})
	require.NoError(t, err)
	require.Empty(t, resp.GetActivity().GetEntries())
	require.Zero(t, rpc.unrollStatusCalls)
	require.Zero(t, rpc.listVTXOsCalls)
}

// TestHistoryIncludesUnilateralExitVTXO confirms a VTXO that has already been
// handed to the unroll subsystem appears as EXIT activity even after the
// original Exit RPC response is gone. The row is decorated from ExitStatus so
// terminal unroll failures surface as FAILED rather than staying invisible or
// permanently pending.
func TestHistoryIncludesUnilateralExitVTXO(t *testing.T) {
	t.Parallel()

	h, swap, rpc := newHistoryFixture(t)
	swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{}
	rpc.listTxResp = &daemonrpc.ListTransactionsResponse{}
	rpc.listVTXOsResp = &daemonrpc.ListVTXOsResponse{
		Vtxos: []*daemonrpc.VTXO{
			{
				Outpoint:  "exit-outpoint:0",
				AmountSat: 7_000,
				Status: daemonrpc.
					VTXOStatus_VTXO_STATUS_UNILATERAL_EXIT,
			},
		},
	}
	rpc.unrollStatusResp = &daemonrpc.GetUnrollStatusResponse{
		Found:     true,
		Status:    daemonrpc.UnrollJobStatus_UNROLL_JOB_STATUS_FAILED,
		LastError: "broadcast failed",
	}

	resp, err := h.List(t.Context(), &walletdkrpc.ListRequest{})
	require.NoError(t, err)

	entries := resp.GetActivity().GetEntries()
	require.Len(t, entries, 1)
	require.Equal(t, "exit-outpoint:0", entries[0].GetId())
	require.Equal(
		t, walletdkrpc.EntryKind_ENTRY_KIND_EXIT, entries[0].GetKind(),
	)
	require.Equal(
		t, walletdkrpc.EntryStatus_ENTRY_STATUS_FAILED,
		entries[0].GetStatus(),
	)
	require.Equal(t, int64(-7_000), entries[0].GetAmountSat())
	require.Equal(t, "broadcast failed", entries[0].GetFailureReason())
	require.Equal(
		t, walletdkrpc.EntryFailureCode_ENTRY_FAILURE_CODE_FAILED,
		entries[0].GetFailureCode(),
	)
	require.Equal(
		t, daemonrpc.VTXOStatus_VTXO_STATUS_UNILATERAL_EXIT,
		rpc.listVTXOsLastReq.GetStatusFilter(),
	)
	require.Equal(t, "exit-outpoint:0", rpc.unrollStatusLast.GetOutpoint())
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

	resp, err := h.List(t.Context(), &walletdkrpc.ListRequest{})
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

	resp, err := h.List(t.Context(), &walletdkrpc.ListRequest{
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

	resp, err := h.List(t.Context(), &walletdkrpc.ListRequest{})
	require.NoError(t, err)
	require.Len(
		t, resp.GetActivity().GetEntries(), 2, "OOR send + OOR "+
			"recv must surface; bookkeeping row stays hidden",
	)

	// Sort is updated_at desc: send(200) before recv(100).
	send := resp.GetActivity().GetEntries()[0]
	require.Equal(t, "oor-send-txid", send.GetId())
	require.Equal(
		t, walletdkrpc.EntryKind_ENTRY_KIND_SEND, send.GetKind(),
	)
	require.Equal(
		t, int64(-3_000), send.GetAmountSat(),
		"SEND amount must be negative",
	)

	recv := resp.GetActivity().GetEntries()[1]
	require.Equal(t, "oor-recv-txid", recv.GetId())
	require.Equal(
		t, walletdkrpc.EntryKind_ENTRY_KIND_RECV, recv.GetKind(),
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

	resp, err := h.List(t.Context(), &walletdkrpc.ListRequest{})
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

// TestHistoryKeepsSameTransactionBoardingOutputs confirms duplicate boarding
// deposits from the same Bitcoin transaction are keyed by outpoint before
// activity de-duplication. A funding transaction can pay the same boarding
// address in multiple outputs; all wallet-owned UTXOs must remain visible so
// activity totals match the raw transaction history.
func TestHistoryKeepsSameTransactionBoardingOutputs(t *testing.T) {
	t.Parallel()

	h, swap, rpc := newHistoryFixture(t)
	swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{}

	const (
		sharedTxid = "18983d5d6f9c2dbf5166174777162f97797bed3e59d5f63b80a58833ea5391a0"
		otherTxid  = "c4411b5d45230b71335dad7f5a20772985facf9634280dc263063b1f9b87f034"
	)
	rpc.listTxResp = &daemonrpc.ListTransactionsResponse{
		Transactions: []*daemonrpc.TransactionHistoryEntry{
			{
				Type:               "boarding",
				Subtype:            ledger.EventWalletUTXOCreated,
				ConfirmationStatus: "confirmed",
				AmountSat:          990_064,
				Txid:               sharedTxid,
				OutputIndex:        127,
				EntryId:            1,
				CreatedAtUnixS:     300,
			},
			{
				Type:               "boarding",
				Subtype:            ledger.EventWalletUTXOCreated,
				ConfirmationStatus: "confirmed",
				AmountSat:          990_064,
				Txid:               sharedTxid,
				OutputIndex:        23,
				EntryId:            2,
				CreatedAtUnixS:     200,
			},
			{
				Type:               "boarding",
				Subtype:            ledger.EventWalletUTXOCreated,
				ConfirmationStatus: "confirmed",
				AmountSat:          283_605,
				Txid:               otherTxid,
				OutputIndex:        0,
				EntryId:            3,
				CreatedAtUnixS:     100,
			},
		},
	}

	resp, err := h.List(t.Context(), &walletdkrpc.ListRequest{})
	require.NoError(t, err)

	entries := resp.GetActivity().GetEntries()
	require.Len(t, entries, 3)

	amountsByID := make(map[string]int64, len(entries))
	var total int64
	for _, entry := range entries {
		amountsByID[entry.GetId()] = entry.GetAmountSat()
		total += entry.GetAmountSat()
	}

	require.Equal(t, int64(2_263_733), total)
	require.Equal(t, int64(990_064), amountsByID[sharedTxid+":127"])
	require.Equal(t, int64(990_064), amountsByID[sharedTxid+":23"])
	require.Equal(t, int64(283_605), amountsByID[otherTxid+":0"])
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

	resp, err := h.List(t.Context(), &walletdkrpc.ListRequest{
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

// TestHistoryDepositBoardsBeforeCompletion confirms that a chain-confirmed
// DEPOSIT row stays pending while the ledger reports the intermediate boarding
// state. The user has seen the on-chain deposit, but it is not spendable until
// the boarding round confirms and creates a live VTXO.
func TestHistoryDepositBoardsBeforeCompletion(t *testing.T) {
	t.Parallel()

	h, swap, rpc := newHistoryFixture(t)
	swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{}
	rpc.listTxResp = &daemonrpc.ListTransactionsResponse{
		Transactions: []*daemonrpc.TransactionHistoryEntry{
			{
				Type:               "boarding",
				Subtype:            ledger.EventWalletUTXOCreated,
				ConfirmationStatus: "boarding",
				AmountSat:          100_000,
				Txid:               "boarding-txid",
				CreatedAtUnixS:     100,
			},
		},
	}

	resp, err := h.List(t.Context(), &walletdkrpc.ListRequest{})
	require.NoError(t, err)
	require.Len(t, resp.GetActivity().GetEntries(), 1)

	entry := resp.GetActivity().GetEntries()[0]
	require.Equal(
		t, walletdkrpc.EntryKind_ENTRY_KIND_DEPOSIT, entry.GetKind(),
	)
	require.Equal(
		t, walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING,
		entry.GetStatus(),
	)
	require.Equal(t, "boarding", entry.GetProgress().GetPhaseLabel())
}

// TestHistoryDepositCompletesAfterBoarding confirms that a DEPOSIT row only
// appears complete once the ledger reports that the boarding flow reached the
// confirmed round state.
func TestHistoryDepositCompletesAfterBoarding(t *testing.T) {
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

	resp, err := h.List(t.Context(), &walletdkrpc.ListRequest{})
	require.NoError(t, err)
	require.Len(t, resp.GetActivity().GetEntries(), 1)

	entry := resp.GetActivity().GetEntries()[0]
	require.Equal(
		t, walletdkrpc.EntryKind_ENTRY_KIND_DEPOSIT, entry.GetKind(),
	)
	require.Equal(
		t, walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE,
		entry.GetStatus(),
	)
	require.Equal(t, "confirmed", entry.GetProgress().GetPhaseLabel())
}

// TestLedgerActivityIDDeposit verifies confirmed-deposit id keying: the
// address-scoped id when the daemon surfaces boarding_address, falling back to
// txid:vout (then bare txid) for older daemons, only for the
// wallet_utxo_created subtype.
func TestLedgerActivityIDDeposit(t *testing.T) {
	t.Parallel()

	dep := walletdkrpc.EntryKind_ENTRY_KIND_DEPOSIT
	tests := []struct {
		name  string
		entry *daemonrpc.TransactionHistoryEntry
		want  string
	}{
		{
			name: "boarding_address present keys by address",
			entry: &daemonrpc.TransactionHistoryEntry{
				Txid:            "boardingtx",
				Subtype:         ledger.EventWalletUTXOCreated,
				OutputIndex:     1,
				BoardingAddress: "bcrt1qaddr",
			},
			want: "deposit-bcrt1qaddr",
		},
		{
			name: "older daemon without address falls back to txid:vout",
			entry: &daemonrpc.TransactionHistoryEntry{
				Txid:        "boardingtx",
				Subtype:     ledger.EventWalletUTXOCreated,
				OutputIndex: 1,
			},
			want: "boardingtx:1",
		},
		{
			name: "no address and no output index falls back to txid",
			entry: &daemonrpc.TransactionHistoryEntry{
				Txid:        "boardingtx",
				Subtype:     ledger.EventWalletUTXOCreated,
				OutputIndex: -1,
			},
			want: "boardingtx",
		},
		{
			name: "non-deposit subtype ignores boarding_address",
			entry: &daemonrpc.TransactionHistoryEntry{
				Txid:            "boardingtx",
				Subtype:         "boarding_fee_paid",
				BoardingAddress: "bcrt1qaddr",
			},
			want: "boardingtx",
		},
		{
			name:  "empty txid returns empty",
			entry: &daemonrpc.TransactionHistoryEntry{},
			want:  "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(
				t, tc.want, ledgerActivityID(tc.entry, dep),
			)
		})
	}
}

// TestHistoryDepositKeyedByBoardingAddress verifies a confirmed boarding
// deposit surfaced through List is keyed by the shared deposit-<address> id
// when the daemon populates boarding_address, so it matches the pending row
// Deposit projected under the same id.
func TestHistoryDepositKeyedByBoardingAddress(t *testing.T) {
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
				OutputIndex:        0,
				BoardingAddress:    "bcrt1qboardingaddr",
				CreatedAtUnixS:     100,
			},
		},
	}

	resp, err := h.List(t.Context(), &walletdkrpc.ListRequest{})
	require.NoError(t, err)
	require.Len(t, resp.GetActivity().GetEntries(), 1)
	entry := resp.GetActivity().GetEntries()[0]
	require.Equal(t, "deposit-bcrt1qboardingaddr", entry.GetId())
	require.Equal(
		t, "bcrt1qboardingaddr",
		entry.GetRequest().GetOnchainAddress().GetAddress(),
		"confirmed deposit must carry its address as a structured "+
			"request",
	)
}

// TestHistoryDepositsSameAddressSummed verifies two confirmed boarding UTXOs
// paid to the same address collapse into one activity row whose amount is the
// SUM, so a reused boarding address never hides funds in the feed.
func TestHistoryDepositsSameAddressSummed(t *testing.T) {
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
				Txid:               "txid-a",
				OutputIndex:        0,
				BoardingAddress:    "bcrt1qshared",
				CreatedAtUnixS:     100,
			},
			{
				Type:               "boarding",
				Subtype:            ledger.EventWalletUTXOCreated,
				ConfirmationStatus: "confirmed",
				AmountSat:          250_000,
				Txid:               "txid-b",
				OutputIndex:        1,
				BoardingAddress:    "bcrt1qshared",
				CreatedAtUnixS:     200,
			},
		},
	}

	resp, err := h.List(t.Context(), &walletdkrpc.ListRequest{})
	require.NoError(t, err)
	require.Len(
		t, resp.GetActivity().GetEntries(), 1,
		"two UTXOs to one address collapse into one row",
	)
	entry := resp.GetActivity().GetEntries()[0]
	require.Equal(t, "deposit-bcrt1qshared", entry.GetId())
	require.EqualValues(
		t, 350_000, entry.GetAmountSat(),
		"the row must show the summed total, not one UTXO",
	)
}

// TestDecorateCooperativeLeaveMatchesRetainedOutpoint verifies that after #610
// (row keyed by the stable leave-job id), the forfeit-driven completion
// correlates on the retained consumed outpoint in vtxo_outpoint, not the id.
func TestDecorateCooperativeLeaveMatchesRetainedOutpoint(t *testing.T) {
	t.Parallel()

	newEntry := func() *walletdkrpc.WalletEntry {
		return &walletdkrpc.WalletEntry{
			Id:     "sendjob-abc",
			Kind:   walletdkrpc.EntryKind_ENTRY_KIND_EXIT,
			Status: walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING,
			Progress: &walletdkrpc.WalletEntryProgress{
				VtxoOutpoint: "abc:0",
			},
		}
	}

	// Forfeiting a set that contains the id (but not the retained
	// outpoint) must NOT complete the row.
	idOnly := newEntry()
	decorateCooperativeLeaveEntry(
		idOnly, map[string]struct{}{"sendjob-abc": {}},
	)
	require.Equal(
		t, walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING,
		idOnly.GetStatus(),
	)

	// Forfeiting the retained outpoint flips it to COMPLETE.
	byOutpoint := newEntry()
	decorateCooperativeLeaveEntry(
		byOutpoint, map[string]struct{}{"abc:0": {}},
	)
	require.Equal(
		t, walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE,
		byOutpoint.GetStatus(),
	)
}

// TestDecorateExitEntryCompletesHashKeyedLeave drives a hash-keyed
// cooperative-leave row through the real decorateExitEntry gate (not
// decorateCooperativeLeaveEntry directly). The daemon's GetUnrollStatus is
// outpoint-only, so the gate must look it up by the retained vtxo_outpoint,
// never the bare send_job_id hash. The fake GetUnrollStatus rejects a
// non-outpoint argument like the real daemon, so a regression that queries by
// the hash id aborts before completion and fails this test.
func TestDecorateExitEntryCompletesHashKeyedLeave(t *testing.T) {
	t.Parallel()

	const outpoint = "aabbcc:0"

	// A bare 64-hex send_job_id has no colon, unlike an outpoint.
	jobID := "deadbeefdeadbeefdeadbeefdeadbeef" +
		"deadbeefdeadbeefdeadbeefdeadbeef"

	newEntry := func() *walletdkrpc.WalletEntry {
		return &walletdkrpc.WalletEntry{
			Id:     jobID,
			Kind:   walletdkrpc.EntryKind_ENTRY_KIND_EXIT,
			Status: walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING,
			Progress: &walletdkrpc.WalletEntryProgress{
				VtxoOutpoint: outpoint,
			},
		}
	}

	// The retained outpoint is forfeited: the gate must query
	// GetUnrollStatus by that outpoint (not the hash), find no unroll job,
	// and complete.
	h, _, rpc := newHistoryFixture(t)
	rpc.unrollStatusResp = &daemonrpc.GetUnrollStatusResponse{Found: false}

	entry := newEntry()
	require.NoError(
		t,
		h.decorateExitEntry(
			t.Context(), entry, map[string]struct{}{outpoint: {}},
		),
	)
	require.Equal(
		t, walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE,
		entry.GetStatus(),
	)
	require.Equal(
		t, outpoint, rpc.unrollStatusLast.GetOutpoint(),
		"must query unroll status by the retained outpoint, not "+
			"the job-id hash",
	)

	// When the retained outpoint is not forfeited, the row stays PENDING
	// and the lookup still succeeds (no spurious error from a hash id).
	h2, _, rpc2 := newHistoryFixture(t)
	rpc2.unrollStatusResp = &daemonrpc.GetUnrollStatusResponse{Found: false}

	pending := newEntry()
	require.NoError(
		t,
		h2.decorateExitEntry(
			t.Context(), pending,
			map[string]struct{}{"other:1": {}},
		),
	)
	require.Equal(
		t, walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING,
		pending.GetStatus(),
	)
}
