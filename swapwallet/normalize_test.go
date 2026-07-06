//go:build walletdkrpc && swapruntime

package swapwallet

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/rpc/swapclientrpc"
	"github.com/lightninglabs/darepo-client/rpc/walletdkrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
)

// TestTruncate confirms truncate clips a long string to n bytes and
// passes shorter strings (and edge cases) through unchanged. Note:
// truncate operates on bytes, not runes, so a multi-byte rune at the
// boundary is intentionally cut — callers passing user-facing strings
// should pass n >= the natural display width.
func TestTruncate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		n    int
		want string
	}{
		{
			"hello",
			10,
			"hello",
		}, // shorter than cap → unchanged
		{
			"hello",
			5,
			"hello",
		}, // equal to cap → unchanged
		{
			"helloworld",
			5,
			"hello",
		},
		{
			"",
			5,
			"",
		},
		{
			"any",
			0,
			"any",
		}, // n <= 0 → unchanged (no truncation)
		{
			"any",
			-1,
			"any",
		}, // negative n → unchanged
	}
	for _, tc := range cases {
		require.Equal(
			t, tc.want, truncate(tc.in, tc.n),
			"in=%q n=%d", tc.in, tc.n,
		)
	}
}

// TestKindFromSwapDirection sanity-checks the direction → kind
// mapping. Unknown directions surface as UNSPECIFIED so a new SDK
// direction does not silently misclassify a row.
func TestKindFromSwapDirection(t *testing.T) {
	t.Parallel()

	require.Equal(
		t, walletdkrpc.EntryKind_ENTRY_KIND_SEND,
		kindFromSwapDirection(
			swapclientrpc.SwapDirection_SWAP_DIRECTION_PAY,
		),
	)
	require.Equal(
		t, walletdkrpc.EntryKind_ENTRY_KIND_RECV,
		kindFromSwapDirection(
			swapclientrpc.SwapDirection_SWAP_DIRECTION_RECEIVE,
		),
	)
	require.Equal(
		t, walletdkrpc.EntryKind_ENTRY_KIND_UNSPECIFIED,
		kindFromSwapDirection(
			swapclientrpc.SwapDirection_SWAP_DIRECTION_UNSPECIFIED,
		),
	)
}

// TestInvoiceMemoDecodesTestNet4 verifies wallet history normalization can
// read memos from persisted invoices on every supported daemon network.
func TestInvoiceMemoDecodesTestNet4(t *testing.T) {
	t.Parallel()

	invoice, _ := testPreparedInvoiceOnNet(
		t, &chaincfg.TestNet4Params, btcutil.Amount(12_345),
		"testnet4 memo",
	)

	require.Equal(t, "testnet4 memo", invoiceMemo(invoice))
}

// TestStatusFromSwapState covers every branch: pending always wins,
// completed maps to COMPLETE, terminal failure states map to FAILED,
// and any other non-pending state is treated as PENDING so callers keep
// polling.
func TestStatusFromSwapState(t *testing.T) {
	t.Parallel()

	// Pending always wins regardless of state.
	require.Equal(
		t, walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING,
		statusFromSwapState(
			swapclientrpc.SwapState_SWAP_STATE_COMPLETED, true,
		),
	)
	require.Equal(
		t, walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING,
		statusFromSwapState(
			swapclientrpc.SwapState_SWAP_STATE_REFUNDED, true,
		),
	)

	// Terminal completed -> COMPLETE.
	require.Equal(
		t, walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE,
		statusFromSwapState(
			swapclientrpc.SwapState_SWAP_STATE_COMPLETED, false,
		),
	)

	// Terminal failed states, including refunded payments, -> FAILED.
	for _, s := range []swapclientrpc.SwapState{
		swapclientrpc.SwapState_SWAP_STATE_FAILED,
		swapclientrpc.SwapState_SWAP_STATE_EXPIRED,
		swapclientrpc.SwapState_SWAP_STATE_NEEDS_INTERVENTION,
		swapclientrpc.SwapState_SWAP_STATE_REFUNDED,
	} {
		require.Equal(
			t, walletdkrpc.EntryStatus_ENTRY_STATUS_FAILED,
			statusFromSwapState(s, false),
		)
	}

	// Unknown non-pending falls back to PENDING.
	require.Equal(
		t, walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING,
		statusFromSwapState(
			swapclientrpc.SwapState_SWAP_STATE_UNSPECIFIED, false,
		),
	)
}

