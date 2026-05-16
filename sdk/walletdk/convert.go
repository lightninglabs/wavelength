package walletdk

import (
	"fmt"
	"time"

	"github.com/lightninglabs/darepo-client/rpc/walletrpc"
)

// listViewToProto maps the SDK ListView string onto the generated
// enum.
func listViewToProto(v ListView) (walletrpc.ListView, error) {
	switch v {
	case ListViewActivity, "":
		return walletrpc.ListView_LIST_VIEW_ACTIVITY, nil

	case ListViewVTXOs:
		return walletrpc.ListView_LIST_VIEW_VTXOS, nil

	case ListViewOnchain:
		return walletrpc.ListView_LIST_VIEW_ONCHAIN, nil

	default:
		return walletrpc.ListView_LIST_VIEW_UNSPECIFIED,
			fmt.Errorf("unknown list view %q "+
				"(activity|vtxos|onchain)", v)
	}
}

// listResultFromProto projects the typed oneof body onto the wrapper
// ListResult shape, populating exactly one variant.
func listResultFromProto(view ListView,
	resp *walletrpc.ListResponse) *ListResult {

	out := &ListResult{View: view}

	switch view {
	case ListViewActivity:
		activity := resp.GetActivity()
		if activity == nil {
			out.Activity = &ActivityList{}

			return out
		}
		entries := make([]Entry, 0, len(activity.GetEntries()))
		for _, e := range activity.GetEntries() {
			entries = append(entries, entryFromProto(e))
		}
		out.Activity = &ActivityList{
			Entries: entries,
			Total:   activity.GetTotal(),
		}

	case ListViewVTXOs:
		inv := resp.GetVtxos()
		if inv == nil {
			out.VTXOs = &VTXOInventory{}

			return out
		}
		vtxos := make([]WalletVTXO, 0, len(inv.GetVtxos()))
		for _, v := range inv.GetVtxos() {
			vtxos = append(vtxos, WalletVTXO{
				Outpoint:       v.GetOutpoint(),
				AmountSat:      v.GetAmountSat(),
				Status:         v.GetStatus(),
				BatchExpiry:    v.GetBatchExpiry(),
				RelativeExpiry: v.GetRelativeExpiry(),
				CommitmentTxid: v.GetCommitmentTxid(),
			})
		}
		out.VTXOs = &VTXOInventory{
			VTXOs: vtxos,
			Total: inv.GetTotal(),
		}

	case ListViewOnchain:
		hist := resp.GetOnchain()
		if hist == nil {
			out.Onchain = &OnchainHistory{}

			return out
		}
		txs := make([]OnchainTx, 0, len(hist.GetTxs()))
		for _, t := range hist.GetTxs() {
			txs = append(txs, OnchainTx{
				Txid:               t.GetTxid(),
				Kind:               t.GetKind(),
				AmountSat:          t.GetAmountSat(),
				FeeSat:             t.GetFeeSat(),
				Status:             t.GetStatus(),
				ConfirmationHeight: t.GetConfirmationHeight(),
				CreatedAt: unixTime(
					t.GetCreatedAtUnix(),
				),
				Description: t.GetDescription(),
			})
		}
		out.Onchain = &OnchainHistory{
			Txs:     txs,
			Total:   hist.GetTotal(),
			HasMore: hist.GetHasMore(),
		}
	}

	return out
}

// exitJobStatusFromProto maps the walletrpc ExitJobStatus enum onto the
// SDK string set.
func exitJobStatusFromProto(s walletrpc.ExitJobStatus) ExitJobStatus {
	switch s {
	case walletrpc.ExitJobStatus_EXIT_JOB_STATUS_PENDING:
		return ExitJobStatusPending

	case walletrpc.ExitJobStatus_EXIT_JOB_STATUS_MATERIALIZING:
		return ExitJobStatusMaterializing

	case walletrpc.ExitJobStatus_EXIT_JOB_STATUS_CSV_PENDING:
		return ExitJobStatusCSVPending

	case walletrpc.ExitJobStatus_EXIT_JOB_STATUS_SWEEPING:
		return ExitJobStatusSweeping

	case walletrpc.ExitJobStatus_EXIT_JOB_STATUS_COMPLETED:
		return ExitJobStatusCompleted

	case walletrpc.ExitJobStatus_EXIT_JOB_STATUS_FAILED:
		return ExitJobStatusFailed

	default:
		return ExitJobStatusUnspecified
	}
}

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
func entryKindToProto(kind EntryKind) (walletrpc.EntryKind, error) {
	switch kind {
	case EntryKindSend:
		return walletrpc.EntryKind_ENTRY_KIND_SEND, nil

	case EntryKindReceive:
		return walletrpc.EntryKind_ENTRY_KIND_RECV, nil

	case EntryKindDeposit:
		return walletrpc.EntryKind_ENTRY_KIND_DEPOSIT, nil

	case EntryKindExit:
		return walletrpc.EntryKind_ENTRY_KIND_EXIT, nil

	default:
		return walletrpc.EntryKind_ENTRY_KIND_UNSPECIFIED,
			fmt.Errorf("unknown entry kind %q "+
				"(send|receive|deposit|exit)", kind)
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
func entryKindsToProto(kinds []EntryKind) ([]walletrpc.EntryKind, error) {
	if len(kinds) == 0 {
		return nil, nil
	}

	out := make([]walletrpc.EntryKind, 0, len(kinds))
	for _, kind := range kinds {
		protoKind, err := entryKindToProto(kind)
		if err != nil {
			return nil, err
		}
		out = append(out, protoKind)
	}

	return out, nil
}

// balanceFromProto copies a wallet RPC balance into SDK-owned fields.
func balanceFromProto(balance *walletrpc.BalanceResponse) Balance {
	if balance == nil {
		return Balance{}
	}

	return Balance{
		ConfirmedSat:  balance.GetConfirmedSat(),
		PendingInSat:  balance.GetPendingInSat(),
		PendingOutSat: balance.GetPendingOutSat(),
	}
}

// unixTime preserves unset timestamp fields as zero time values.
func unixTime(sec int64) time.Time {
	if sec == 0 {
		return time.Time{}
	}

	return time.Unix(sec, 0)
}
