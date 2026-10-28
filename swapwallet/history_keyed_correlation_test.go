//go:build wavewalletrpc && swapruntime

package swapwallet

// Regression matrix for issue #1464: an internal swap funding OOR leg
// whose swap record never persisted its funding session id surfaced as a
// phantom ledger-N SEND that no restart could reconcile away. Each case
// builds one production shape and asserts whether the send row is hidden,
// covering the structured swap correlation, the keyed (idempotency)
// correlation added as the fix, and the documented hard boundaries.

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/lightninglabs/wavelength/ledger"
	"github.com/lightninglabs/wavelength/rpc/swapclientrpc"
	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/stretchr/testify/require"
)

// TestKeyedFundingCorrelationPreservesUnprovenSends ensures optional lookup
// failures and unrelated dispatch keys cannot authorize durable row removal.
func TestKeyedFundingCorrelationPreservesUnprovenSends(t *testing.T) {
	t.Parallel()

	paymentHash := strings.Repeat("ab", 32)
	for _, test := range []struct {
		name      string
		key       string
		err       error
		direction swapclientrpc.SwapDirection
	}{
		{
			name: "lookup failure",
			err:  errors.New("lookup unavailable"),
		},
		{
			name: "keyless dispatch",
		},
		{
			name: "unrelated key",
			key:  "user-send:" + paymentHash,
		},
		{
			name: "unknown pay swap",
			key:  "in-swap-funding:" + strings.Repeat("cd", 32),
		},
		{
			name: "short hash",
			key:  "in-swap-funding:ab",
		},
		{
			name: "nonhex hash",
			key:  "in-swap-funding:" + strings.Repeat("z", 64),
		},
		{
			name: "receive swap",
			key:  "in-swap-funding:" + paymentHash,
			direction: swapclientrpc.
				SwapDirection_SWAP_DIRECTION_RECEIVE,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h, swap, rpc := newHistoryFixture(t)
			session := testBytes(32, 0x91)
			summary := paySwapSummary("", 12_233)
			summary.PaymentHash = paymentHash
			if test.direction != 0 {
				summary.Direction = test.direction
			}
			swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{
				Swaps: []*swapclientrpc.SwapSummary{
					summary,
				},
			}
			rows := []*waverpc.TransactionHistoryEntry{
				fundingSendRow(session, 12_233, 138),
			}
			rpc.listTxResp = &waverpc.ListTransactionsResponse{
				Transactions: rows,
			}
			h.deps.OORDispatch = &fakeOORDispatch{
				keys: map[string]string{
					testSessionString(t, session): test.key,
				},
				err: test.err,
			}

			require.Contains(t, visibleSendIDs(t, h), "ledger-138")
			require.Empty(t, h.sortedInternalOORActivityIDs())
		})
	}
}

// TestBackfillRepairsKeyedFundingSends covers upgrade repair with missing and
// complete swap metadata. Both paths must remove the stored row and its events.
func TestBackfillRepairsKeyedFundingSends(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name        string
		withSession bool
		change      int64
	}{
		{
			name: "missing session",
		},
		{
			name:        "known session",
			withSession: true,
		},
		{
			name:   "missing session with change",
			change: 2_767,
		},
		{
			name:        "known session with change",
			withSession: true,
			change:      2_767,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()
			runtime, store, rpc := newReconcileFixture(t)
			swap, ok := runtime.deps.SwapService.(*fakeSwapService)
			require.True(t, ok)
			session := testBytes(32, 0x71)
			sessionHex := testSessionString(t, session)
			paymentHash := strings.Repeat("ab", 32)
			summary := paySwapSummary("", 12_233)
			summary.PaymentHash = paymentHash
			if test.withSession {
				summary.FundingSessionId = sessionHex
			}
			swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{
				Swaps: []*swapclientrpc.SwapSummary{
					summary,
				},
			}
			gross := 12_233 + test.change
			rows := []*waverpc.TransactionHistoryEntry{
				fundingSendRow(session, gross, 138),
				fundingSendRow(testBytes(32, 0x81), 500, 139),
			}
			rpc.listTxResp = &waverpc.ListTransactionsResponse{
				Transactions: rows,
			}
			if test.change > 0 {
				change := &waverpc.TransactionHistoryEntry{
					Type:               "oor",
					ConfirmationStatus: "recorded",
					AmountSat:          test.change,
					SessionId:          session,
					EntryId:            140,
					CreatedAtUnixS:     100,
				}
				change.Subtype = ledger.EventVTXOReceived
				change.DebitAccount = ledger.AccountVTXOBalance
				change.CreditAccount = ledger.AccountTransfersIn
				rpc.listTxResp.Transactions = append(
					rpc.listTxResp.Transactions, change,
				)
			}
			runtime.deps.OORDispatch = &fakeOORDispatch{
				keys: map[string]string{
					sessionHex: "in-swap-funding:" +
						paymentHash,
				},
			}

			// Seed the durable artifact as an older binary would.
			artifact, ok := walletEntryFromLedgerRow(
				rpc.listTxResp.Transactions[0],
			)
			require.True(t, ok)
			_, _, err := runtime.project(ctx, artifact)
			require.NoError(t, err)

			for range 2 {
				_, err = runtime.reprojectActivity(
					ctx, nil, true,
				)
				require.NoError(t, err)
				_, err = store.GetEntry(ctx, "ledger-138")
				require.ErrorIs(t, err, sql.ErrNoRows)
				_, err = store.GetEntry(ctx, paymentHash)
				require.NoError(t, err)
				_, err = store.GetEntry(ctx, "ledger-139")
				require.NoError(t, err)
				_, err = store.GetEntry(ctx, "ledger-140")
				require.ErrorIs(t, err, sql.ErrNoRows)
			}

			events, err := store.PullEvents(ctx, 0, 10)
			require.NoError(t, err)
			for _, event := range events {
				require.NotEqual(
					t, "ledger-138", event.CanonicalID,
				)
			}
		})
	}
}

