//go:build walletrpc && swapruntime

package swapwallet

import (
	"strings"
	"time"

	"github.com/lightninglabs/darepo-client/rpc/swapclientrpc"
	"github.com/lightninglabs/darepo-client/rpc/walletrpc"
)

// truncatedCounterpartyLen caps the rendered counterparty field length so
// the wallet UI never has to deal with a multi-hundred-character invoice
// string. Truncation keeps the start of the bech32 prefix plus a recognisable
// chunk of the body.
const truncatedCounterpartyLen = 32

// swapEntryFromSummary normalizes a swapclientrpc.SwapSummary into the flat
// WalletEntry shape. The wallet layer collapses every internal swap state
// into PENDING / COMPLETE / FAILED and drops all internal correlators
// (session IDs, settlement type, vHTLC outpoints) so the user surface is
// uniform across SEND, RECV, DEPOSIT, and EXIT rows.
//
// counterparty carries the invoice (truncated) for SEND rows and the
// payment hash (truncated) for RECV rows so callers can correlate a
// generated invoice with the row it produced; note carries the caller's
// label as-is when present.
func swapEntryFromSummary(s *swapclientrpc.SwapSummary, note string,
	counterparty string) *walletrpc.WalletEntry {

	if s == nil {
		return &walletrpc.WalletEntry{
			Kind:   walletrpc.EntryKind_ENTRY_KIND_UNSPECIFIED,
			Status: walletrpc.EntryStatus_ENTRY_STATUS_UNSPECIFIED,
		}
	}

	entry := &walletrpc.WalletEntry{
		Id:            s.GetPaymentHash(),
		Kind:          kindFromSwapDirection(s.GetDirection()),
		Status:        statusFromSwapState(s.GetState(), s.GetPending()),
		FeeSat:        int64(s.GetFeeSat()),
		Counterparty:  truncate(counterparty, truncatedCounterpartyLen),
		CreatedAtUnix: s.GetCreatedAtUnix(),
		UpdatedAtUnix: s.GetUpdatedAtUnix(),
		Note:          note,
		FailureReason: failureReasonFromTerminal(s.GetTerminalReason()),
	}

	// Render amount with the wallet's signed-amount convention: positive
	// for incoming RECV, negative for outgoing SEND.
	amount := s.GetAmountSat()
	switch entry.Kind {
	case walletrpc.EntryKind_ENTRY_KIND_SEND:
		entry.AmountSat = -amount

	case walletrpc.EntryKind_ENTRY_KIND_RECV:
		entry.AmountSat = amount

	default:
		entry.AmountSat = amount
	}

	return entry
}

// kindFromSwapDirection maps the swap proto direction to a user-facing
// EntryKind.
func kindFromSwapDirection(dir swapclientrpc.SwapDirection,
) walletrpc.EntryKind {

	switch dir {
	case swapclientrpc.SwapDirection_SWAP_DIRECTION_PAY:
		return walletrpc.EntryKind_ENTRY_KIND_SEND

	case swapclientrpc.SwapDirection_SWAP_DIRECTION_RECEIVE:
		return walletrpc.EntryKind_ENTRY_KIND_RECV

	default:
		return walletrpc.EntryKind_ENTRY_KIND_UNSPECIFIED
	}
}

// statusFromSwapState collapses every backing SwapState plus the pending
// flag into the three user-facing wallet states. Pending governs PENDING
// vs COMPLETE / FAILED; terminal states pick between COMPLETE and FAILED
// based on whether the run reached the happy path.
func statusFromSwapState(state swapclientrpc.SwapState,
	pending bool) walletrpc.EntryStatus {

	if pending {
		return walletrpc.EntryStatus_ENTRY_STATUS_PENDING
	}

	switch state {
	case swapclientrpc.SwapState_SWAP_STATE_COMPLETED,
		swapclientrpc.SwapState_SWAP_STATE_REFUNDED:
		return walletrpc.EntryStatus_ENTRY_STATUS_COMPLETE

	case swapclientrpc.SwapState_SWAP_STATE_FAILED,
		swapclientrpc.SwapState_SWAP_STATE_EXPIRED,
		swapclientrpc.SwapState_SWAP_STATE_NEEDS_INTERVENTION:
		return walletrpc.EntryStatus_ENTRY_STATUS_FAILED

	default:
		// Non-pending, non-terminal is an unexpected state. Surface
		// it as PENDING so the caller can poll for the next
		// transition rather than treating the row as terminal.
		return walletrpc.EntryStatus_ENTRY_STATUS_PENDING
	}
}

// failureReasonFromTerminal surfaces the SDK's terminal reason string only
// when it carries information. The wallet layer keeps the field empty for
// happy-path rows so callers can use !=" "" as a "did something go wrong"
// check.
func failureReasonFromTerminal(reason string) string {
	return strings.TrimSpace(reason)
}

// truncate clips s to at most n characters. The function preserves the
// prefix so a Lightning bech32 invoice still shows its network and amount
// segments in the rendered counterparty.
func truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}

	return s[:n]
}

// nowUnix returns the current unix-seconds timestamp. Hoisted so a future
// unit test can replace it via a build-tagged variable; the production path
// always uses time.Now.
func nowUnix() int64 {
	return time.Now().Unix()
}

// unixToTime converts an int64 unix-seconds timestamp into time.Time.
// Returns time.Now when the input is zero so callers always get a usable
// timestamp for pending tracking.
func unixToTime(ts int64) time.Time {
	if ts == 0 {
		return time.Now()
	}

	return time.Unix(ts, 0)
}

// leaveEntryStub builds the initial WalletEntry returned by Send when the
// caller targets an onchain destination. The id is populated with the first
// queued outpoint so the row is correlatable; downstream history merges fill
// in the broadcast txid once the leave registry produces one.
func leaveEntryStub(queuedOutpoints []string, destination string, amtSat int64,
	note string) *walletrpc.WalletEntry {

	id := ""
	if len(queuedOutpoints) > 0 {
		id = queuedOutpoints[0]
	}
	createdAt := nowUnix()

	return &walletrpc.WalletEntry{
		Id:            id,
		Kind:          walletrpc.EntryKind_ENTRY_KIND_EXIT,
		Status:        walletrpc.EntryStatus_ENTRY_STATUS_PENDING,
		AmountSat:     -amtSat,
		Counterparty:  truncate(destination, truncatedCounterpartyLen),
		CreatedAtUnix: createdAt,
		UpdatedAtUnix: createdAt,
		Note:          note,
	}
}
