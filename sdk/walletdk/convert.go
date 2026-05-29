package walletdk

import (
	"fmt"
	"time"

	"github.com/lightninglabs/darepo-client/rpc/walletdkrpc"
)

// listViewToProto maps the SDK ListView string onto the generated
// enum.
func listViewToProto(v ListView) (walletdkrpc.ListView, error) {
	switch v {
	case ListViewActivity, "":
		return walletdkrpc.ListView_LIST_VIEW_ACTIVITY, nil

	case ListViewVTXOs:
		return walletdkrpc.ListView_LIST_VIEW_VTXOS, nil

	case ListViewOnchain:
		return walletdkrpc.ListView_LIST_VIEW_ONCHAIN, nil

	default:
		return walletdkrpc.ListView_LIST_VIEW_UNSPECIFIED,
			fmt.Errorf("unknown list view %q "+
				"(activity|vtxos|onchain)", v)
	}
}

// listResultFromProto projects the typed oneof body onto the wrapper
// ListResult shape, populating exactly one variant.
func listResultFromProto(view ListView,
	resp *walletdkrpc.ListResponse) *ListResult {

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

func prepareSendResultFromProto(
	resp *walletdkrpc.PrepareSendResponse) *PrepareSendResult {

	if resp == nil {
		return &PrepareSendResult{}
	}

	return &PrepareSendResult{
		SendIntentID:            resp.GetSendIntentId(),
		AmountSat:               resp.GetAmountSat(),
		ExpectedFeeSat:          resp.GetExpectedFeeSat(),
		FeeKnown:                resp.GetFeeKnown(),
		ExpectedTotalOutflowSat: resp.GetExpectedTotalOutflowSat(),
		TotalOutflowKnown:       resp.GetTotalOutflowKnown(),
		Rail:                    sendRailFromProto(resp.GetRail()),
		QuoteStatus: quoteStatusFromProto(
			resp.GetQuoteStatus(),
		),
		DestinationSummary: resp.GetDestinationSummary(),
		InvoiceDescription: resp.GetInvoiceDescription(),
		PaymentHash:        resp.GetPaymentHash(),
		ExpiresAtUnix:      resp.GetExpiresAtUnix(),
		SelectedOutpoints: append(
			[]string(nil), resp.GetSelectedOutpoints()...,
		),
		Warning: resp.GetWarning(),
	}
}

func sendRailFromProto(rail walletdkrpc.SendRail) SendRail {
	switch rail {
	case walletdkrpc.SendRail_SEND_RAIL_OFFCHAIN_UNKNOWN:
		return SendRailOffchainUnknown

	case walletdkrpc.SendRail_SEND_RAIL_IN_ARK:
		return SendRailInArk

	case walletdkrpc.SendRail_SEND_RAIL_LIGHTNING:
		return SendRailLightning

	case walletdkrpc.SendRail_SEND_RAIL_ONCHAIN:
		return SendRailOnchain

	default:
		return SendRailUnspecified
	}
}

func quoteStatusFromProto(status walletdkrpc.SendQuoteStatus) SendQuoteStatus {
	switch status {
	case walletdkrpc.SendQuoteStatus_SEND_QUOTE_STATUS_COMPLETE:
		return SendQuoteStatusComplete

	case walletdkrpc.SendQuoteStatus_SEND_QUOTE_STATUS_LOCAL_ONLY:
		return SendQuoteStatusLocalOnly

	default:
		return SendQuoteStatusUnspecified
	}
}

// exitJobStatusFromProto maps the walletdkrpc ExitJobStatus enum onto the
// SDK string set.
func exitJobStatusFromProto(s walletdkrpc.ExitJobStatus) ExitJobStatus {
	switch s {
	case walletdkrpc.ExitJobStatus_EXIT_JOB_STATUS_PENDING:
		return ExitJobStatusPending

	case walletdkrpc.ExitJobStatus_EXIT_JOB_STATUS_MATERIALIZING:
		return ExitJobStatusMaterializing

	case walletdkrpc.ExitJobStatus_EXIT_JOB_STATUS_CSV_PENDING:
		return ExitJobStatusCSVPending

	case walletdkrpc.ExitJobStatus_EXIT_JOB_STATUS_SWEEPING:
		return ExitJobStatusSweeping

	case walletdkrpc.ExitJobStatus_EXIT_JOB_STATUS_COMPLETED:
		return ExitJobStatusCompleted

	case walletdkrpc.ExitJobStatus_EXIT_JOB_STATUS_FAILED:
		return ExitJobStatusFailed

	default:
		return ExitJobStatusUnspecified
	}
}

// entryFromProto copies one wallet RPC entry into wrapper-owned fields so UI
// and bridge callers do not need protobuf types.
func entryFromProto(entry *walletdkrpc.WalletEntry) Entry {
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
func entryKindFromProto(kind walletdkrpc.EntryKind) EntryKind {
	switch kind {
	case walletdkrpc.EntryKind_ENTRY_KIND_SEND:
		return EntryKindSend

	case walletdkrpc.EntryKind_ENTRY_KIND_RECV:
		return EntryKindReceive

	case walletdkrpc.EntryKind_ENTRY_KIND_DEPOSIT:
		return EntryKindDeposit

	case walletdkrpc.EntryKind_ENTRY_KIND_EXIT:
		return EntryKindExit

	default:
		return ""
	}
}

// entryKindToProto maps SDK filters back to the generated enum.
func entryKindToProto(kind EntryKind) (walletdkrpc.EntryKind, error) {
	switch kind {
	case EntryKindSend:
		return walletdkrpc.EntryKind_ENTRY_KIND_SEND, nil

	case EntryKindReceive:
		return walletdkrpc.EntryKind_ENTRY_KIND_RECV, nil

	case EntryKindDeposit:
		return walletdkrpc.EntryKind_ENTRY_KIND_DEPOSIT, nil

	case EntryKindExit:
		return walletdkrpc.EntryKind_ENTRY_KIND_EXIT, nil

	default:
		return walletdkrpc.EntryKind_ENTRY_KIND_UNSPECIFIED,
			fmt.Errorf("unknown entry kind %q "+
				"(send|receive|deposit|exit)", kind)
	}
}

// entryStatusFromProto maps generated statuses into stable display strings.
func entryStatusFromProto(status walletdkrpc.EntryStatus) EntryStatus {
	switch status {
	case walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING:
		return EntryStatusPending

	case walletdkrpc.EntryStatus_ENTRY_STATUS_COMPLETE:
		return EntryStatusComplete

	case walletdkrpc.EntryStatus_ENTRY_STATUS_FAILED:
		return EntryStatusFailed

	default:
		return ""
	}
}

// entryKindsToProto copies SDK filters into generated enum values.
func entryKindsToProto(kinds []EntryKind) ([]walletdkrpc.EntryKind, error) {
	if len(kinds) == 0 {
		return nil, nil
	}

	out := make([]walletdkrpc.EntryKind, 0, len(kinds))
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
func balanceFromProto(balance *walletdkrpc.BalanceResponse) Balance {
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
