package walletdk

import (
	"time"

	"github.com/lightninglabs/darepo-client/rpc/walletrpc"
)

// entryFromProto copies one wallet RPC entry into wrapper-owned fields so UI
// and bridge callers do not need protobuf types.
func entryFromProto(entry *walletrpc.WalletEntry) Entry {
	if entry == nil {
		return Entry{}
	}

	return Entry{
		ID:            entry.GetId(),
		Kind:          entryKindFromProto(entry.GetKind()),
		Status:        entryStatusFromProto(entry.GetStatus()),
		AmountSat:     entry.GetAmountSat(),
		FeeSat:        entry.GetFeeSat(),
		Counterparty:  entry.GetCounterparty(),
		CreatedAt:     unixTime(entry.GetCreatedAtUnix()),
		UpdatedAt:     unixTime(entry.GetUpdatedAtUnix()),
		Note:          entry.GetNote(),
		FailureReason: entry.GetFailureReason(),
	}
}

// entryKindFromProto maps the generated enum into walletdk's string-like kind.
func entryKindFromProto(kind walletrpc.EntryKind) EntryKind {
	switch kind {
	case walletrpc.EntryKind_ENTRY_KIND_SEND:
		return EntryKindSend

	case walletrpc.EntryKind_ENTRY_KIND_RECV:
		return EntryKindReceive

	case walletrpc.EntryKind_ENTRY_KIND_DEPOSIT:
		return EntryKindDeposit

	case walletrpc.EntryKind_ENTRY_KIND_EXIT:
		return EntryKindExit

	default:
		return ""
	}
}

// entryKindToProto maps SDK filters back to the generated enum.
func entryKindToProto(kind EntryKind) walletrpc.EntryKind {
	switch kind {
	case EntryKindSend:
		return walletrpc.EntryKind_ENTRY_KIND_SEND

	case EntryKindReceive:
		return walletrpc.EntryKind_ENTRY_KIND_RECV

	case EntryKindDeposit:
		return walletrpc.EntryKind_ENTRY_KIND_DEPOSIT

	case EntryKindExit:
		return walletrpc.EntryKind_ENTRY_KIND_EXIT

	default:
		return walletrpc.EntryKind_ENTRY_KIND_UNSPECIFIED
	}
}

// entryStatusFromProto maps generated statuses into stable display strings.
func entryStatusFromProto(status walletrpc.EntryStatus) EntryStatus {
	switch status {
	case walletrpc.EntryStatus_ENTRY_STATUS_PENDING:
		return EntryStatusPending

	case walletrpc.EntryStatus_ENTRY_STATUS_COMPLETE:
		return EntryStatusComplete

	case walletrpc.EntryStatus_ENTRY_STATUS_FAILED:
		return EntryStatusFailed

	default:
		return ""
	}
}

// entryKindsToProto copies SDK filters into generated enum values.
func entryKindsToProto(kinds []EntryKind) []walletrpc.EntryKind {
	if len(kinds) == 0 {
		return nil
	}

	out := make([]walletrpc.EntryKind, 0, len(kinds))
	for _, kind := range kinds {
		out = append(out, entryKindToProto(kind))
	}

	return out
}

// balanceFromProto copies a wallet RPC balance into SDK-owned fields.
func balanceFromProto(balance *walletrpc.BalanceResponse) Balance {
	if balance == nil {
		return Balance{}
	}

	return Balance{
		ConfirmedSat:              balance.GetConfirmedSat(),
		PendingInSat:              balance.GetPendingInSat(),
		PendingOutSat:             balance.GetPendingOutSat(),
		BoardingConfirmedSat:      balance.GetConfirmedSat(),
		BoardingUnconfirmedSat:    balance.GetPendingInSat(),
		VTXOBalanceSat:            balance.GetConfirmedSat(),
		TotalConfirmedSat:         balance.GetConfirmedSat(),
		OnchainWalletConfirmedSat: balance.GetConfirmedSat(),
	}
}

// swapSummaryFromEntry maps a wallet entry into the deprecated swap-shaped
// view used by the pre-walletrpc walletdk TUI.
func swapSummaryFromEntry(entry Entry) SwapSummary {
	summary := SwapSummary{
		PaymentHash:    entry.ID,
		State:          string(entry.Status),
		Pending:        entry.Status == EntryStatusPending,
		AmountSat:      entry.AmountSat,
		TerminalReason: entry.FailureReason,
		CreatedAt:      entry.CreatedAt,
		UpdatedAt:      entry.UpdatedAt,
	}
	if entry.FeeSat > 0 {
		summary.FeeSat = uint64(entry.FeeSat)
	}

	switch entry.Kind {
	case EntryKindSend:
		summary.Direction = SwapDirectionPay

	case EntryKindReceive:
		summary.Direction = SwapDirectionReceive

	case EntryKindDeposit, EntryKindExit, "":
	}

	return summary
}

// unixTime preserves unset timestamp fields as zero time values.
func unixTime(sec int64) time.Time {
	if sec == 0 {
		return time.Time{}
	}

	return time.Unix(sec, 0)
}