// TestFailureCodeFromSwapState classifies each terminal failure state and
// leaves pending or non-failure states unspecified.
func TestFailureCodeFromSwapState(t *testing.T) {
	t.Parallel()

	// Pending is never a failure, even in a terminal-failed state.
	require.Equal(
		t, unspecCode, failureCodeFromSwapState(
			swapclientrpc.SwapState_SWAP_STATE_FAILED, true,
		),
	)

	cases := []struct {
		state swapclientrpc.SwapState
		want  walletdkrpc.EntryFailureCode
	}{{
		state: swapclientrpc.SwapState_SWAP_STATE_EXPIRED,
		want:  fcEnum("EXPIRED"),
	}, {
		state: swapclientrpc.SwapState_SWAP_STATE_REFUNDED,
		want:  fcEnum("REFUNDED"),
	}, {
		state: swapclientrpc.SwapState_SWAP_STATE_NEEDS_INTERVENTION,
		want:  fcEnum("NEEDS_INTERVENTION"),
	}, {
		state: swapclientrpc.SwapState_SWAP_STATE_FAILED,
		want:  fcEnum("FAILED"),
	}}
	for _, tc := range cases {
		require.Equal(
			t, tc.want, failureCodeFromSwapState(tc.state, false),
			"state=%v", tc.state,
		)
	}

	// Unknown non-failure state carries no code.
	require.Equal(
		t, unspecCode, failureCodeFromSwapState(
			swapclientrpc.SwapState_SWAP_STATE_COMPLETED, false,
		),
	)
}

// fcEnum resolves a short failure-code suffix to its generated enum value,
// keeping the test table within the line limit.
func fcEnum(suffix string) walletdkrpc.EntryFailureCode {
	m := walletdkrpc.EntryFailureCode_value

	return walletdkrpc.EntryFailureCode(m["ENTRY_FAILURE_CODE_"+suffix])
}

// TestFailureCodeMatchesFailedStatus is a drift guard: every SwapState that
// statusFromSwapState collapses to FAILED must carry a non-unspecified failure
// code, and no other state may. This ties the two classifiers together so a
// newly-added terminal-failure state cannot silently stay uncoded.
func TestFailureCodeMatchesFailedStatus(t *testing.T) {
	t.Parallel()

	for s := range swapclientrpc.SwapState_name {
		state := swapclientrpc.SwapState(s)
		failed := statusFromSwapState(state, false) ==
			walletdkrpc.EntryStatus_ENTRY_STATUS_FAILED
		coded := failureCodeFromSwapState(state, false) != unspecCode
		require.Equal(t, failed, coded, "state=%v", state)
	}
}

// TestFailureReasonFromTerminal trims whitespace and leaves the rest.
func TestFailureReasonFromTerminal(t *testing.T) {
	t.Parallel()

	require.Equal(t, "", failureReasonFromTerminal(""))
	require.Equal(t, "", failureReasonFromTerminal("  \t\n"))
	require.Equal(
		t, "timed out", failureReasonFromTerminal("  timed out\n"),
	)
}

// TestEntryNotePrefersExplicitNote confirms the immediate submit path keeps
// the user's memo even before the persisted invoice summary is read back.
func TestEntryNotePrefersExplicitNote(t *testing.T) {
	t.Parallel()

	require.Equal(t, "coffee", entryNote(" coffee ", "not-an-invoice"))
	require.Equal(t, "", entryNote("", "not-an-invoice"))
}

// TestUnixToTime confirms zero turns into "now-ish" and non-zero
// values round-trip through time.Unix.
func TestUnixToTime(t *testing.T) {
	t.Parallel()

	require.False(
		t, unixToTime(0).IsZero(),
		"zero must surface a usable now-ish time, not the zero time",
	)

	ts := int64(1_700_000_000)
	require.Equal(t, time.Unix(ts, 0), unixToTime(ts))
}