// visibleSendIDs returns the ids of all visible SEND entries.
func visibleSendIDs(t *testing.T, h *history) []string {
	t.Helper()

	resp, err := h.List(t.Context(), &wavewalletrpc.ListRequest{})
	require.NoError(t, err)

	var ids []string
	for _, e := range resp.GetActivity().GetEntries() {
		if e.GetKind() == wavewalletrpc.EntryKind_ENTRY_KIND_SEND {
			ids = append(ids, e.GetId())
		}
	}

	return ids
}

// paySwapSummary creates a completed pay with optional funding metadata.
func paySwapSummary(sessionHex string,
	amount int64) *swapclientrpc.SwapSummary {

	return &swapclientrpc.SwapSummary{
		PaymentHash: "payment-hash",
		Direction: swapclientrpc.
			SwapDirection_SWAP_DIRECTION_PAY,
		State:            swapclientrpc.SwapState_SWAP_STATE_COMPLETED,
		AmountSat:        amount,
		UpdatedAtUnix:    200,
		FundingSessionId: sessionHex,
	}
}

// fundingSendRow creates the gross ledger debit before swap registration.
func fundingSendRow(session []byte, amount,
	entryID int64) *waverpc.TransactionHistoryEntry {

	return &waverpc.TransactionHistoryEntry{
		Type:               "oor",
		Subtype:            ledger.EventVTXOSent,
		ConfirmationStatus: "recorded",
		AmountSat:          amount,
		DebitAccount:       ledger.AccountTransfersOut,
		CreditAccount:      ledger.AccountVTXOBalance,
		SessionId:          session,
		EntryId:            entryID,
		CreatedAtUnixS:     100,
	}
}

// TestIssue1464CaseAExactInput covers structured funding correlation without
// wallet change as a control for the keyed fallback.
func TestIssue1464CaseAExactInput(t *testing.T) {
	t.Parallel()

	h, swap, rpc := newHistoryFixture(t)
	sessionID := testBytes(32, 0x31)
	sessionHex := testSessionString(t, sessionID)

	swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{
		Swaps: []*swapclientrpc.SwapSummary{
			paySwapSummary(sessionHex, 12_233),
		},
	}
	rpc.listTxResp = &waverpc.ListTransactionsResponse{
		Transactions: []*waverpc.TransactionHistoryEntry{
			fundingSendRow(sessionID, 12_233, 138),
		},
	}

	require.NotContains(t, visibleSendIDs(t, h), "ledger-138")
}

// TestIssue1464CaseBChangeKeyedByRealTxid documents that a receive without its
// funding session id cannot be attributed as wallet change.
func TestIssue1464CaseBChangeKeyedByRealTxid(t *testing.T) {
	t.Parallel()

	h, swap, rpc := newHistoryFixture(t)
	sessionID := testBytes(32, 0x32)
	sessionHex := testSessionString(t, sessionID)

	swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{
		Swaps: []*swapclientrpc.SwapSummary{
			paySwapSummary(sessionHex, 12_233),
		},
	}
	rpc.listTxResp = &waverpc.ListTransactionsResponse{
		Transactions: []*waverpc.TransactionHistoryEntry{
			fundingSendRow(sessionID, 15_000, 138),
			{
				Type:               "oor",
				Subtype:            ledger.EventVTXOReceived,
				ConfirmationStatus: "recorded",
				AmountSat:          2_767,
				DebitAccount:       ledger.AccountVTXOBalance,
				CreditAccount:      ledger.AccountTransfersIn,
				// Real txid, NOT the session id.
				Txid: "aa55aa55aa55aa55aa55aa55aa55aa55" +
					"aa55aa55aa55aa55aa55aa55aa55aa55",
				OutputIndex:    1,
				EntryId:        139,
				CreatedAtUnixS: 101,
			},
		},
	}

	// Known limitation, documented: not a current production shape (both
	// receive-booking sites always set SessionID), but if a change row
	// ever loses its session id the gross send leaks.
	require.Contains(t, visibleSendIDs(t, h), "ledger-138")
}

