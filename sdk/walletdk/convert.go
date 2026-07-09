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
			Entries:    entries,
			Total:      activity.GetTotal(),
			HasMore:    activity.GetHasMore(),
			NextCursor: activity.GetNextCursor(),
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
		Warning:       resp.GetWarning(),
		CreditPreview: creditPreviewFromProto(resp.GetCreditPreview()),
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

	case walletdkrpc.SendRail_SEND_RAIL_CREDIT:
		return SendRailCredit

	case walletdkrpc.SendRail_SEND_RAIL_MIXED:
		return SendRailMixed

	default:
		return SendRailUnspecified
	}
}

func creditPreviewFromProto(preview *walletdkrpc.CreditPreview) *CreditPreview {
	if preview == nil {
		return nil
	}

	return &CreditPreview{
		MustUseCredit:      preview.GetMustUseCredit(),
		CreditAppliedSat:   preview.GetCreditAppliedSat(),
		CreditShortfallSat: preview.GetCreditShortfallSat(),
		CreditTopupSat:     preview.GetCreditTopupSat(),
		ArkFundingSat:      preview.GetArkFundingSat(),
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

// exitStatusResultFromProto projects the walletdkrpc ExitStatus response onto
// its SDK DTO, mapping the optional progress, CSV, and fee sub-messages to nil
// pointers when the daemon did not populate them (a coarse query).
func exitStatusResultFromProto(
	resp *walletdkrpc.ExitStatusResponse) *ExitStatusResult {

	if resp == nil {
		return &ExitStatusResult{}
	}

	return &ExitStatusResult{
		Found: resp.GetFound(),
		Status: exitJobStatusFromProto(
			resp.GetStatus(),
		),
		SweepTxid:   resp.GetSweepTxid(),
		LastError:   resp.GetLastError(),
		PhaseDetail: resp.GetPhaseDetail(),
		Progress: exitProgressFromProto(
			resp.GetProgress(),
		),
		CSV:                     exitCSVFromProto(resp.GetCsv()),
		Fees:                    exitFeesFromProto(resp.GetFees()),
		BestCaseBlocksRemaining: resp.GetBestCaseBlocksRemaining(),
		CurrentHeight:           resp.GetCurrentHeight(),
	}
}

// exitProgressFromProto projects the proto progress message, returning nil when
// absent so callers nil-check rather than reading a zeroed struct.
func exitProgressFromProto(p *walletdkrpc.ExitProgress) *ExitProgress {
	if p == nil {
		return nil
	}

	return &ExitProgress{
		ConfirmedTxs:      p.GetConfirmedTxs(),
		InFlightTxs:       p.GetInFlightTxs(),
		ReadyTxs:          p.GetReadyTxs(),
		BlockedTxs:        p.GetBlockedTxs(),
		TotalTxs:          p.GetTotalTxs(),
		CurrentLayer:      p.GetCurrentLayer(),
		TotalLayers:       p.GetTotalLayers(),
		TargetConfirmed:   p.GetTargetConfirmed(),
		AllProofConfirmed: p.GetAllProofConfirmed(),
	}
}

// exitCSVFromProto projects the proto CSV countdown, returning nil until the
// target confirms.
func exitCSVFromProto(c *walletdkrpc.ExitCSV) *ExitCSV {
	if c == nil {
		return nil
	}

	return &ExitCSV{
		TargetConfirmHeight: c.GetTargetConfirmHeight(),
		MaturityHeight:      c.GetMaturityHeight(),
		BlocksRemaining:     c.GetBlocksRemaining(),
		Mature:              c.GetMature(),
	}
}

// exitFeesFromProto projects the proto fee breakdown, returning nil when the
// daemon did not populate it.
func exitFeesFromProto(f *walletdkrpc.ExitFees) *ExitFees {
	if f == nil {
		return nil
	}

	return &ExitFees{
		CPFPFeeSat:      f.GetCpfpFeeSat(),
		SweepFeeSat:     f.GetSweepFeeSat(),
		TotalCostSat:    f.GetTotalCostSat(),
		SpentSoFarSat:   f.GetSpentSoFarSat(),
		VTXOAmountSat:   f.GetVtxoAmountSat(),
		NetRecoveredSat: f.GetNetRecoveredSat(),
		FeeRateSatVByte: f.GetFeeRateSatVbyte(),
		SweepFeeActual:  f.GetSweepFeeActual(),
	}
}

// exitSummaryFromProto projects the walletdkrpc ExitSummary response onto its
// SDK DTO.
func exitSummaryFromProto(
	resp *walletdkrpc.ExitSummaryResponse) *ExitSummaryResult {

	if resp == nil {
		return &ExitSummaryResult{}
	}

	exits := make([]ExitSummaryEntry, 0, len(resp.GetExits()))
	for _, e := range resp.GetExits() {
		exits = append(exits, ExitSummaryEntry{
			Outpoint: e.GetOutpoint(),
			Status: exitJobStatusFromProto(
				e.GetStatus(),
			),
			VTXOAmountSat:      e.GetVtxoAmountSat(),
			EstTotalFeeSat:     e.GetEstTotalFeeSat(),
			EstNetRecoveredSat: e.GetEstNetRecoveredSat(),
		})
	}

	return &ExitSummaryResult{
		Exits:                   exits,
		TotalExits:              resp.GetTotalExits(),
		TotalVTXOAmountSat:      resp.GetTotalVtxoAmountSat(),
		TotalEstFeeSat:          resp.GetTotalEstFeeSat(),
		TotalEstNetRecoveredSat: resp.GetTotalEstNetRecoveredSat(),
	}
}

func exitPlanFromProto(r *walletdkrpc.GetExitPlanResponse) *GetExitPlanResult {
	if r == nil {
		return &GetExitPlanResult{}
	}

	plans := make([]ExitPlanEntry, 0, len(r.GetPlans()))
	for _, entry := range r.GetPlans() {
		plans = append(plans, exitPlanEntryFromProto(entry))
	}

	return &GetExitPlanResult{
		Plans:                      plans,
		FeeRateSatPerVByte:         r.GetFeeRateSatPerVbyte(),
		CanStart:                   r.GetCanStart(),
		TotalFundingShortfallSat:   r.GetTotalFundingShortfallSat(),
		TotalRecommendedFundingSat: r.GetTotalRecommendedFundingSat(),
	}
}

// exitPlanEntryFromProto projects one proto ExitPlanEntry into its SDK DTO.
func exitPlanEntryFromProto(e *walletdkrpc.ExitPlanEntry) ExitPlanEntry {
	return ExitPlanEntry{
		Outpoint:                   e.GetOutpoint(),
		FundingAddress:             e.GetFundingAddress(),
		RequiredConfirmations:      e.GetRequiredConfirmations(),
		RequiredFeeUTXOCount:       e.GetRequiredFeeUtxoCount(),
		UsableFeeUTXOCount:         e.GetUsableFeeUtxoCount(),
		RecommendedUTXOAmountSat:   e.GetRecommendedUtxoAmountSat(),
		RecommendedTotalFundingSat: e.GetRecommendedTotalFundingSat(),
		FundingShortfallSat:        e.GetFundingShortfallSat(),
		CanStart:                   e.GetCanStart(),
		ExitJobFound:               e.GetExitJobFound(),
		ExitStatus: exitJobStatusFromProto(
			e.GetExitStatus(),
		),
		SweepTxid: e.GetSweepTxid(),
		LastError: e.GetLastError(),
		Err:       e.GetError(),
	}
}

func sweepWalletFromProto(
	resp *walletdkrpc.SweepWalletResponse) *SweepWalletResult {

	if resp == nil {
		return &SweepWalletResult{}
	}

	inputs := make([]WalletSweepInput, 0, len(resp.GetInputs()))
	for _, input := range resp.GetInputs() {
		inputs = append(inputs, WalletSweepInput{
			Outpoint:  input.GetOutpoint(),
			AmountSat: input.GetAmountSat(),
		})
	}

	return &SweepWalletResult{
		Inputs:             inputs,
		TotalInputSat:      resp.GetTotalInputSat(),
		EstimatedFeeSat:    resp.GetEstimatedFeeSat(),
		NetAmountSat:       resp.GetNetAmountSat(),
		FeeRateSatPerVByte: resp.GetFeeRateSatPerVbyte(),
		CanBroadcast:       resp.GetCanBroadcast(),
		Txid:               resp.GetTxid(),
		FailureReason:      resp.GetFailureReason(),
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
		Progress:      entryProgressFromProto(entry.GetProgress()),
		Request:       entryRequestFromProto(entry.GetRequest()),
		FailureCode: entryFailureCodeFromProto(
			entry.GetFailureCode(),
		),
	}
}

// entryProgressFromProto copies the daemon-normalized lifecycle metadata into
// wrapper-owned fields. It returns nil when the entry carried no progress, so
// callers can treat absence and emptiness uniformly.
func entryProgressFromProto(
	progress *walletdkrpc.WalletEntryProgress) *EntryProgress {

	if progress == nil {
		return nil
	}

	return &EntryProgress{
		Phase:              entryPhaseFromProto(progress.GetPhase()),
		PhaseLabel:         progress.GetPhaseLabel(),
		PaymentHash:        progress.GetPaymentHash(),
		Txid:               progress.GetTxid(),
		ConfirmationHeight: progress.GetConfirmationHeight(),
		VTXOOutpoint:       progress.GetVtxoOutpoint(),
		Preimage:           progress.GetPreimage(),
	}
}

// entryPhaseFromProto maps the generated lifecycle enum into walletdk's
// string-like phase, decoupled from proto enum renumbering.
func entryPhaseFromProto(phase walletdkrpc.WalletEntryPhase) EntryPhase {
	switch phase {
	case walletdkrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_REQUEST_CREATED:
		return EntryPhaseRequestCreated

	case walletdkrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_WAITING_FOR_PAYMENT: //nolint:ll
		return EntryPhaseWaitingForPayment

	case walletdkrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_PAYMENT_DETECTED:
		return EntryPhasePaymentDetected

	case walletdkrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_SETTLING:
		return EntryPhaseSettling

	case walletdkrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_CONFIRMED:
		return EntryPhaseConfirmed

	case walletdkrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_REFUNDING:
		return EntryPhaseRefunding

	case walletdkrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_REFUNDED:
		return EntryPhaseRefunded

	case walletdkrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_FAILED:
		return EntryPhaseFailed

	case walletdkrpc.WalletEntryPhase_WALLET_ENTRY_PHASE_WAITING_FOR_CONFIRMATION: //nolint:ll
		return EntryPhaseWaitingForConfirmation

	default:
		return EntryPhaseUnspecified
	}
}

// entryRequestFromProto flattens the request oneof into a wrapper-owned shape
// with a Type discriminator. It returns nil when no request was carried or no
// variant is set, so callers can treat absence and emptiness uniformly.
func entryRequestFromProto(
	request *walletdkrpc.WalletEntryRequest) *EntryRequest {

	if request == nil {
		return nil
	}

	switch req := request.GetRequest().(type) {
	case *walletdkrpc.WalletEntryRequest_LightningInvoice:
		return &EntryRequest{
			Type:             EntryRequestTypeLightning,
			LightningInvoice: req.LightningInvoice.GetInvoice(),
			PaymentHash:      req.LightningInvoice.GetPaymentHash(),
		}

	case *walletdkrpc.WalletEntryRequest_OnchainAddress:
		return &EntryRequest{
			Type:           EntryRequestTypeOnchain,
			OnchainAddress: req.OnchainAddress.GetAddress(),
		}

	case *walletdkrpc.WalletEntryRequest_ArkAddress:
		return &EntryRequest{
			Type:       EntryRequestTypeArk,
			ArkAddress: req.ArkAddress.GetAddress(),
		}

	default:
		return nil
	}
}

// entryFailureCodeFromProto maps the generated failure-code enum into
// walletdk's string-like code, decoupled from proto enum renumbering.
func entryFailureCodeFromProto(
	code walletdkrpc.EntryFailureCode) EntryFailureCode {

	switch code {
	case walletdkrpc.EntryFailureCode_ENTRY_FAILURE_CODE_TIMED_OUT:
		return EntryFailureCodeTimedOut

	case walletdkrpc.EntryFailureCode_ENTRY_FAILURE_CODE_EXPIRED:
		return EntryFailureCodeExpired

	case walletdkrpc.EntryFailureCode_ENTRY_FAILURE_CODE_REFUNDED:
		return EntryFailureCodeRefunded

	case walletdkrpc.EntryFailureCode_ENTRY_FAILURE_CODE_NEEDS_INTERVENTION:
		return EntryFailureCodeNeedsIntervention

	case walletdkrpc.EntryFailureCode_ENTRY_FAILURE_CODE_FAILED:
		return EntryFailureCodeFailed

	// An absent (presence-tracked) or unrecognized code is not a failure;
	// surface the empty string, mirroring FailureReason, rather than a
	// sentinel a non-failed entry would carry.
	default:
		return ""
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
		ConfirmedSat:       balance.GetConfirmedSat(),
		PendingInSat:       balance.GetPendingInSat(),
		PendingOutSat:      balance.GetPendingOutSat(),
		CreditAvailableSat: balance.GetCreditAvailableSat(),
		CreditReservedSat:  balance.GetCreditReservedSat(),
	}
}

// unixTime preserves unset timestamp fields as zero time values.
func unixTime(sec int64) time.Time {
	if sec == 0 {
		return time.Time{}
	}

	return time.Unix(sec, 0)
}