// TestWalletVTXOFromDaemon confirms the projection drops internal
// detail (chain_depth, oor finalized PSBTs, spent_by) and copies the
// wallet-visible fields verbatim. Hidden statuses surface as keep=false
// so the wallet view stays focused on actionable VTXOs.
func TestWalletVTXOFromDaemon(t *testing.T) {
	t.Parallel()

	// Nil input is a defensive no-op.
	out, keep := walletVTXOFromDaemon(nil)
	require.False(t, keep)
	require.Nil(t, out)

	// Live VTXO is kept and projected verbatim.
	in := &daemonrpc.VTXO{
		Outpoint:       "abc:0",
		AmountSat:      10_000,
		Status:         daemonrpc.VTXOStatus_VTXO_STATUS_LIVE,
		BatchExpiry:    1_000_000,
		RelativeExpiry: 144,
		CommitmentTxid: "dead",
	}
	out, keep = walletVTXOFromDaemon(in)
	require.True(t, keep)
	require.Equal(t, &walletdkrpc.WalletVTXO{
		Outpoint:       "abc:0",
		AmountSat:      10_000,
		Status:         "live",
		BatchExpiry:    1_000_000,
		RelativeExpiry: 144,
		CommitmentTxid: "dead",
	}, out)

	// Terminal states drop out.
	for _, s := range []daemonrpc.VTXOStatus{
		daemonrpc.VTXOStatus_VTXO_STATUS_FORFEITED,
		daemonrpc.VTXOStatus_VTXO_STATUS_SPENT,
		daemonrpc.VTXOStatus_VTXO_STATUS_FAILED,
		daemonrpc.VTXOStatus_VTXO_STATUS_UNSPECIFIED,
	} {
		_, keep := walletVTXOFromDaemon(&daemonrpc.VTXO{
			Status: s,
		})
		require.False(t, keep, "status %v should be hidden", s)
	}
}

// TestWalletVTXOStatusFromDaemon table-tests the status string
// projection across every defined enum value.
func TestWalletVTXOStatusFromDaemon(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   daemonrpc.VTXOStatus
		out  string
		keep bool
	}{
		{
			daemonrpc.VTXOStatus_VTXO_STATUS_LIVE,
			"live",
			true,
		},
		{
			daemonrpc.VTXOStatus_VTXO_STATUS_PENDING_FORFEIT,
			"pending_forfeit", true,
		},
		{
			daemonrpc.VTXOStatus_VTXO_STATUS_FORFEITING,
			"forfeiting", true,
		},
		{
			daemonrpc.VTXOStatus_VTXO_STATUS_SPENDING,
			"spending", true,
		},
		{
			daemonrpc.VTXOStatus_VTXO_STATUS_UNILATERAL_EXIT,
			"unilateral_exit", true,
		},
		{
			daemonrpc.VTXOStatus_VTXO_STATUS_FORFEITED,
			"",
			false,
		},
		{
			daemonrpc.VTXOStatus_VTXO_STATUS_SPENT,
			"",
			false,
		},
		{
			daemonrpc.VTXOStatus_VTXO_STATUS_FAILED,
			"",
			false,
		},
		{
			daemonrpc.VTXOStatus_VTXO_STATUS_UNSPECIFIED,
			"",
			false,
		},
	}
	for _, tc := range cases {
		got, keep := walletVTXOStatusFromDaemon(tc.in)
		require.Equal(t, tc.out, got, "in=%v", tc.in)
		require.Equal(t, tc.keep, keep, "in=%v", tc.in)
	}
}