// TestIssue1464CaseCMissingFundingSession preserves legacy visibility when
// neither funding metadata nor dispatch lookup is available.
func TestIssue1464CaseCMissingFundingSession(t *testing.T) {
	t.Parallel()

	h, swap, rpc := newHistoryFixture(t)
	sessionID := testBytes(32, 0x33)

	swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{
		Swaps: []*swapclientrpc.SwapSummary{
			paySwapSummary("", 12_233),
		},
	}
	rpc.listTxResp = &waverpc.ListTransactionsResponse{
		Transactions: []*waverpc.TransactionHistoryEntry{
			fundingSendRow(sessionID, 12_233, 138),
		},
	}

	require.Contains(t, visibleSendIDs(t, h), "ledger-138")
}

// fakeOORDispatch is an OORDispatchLookup fake keyed by session id string.
type fakeOORDispatch struct {
	keys map[string]string
	err  error
}

// DispatchIdempotencyKey returns the configured key or lookup failure.
func (f *fakeOORDispatch) DispatchIdempotencyKey(_ context.Context,
	sessionID string) (string, error) {

	if f.err != nil {
		return "", f.err
	}

	return f.keys[sessionID], nil
}

// TestIssue1464CaseCFixedByDispatchKey hides funding without a persisted swap
// session id when the dispatch key names a known pay swap.
func TestIssue1464CaseCFixedByDispatchKey(t *testing.T) {
	t.Parallel()

	h, swap, rpc := newHistoryFixture(t)
	sessionID := testBytes(32, 0x36)
	sessionHex := testSessionString(t, sessionID)
	payHash := strings.Repeat("ab", 32)
	summary := paySwapSummary("", 12_233)
	summary.PaymentHash = payHash

	swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{
		Swaps: []*swapclientrpc.SwapSummary{
			summary,
		},
	}
	rpc.listTxResp = &waverpc.ListTransactionsResponse{
		Transactions: []*waverpc.TransactionHistoryEntry{
			fundingSendRow(sessionID, 12_233, 138),
		},
	}
	h.deps.OORDispatch = &fakeOORDispatch{
		keys: map[string]string{
			sessionHex: "in-swap-funding:" + payHash,
		},
	}

	require.NotContains(t, visibleSendIDs(t, h), "ledger-138")
}

// TestIssue1464CaseCUnknownSwapStaysVisible requires a matching pay swap before
// a dispatch key can hide a raw send.
func TestIssue1464CaseCUnknownSwapStaysVisible(t *testing.T) {
	t.Parallel()

	h, swap, rpc := newHistoryFixture(t)
	sessionID := testBytes(32, 0x37)
	sessionHex := testSessionString(t, sessionID)

	swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{}
	rpc.listTxResp = &waverpc.ListTransactionsResponse{
		Transactions: []*waverpc.TransactionHistoryEntry{
			fundingSendRow(sessionID, 12_233, 138),
		},
	}
	h.deps.OORDispatch = &fakeOORDispatch{
		keys: map[string]string{
			sessionHex: "in-swap-funding:" +
				strings.Repeat("cd", 32),
		},
	}

	require.Contains(t, visibleSendIDs(t, h), "ledger-138")
}

// TestIssue1464CaseDSwapAbsent keeps sends visible without a swap record.
func TestIssue1464CaseDSwapAbsent(t *testing.T) {
	t.Parallel()

	h, swap, rpc := newHistoryFixture(t)
	sessionID := testBytes(32, 0x34)

	swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{}
	rpc.listTxResp = &waverpc.ListTransactionsResponse{
		Transactions: []*waverpc.TransactionHistoryEntry{
			fundingSendRow(sessionID, 12_233, 138),
		},
	}

	require.Contains(t, visibleSendIDs(t, h), "ledger-138")
}

// TestIssue1464CaseELiongrassShape covers the reported single-send amount
// with complete funding metadata as a structured-correlation control.
func TestIssue1464CaseELiongrassShape(t *testing.T) {
	t.Parallel()

	h, swap, rpc := newHistoryFixture(t)
	sessionID := testBytes(32, 0x35)
	sessionHex := testSessionString(t, sessionID)

	swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{
		Swaps: []*swapclientrpc.SwapSummary{
			paySwapSummary(sessionHex, 12_233),
		},
	}
	rpc.listTxResp = &waverpc.ListTransactionsResponse{
		Transactions: []*waverpc.TransactionHistoryEntry{
			fundingSendRow(sessionID, 12_233, 138),
		},
	}

	sends := visibleSendIDs(t, h)
	t.Logf("visible sends: %v", sends)
	require.NotContains(t, sends, "ledger-138")
}