// TestOnchainTxFromLedgerRow confirms the projection flattens the
// daemonrpc TransactionHistoryEntry onto the wallet-facing OnchainTx
// shape and drops internal correlators (round_id, session_id,
// debit/credit accounts) entirely.
func TestOnchainTxFromLedgerRow(t *testing.T) {
	t.Parallel()

	// Nil → empty (not a panic).
	require.Equal(
		t, &walletdkrpc.OnchainTx{}, onchainTxFromLedgerRow(nil),
	)

	in := &daemonrpc.TransactionHistoryEntry{
		Txid:               "deadbeef",
		Type:               "boarding",
		AmountSat:          25_000,
		FeeSat:             1_500,
		ConfirmationStatus: "confirmed",
		ConfirmationHeight: 12345,
		CreatedAtUnixS:     500,
		Description:        "boarding deposit",
		// Internal correlators that must NOT leak.
		RoundId: []byte{
			1,
			2,
			3,
		},
		SessionId: []byte{
			9,
			9,
			9,
		},
		DebitAccount:  "internal:vtxo",
		CreditAccount: "internal:transfers_in",
	}
	out := onchainTxFromLedgerRow(in)
	require.Equal(t, "deadbeef", out.GetTxid())
	require.Equal(t, "boarding", out.GetKind())
	require.Equal(t, int64(25_000), out.GetAmountSat())
	require.Equal(t, int64(1_500), out.GetFeeSat())
	require.Equal(t, "confirmed", out.GetStatus())
	require.Equal(t, int32(12345), out.GetConfirmationHeight())
	require.Equal(t, int64(500), out.GetCreatedAtUnix())
	require.Equal(t, "boarding deposit", out.GetDescription())
}

// TestPaginateVTXOs confirms paginateVTXOs slices the inventory by
// offset/limit, handles past-end requests by returning nil, and never
// shares the underlying slice with the caller's input.
func TestPaginateVTXOs(t *testing.T) {
	t.Parallel()

	in := []*walletdkrpc.WalletVTXO{
		{
			Outpoint: "a:0",
		},
		{
			Outpoint: "b:0",
		},
		{
			Outpoint: "c:0",
		},
		{
			Outpoint: "d:0",
		},
	}

	// Whole page.
	got := paginateVTXOs(in, 0, 10)
	require.Len(t, got, 4)
	require.Equal(t, "a:0", got[0].GetOutpoint())
	require.Equal(t, "d:0", got[3].GetOutpoint())

	// Sub-page with offset.
	got = paginateVTXOs(in, 1, 2)
	require.Len(t, got, 2)
	require.Equal(t, "b:0", got[0].GetOutpoint())
	require.Equal(t, "c:0", got[1].GetOutpoint())

	// Offset past end.
	require.Nil(t, paginateVTXOs(in, 10, 5))

	// Limit larger than tail.
	got = paginateVTXOs(in, 2, 10)
	require.Len(t, got, 2)
	require.Equal(t, "c:0", got[0].GetOutpoint())
}

// TestLeaveEntryStub confirms the stub builds a PENDING EXIT entry
// with the canonical id derived from the first queued outpoint and
// the amount carried as a negative (outgoing) signed value.
func TestLeaveEntryStub(t *testing.T) {
	t.Parallel()

	out := leaveEntryStub(
		[]string{
			"abc:0",
			"def:1",
		}, "bcrt1q...",
		5_000, "rent",
	)
	require.Equal(t, "abc:0", out.GetId())
	require.Equal(t, walletdkrpc.EntryKind_ENTRY_KIND_EXIT, out.GetKind())
	require.Equal(
		t, walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING,
		out.GetStatus(),
	)
	require.Equal(t, int64(-5_000), out.GetAmountSat())
	require.Equal(t, "rent", out.GetNote())
	require.NotZero(t, out.GetCreatedAtUnix())
	require.Equal(t, out.GetCreatedAtUnix(), out.GetUpdatedAtUnix())

	// No queued outpoints → id is empty.
	out = leaveEntryStub(nil, "bcrt1q...", 1_000, "")
	require.Equal(t, "", out.GetId())
}

// TestSwapEntryFromSummaryCallerKindOverride confirms the
// callerKind override pins the wallet direction even when the
// underlying SwapSummary's direction is UNSPECIFIED (a real
// intermediate state on first lazy summary).
func TestSwapEntryFromSummaryCallerKindOverride(t *testing.T) {
	t.Parallel()

	s := &swapclientrpc.SwapSummary{
		PaymentHash: "ph",
		Invoice:     "lnbc1invoice",
		Direction:   swapclientrpc.SwapDirection_SWAP_DIRECTION_UNSPECIFIED,
		AmountSat:   10_000,
		Pending:     true,
	}

	send := swapEntryFromSummary(
		s, "", "", walletdkrpc.EntryKind_ENTRY_KIND_SEND,
	)
	require.Equal(t, walletdkrpc.EntryKind_ENTRY_KIND_SEND, send.GetKind())
	require.Equal(
		t, "lnbc1invoice",
		send.GetRequest().GetLightningInvoice().GetInvoice(),
	)
	require.Equal(
		t, int64(-10_000), send.GetAmountSat(),
		"caller-pinned SEND must surface as outgoing (negative)",
	)

	recv := swapEntryFromSummary(
		s, "", "", walletdkrpc.EntryKind_ENTRY_KIND_RECV,
	)
	require.Equal(t, walletdkrpc.EntryKind_ENTRY_KIND_RECV, recv.GetKind())
	require.Equal(
		t, int64(10_000), recv.GetAmountSat(),
		"caller-pinned RECV must surface as incoming (positive)",
	)
}

// TestSwapEntryFromSummaryRequestMetadata keeps invoices and payment hashes in
// their own fields. Historical rows may not have a BOLT-11 invoice in the swap
// summary, but the payment hash is still a useful identifier.
func TestSwapEntryFromSummaryRequestMetadata(t *testing.T) {
	t.Parallel()

	s := &swapclientrpc.SwapSummary{
		PaymentHash: "ph",
		Direction:   swapclientrpc.SwapDirection_SWAP_DIRECTION_PAY,
		AmountSat:   10_000,
		Pending:     true,
	}

	send := swapEntryFromSummary(
		s, "", "", walletdkrpc.EntryKind_ENTRY_KIND_UNSPECIFIED,
	)
	req := send.GetRequest().GetLightningInvoice()
	require.Equal(t, "ph", req.GetPaymentHash())
	require.Empty(t, req.GetInvoice())

	withInvoice := swapEntryFromSummary(
		s, "", "lnbc1invoice", walletdkrpc.EntryKind_ENTRY_KIND_SEND,
	)
	req = withInvoice.GetRequest().GetLightningInvoice()
	require.Equal(t, "ph", req.GetPaymentHash())
	require.Equal(t, "lnbc1invoice", req.GetInvoice())

	withHashHint := swapEntryFromSummary(
		s, "", "ph", walletdkrpc.EntryKind_ENTRY_KIND_SEND,
	)
	req = withHashHint.GetRequest().GetLightningInvoice()
	require.Equal(t, "ph", req.GetPaymentHash())
	require.Empty(t, req.GetInvoice())
}

// TestSwapEntryFromSummaryPreimage confirms the Lightning payment preimage
// rides from the swap summary onto the entry's progress once durably known,
// and stays empty while the send is still in flight. The preimage is the
// L402 proof-of-payment surfaced on a settled Lightning send.
func TestSwapEntryFromSummaryPreimage(t *testing.T) {
	t.Parallel()

	// A completed pay swap with a revealed preimage carries it onto the
	// progress sub-object so a settle watcher can read it off the row.
	settled := swapEntryFromSummary(&swapclientrpc.SwapSummary{
		PaymentHash: "ph",
		Direction:   swapclientrpc.SwapDirection_SWAP_DIRECTION_PAY,
		State:       swapclientrpc.SwapState_SWAP_STATE_COMPLETED,
		Preimage:    "deadbeef",
	}, "", "", walletdkrpc.EntryKind_ENTRY_KIND_SEND)
	require.Equal(t, "deadbeef", settled.GetProgress().GetPreimage())

	// A still-pending send has no preimage yet, so the field stays empty.
	pending := swapEntryFromSummary(&swapclientrpc.SwapSummary{
		PaymentHash: "ph",
		Direction:   swapclientrpc.SwapDirection_SWAP_DIRECTION_PAY,
		State:       swapclientrpc.SwapState_SWAP_STATE_WAITING_FOR_CLAIM,
		Pending:     true,
	}, "", "", walletdkrpc.EntryKind_ENTRY_KIND_SEND)
	require.Empty(t, pending.GetProgress().GetPreimage())
}

// TestSwapEntryFromSummaryNilSafe confirms a nil input does not
// panic and returns a usable (empty) WalletEntry.
func TestSwapEntryFromSummaryNilSafe(t *testing.T) {
	t.Parallel()

	out := swapEntryFromSummary(
		nil, "", "", walletdkrpc.EntryKind_ENTRY_KIND_UNSPECIFIED,
	)
	require.NotNil(t, out)
	require.Equal(
		t, walletdkrpc.EntryKind_ENTRY_KIND_UNSPECIFIED, out.GetKind(),
	)

	// A nil summary is not a failure, so failure_code stays unset.
	require.Nil(t, out.FailureCode)
}

// TestSwapEntryFromSummaryFailureCodePresence confirms failure_code is
// presence-tracked: a non-failed entry leaves it unset (its absence is the
// canonical "no failure" signal), while a failed entry carries the concrete
// code. GetFailureCode still reads back UNSPECIFIED when absent.
func TestSwapEntryFromSummaryFailureCodePresence(t *testing.T) {
	t.Parallel()

	ok := swapEntryFromSummary(&swapclientrpc.SwapSummary{
		PaymentHash: "ph-ok",
		State:       swapclientrpc.SwapState_SWAP_STATE_COMPLETED,
	}, "", "", walletdkrpc.EntryKind_ENTRY_KIND_RECV)
	require.Nil(
		t, ok.FailureCode, "non-failed entry must omit failure_code",
	)
	require.Equal(t, unspecCode, ok.GetFailureCode())

	failed := swapEntryFromSummary(&swapclientrpc.SwapSummary{
		PaymentHash: "ph-expired",
		State:       swapclientrpc.SwapState_SWAP_STATE_EXPIRED,
	}, "", "", walletdkrpc.EntryKind_ENTRY_KIND_RECV)
	require.NotNil(
		t, failed.FailureCode, "failed entry must set failure_code",
	)
	require.Equal(t, fcEnum("EXPIRED"), failed.GetFailureCode())
}

// TestWalletEntryFailureCodeJSONShape reproduces the gateway's JSON marshaling
// (UseProtoNames + EmitUnpopulated, per gateway.ServeMuxOptions) and confirms
// the presence-tracked failure_code is omitted for a non-failed entry and
// present for a failed one. Before failure_code was made optional it serialized
// as "ENTRY_FAILURE_CODE_UNSPECIFIED" on every non-failed entry; this is the
// shape guard that pins that regression shut.
func TestWalletEntryFailureCodeJSONShape(t *testing.T) {
	t.Parallel()

	marshal := protojson.MarshalOptions{
		UseProtoNames:   true,
		EmitUnpopulated: true,
	}
	decode := func(e *walletdkrpc.WalletEntry) map[string]any {
		raw, err := marshal.Marshal(e)
		require.NoError(t, err)

		var m map[string]any
		require.NoError(t, json.Unmarshal(raw, &m))

		return m
	}

	ok := swapEntryFromSummary(&swapclientrpc.SwapSummary{
		PaymentHash: "ph-ok",
		State:       swapclientrpc.SwapState_SWAP_STATE_COMPLETED,
	}, "", "", walletdkrpc.EntryKind_ENTRY_KIND_RECV)
	_, present := decode(ok)["failure_code"]
	require.False(
		t, present, "non-failed entry JSON must not carry failure_code",
	)

	failed := swapEntryFromSummary(&swapclientrpc.SwapSummary{
		PaymentHash: "ph-expired",
		State:       swapclientrpc.SwapState_SWAP_STATE_EXPIRED,
	}, "", "", walletdkrpc.EntryKind_ENTRY_KIND_RECV)
	require.Equal(
		t, "ENTRY_FAILURE_CODE_EXPIRED", decode(failed)["failure_code"],
		"failed entry JSON must carry the concrete failure_code",
	)
}
