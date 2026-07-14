//go:build walletdkrpc && swapruntime

package swapwallet

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/lightninglabs/wavelength/ledger"
	"github.com/lightninglabs/wavelength/rpc/swapclientrpc"
	"github.com/lightninglabs/wavelength/rpc/walletdkrpc"
	"github.com/lightninglabs/wavelength/waverpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// InspectionService implements the technical WalletInspectionService surface.
// It deliberately sits next to, not inside, the friendly WalletService so
// callers must opt into internal correlators and ledger details.
type InspectionService struct {
	walletdkrpc.UnimplementedWalletInspectionServiceServer

	deps    *Deps
	runtime *Runtime
	history *history
}

// newInspectionService wires the inspection RPC implementation to the wallet
// dependencies.
func newInspectionService(deps *Deps, runtime *Runtime) *InspectionService {
	return &InspectionService{
		deps:    deps,
		runtime: runtime,
		history: newHistory(deps, runtime),
	}
}

// InspectActivity returns a best-effort technical trace for one activity row.
func (s *InspectionService) InspectActivity(ctx context.Context,
	req *walletdkrpc.InspectActivityRequest) (
	*walletdkrpc.InspectActivityResponse, error) {

	if s == nil || s.deps == nil {
		return nil, ErrSwapBackendUnavailable
	}

	id := strings.TrimSpace(req.GetId())
	if id == "" {
		return nil, status.Error(
			codes.InvalidArgument, "activity id is required",
		)
	}

	entry, err := s.activityEntry(ctx, id)
	if err != nil {
		return nil, err
	}

	swaps, err := s.listSwaps(ctx)
	if err != nil {
		return nil, err
	}

	swap := findSwapByPaymentHash(swaps, id)

	ledgerLimit := req.GetLedgerLimit()
	ledgerRows, err := s.listLedgerRows(ctx, ledgerLimit)
	if err != nil {
		return nil, err
	}
	ledgerTruncated := ledgerLimit > 0 && uint32(len(ledgerRows)) >
		ledgerLimit
	if ledgerTruncated {
		ledgerRows = ledgerRows[:ledgerLimit]
	}

	traceRows := correlateLedgerRows(entry, swap, ledgerRows)
	hidden := hiddenOORLedgerRows(swaps, ledgerRows)
	ledgerTrace := ledgerTraceRows(traceRows, hidden)
	vtxoTrace := vtxoTraceRows(swap, ledgerTrace)

	resp := &walletdkrpc.InspectActivityResponse{
		Entry:      entry,
		Swap:       swapTraceFromSummary(swap),
		LedgerRows: ledgerTrace,
		Vtxos:      vtxoTrace,
		Notes:      inspectionNotes(vtxoTrace),
	}

	if ledgerTruncated {
		resp.Notes = append(
			resp.Notes, "Ledger scan reached the requested "+
				"limit; older correlated rows may be omitted.",
		)
	}

	return resp, nil
}

// activityEntry finds the friendly WalletEntry that the inspection report
// should explain.
func (s *InspectionService) activityEntry(ctx context.Context, id string) (
	*walletdkrpc.WalletEntry, error) {

	resp, err := s.history.List(ctx, &walletdkrpc.ListRequest{
		View:  walletdkrpc.ListView_LIST_VIEW_ACTIVITY,
		Limit: s.deps.resolveMaxListLimit(),
	})
	if err != nil {
		return nil, fmt.Errorf("list activity: %w", err)
	}

	for _, entry := range resp.GetActivity().GetEntries() {
		if entry.GetId() == id {
			return entry, nil
		}
	}

	return nil, status.Errorf(codes.NotFound, "activity entry %q not found",
		id)
}

// listSwaps returns the latest swap summaries used for correlation.
func (s *InspectionService) listSwaps(ctx context.Context) (
	[]*swapclientrpc.SwapSummary, error) {

	if s.deps.SwapService == nil {
		return nil, nil
	}

	resp, err := s.deps.SwapService.ListSwaps(
		ctx, &swapclientrpc.ListSwapsRequest{},
	)
	if err != nil {
		return nil, fmt.Errorf("list swaps: %w", err)
	}

	return resp.GetSwaps(), nil
}

// listLedgerRows returns ledger rows, fetching one extra row when a non-zero
// caller limit lets the response report truncation.
func (s *InspectionService) listLedgerRows(ctx context.Context, limit uint32) (
	[]*waverpc.TransactionHistoryEntry, error) {

	if s.deps.RPCServer == nil {
		return nil, nil
	}

	queryLimit := limit
	if queryLimit == 0 {
		queryLimit = s.deps.resolveMaxListLimit()
	} else if queryLimit < ^uint32(0) {
		queryLimit++
	}

	resp, err := s.deps.RPCServer.ListTransactions(
		ctx, &waverpc.ListTransactionsRequest{
			Limit: queryLimit,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("list transactions: %w", err)
	}

	return resp.GetTransactions(), nil
}

// findSwapByPaymentHash returns the swap summary with the requested payment
// hash.
func findSwapByPaymentHash(swaps []*swapclientrpc.SwapSummary,
	paymentHash string) *swapclientrpc.SwapSummary {

	for _, swap := range swaps {
		if swap.GetPaymentHash() == paymentHash {
			return swap
		}
	}

	return nil
}

type ledgerInspectionRow struct {
	row  *waverpc.TransactionHistoryEntry
	role string
}

// correlateLedgerRows finds ledger rows that directly match the activity entry
// or one of the swap execution sessions behind it.
func correlateLedgerRows(entry *walletdkrpc.WalletEntry,
	swap *swapclientrpc.SwapSummary,
	rows []*waverpc.TransactionHistoryEntry) []ledgerInspectionRow {

	out := make(map[string]ledgerInspectionRow)
	add := func(row *waverpc.TransactionHistoryEntry, role string) {
		if row == nil {
			return
		}

		key := ledgerTraceKey(row)
		if existing, ok := out[key]; ok && existing.role != "" {
			if inspectionRoleRank(existing.role) >=
				inspectionRoleRank(role) {
				return
			}
		}
		out[key] = ledgerInspectionRow{
			row:  row,
			role: role,
		}
	}

	for _, row := range rows {
		if ledgerRowMatchesEntry(row, entry) {
			add(row, "activity_row")
		}
	}

	if swap != nil {
		correlateSwapLedgerRows(swap, rows, add)
	}

	result := make([]ledgerInspectionRow, 0, len(out))
	for _, row := range out {
		result = append(result, row)
	}

	return result
}

// inspectionRoleRank lets precise execution roles replace broad correlation
// roles when the same ledger row is discovered through multiple paths.
func inspectionRoleRank(role string) int {
	switch role {
	case "activity_row":
		return 3

	case "spent_input", "change_output", "materialized_output":
		return 2

	case "vhtlc_tx", "swap_session":
		return 1

	default:
		return 0
	}
}

// ledgerTraceKey returns a stable de-duplication key for an inspection ledger
// row.
func ledgerTraceKey(row *waverpc.TransactionHistoryEntry) string {
	if row.GetEntryId() != 0 {
		return fmt.Sprintf("entry:%d", row.GetEntryId())
	}
	if row.GetTxid() != "" {
		return "txid:" + row.GetTxid()
	}

	return fmt.Sprintf("row:%s:%s:%d:%d", row.GetType(), row.GetSubtype(),
		row.GetAmountSat(), row.GetCreatedAtUnixS())
}

// ledgerRowMatchesEntry reports whether a ledger row is the durable source for
// the friendly activity entry.
func ledgerRowMatchesEntry(row *waverpc.TransactionHistoryEntry,
	entry *walletdkrpc.WalletEntry) bool {

	if row == nil || entry == nil {
		return false
	}

	if kind, _, ok := classifyLedgerRow(row); ok {
		id := ledgerActivityID(row, kind)
		if id != "" && id == entry.GetId() {
			return true
		}
	}

	if row.GetTxid() != "" && row.GetTxid() == entry.GetId() {
		return true
	}

	return fmt.Sprintf("ledger-%d", row.GetEntryId()) == entry.GetId()
}

// correlateSwapLedgerRows adds ledger rows that match known swap sessions or
// vHTLC transactions.
func correlateSwapLedgerRows(swap *swapclientrpc.SwapSummary,
	rows []*waverpc.TransactionHistoryEntry,
	add func(*waverpc.TransactionHistoryEntry, string)) {

	sessionIDs := map[string]struct{}{}
	for _, id := range []string{
		swap.GetFundingSessionId(),
		swap.GetClaimSessionId(),
		swap.GetRefundSessionId(),
	} {
		if id == "" {
			continue
		}
		sessionIDs[strings.ToLower(id)] = struct{}{}
	}

	vhtlcTxid := strings.Split(swap.GetVhtlcOutpoint(), ":")[0]

	for _, row := range rows {
		if vhtlcTxid != "" && row.GetTxid() == vhtlcTxid {
			add(row, "vhtlc_tx")
		}

		session, ok := oorSendSessionID(row)
		if ok {
			if _, matched := sessionIDs[session]; matched {
				add(row, "swap_session")
			}
		}

		session, _, ok = oorReceiveRef(row)
		if ok {
			if _, matched := sessionIDs[session]; matched {
				add(row, "swap_session")
			}
		}
	}

	correlateOORPairs(swap, rows, add)
}

// correlateOORPairs links OOR send and receive rows that represent swap
// funding, claim, refund, or change legs.
func correlateOORPairs(swap *swapclientrpc.SwapSummary,
	rows []*waverpc.TransactionHistoryEntry,
	add func(*waverpc.TransactionHistoryEntry, string)) {

	receivedBySession := make(map[string]int64)
	receiveRows := make(map[string][]*waverpc.TransactionHistoryEntry)
	for _, row := range rows {
		session, _, ok := oorReceiveRef(row)
		if !ok {
			continue
		}

		receivedBySession[session] += row.GetAmountSat()
		receiveRows[session] = append(receiveRows[session], row)
	}

	for _, row := range rows {
		session, ok := oorSendSessionID(row)
		if !ok {
			continue
		}

		received := receivedBySession[session]
		if received == 0 {
			continue
		}

		switch swap.GetDirection() {
		case swapclientrpc.SwapDirection_SWAP_DIRECTION_PAY:
			if row.GetAmountSat()-received != swap.GetAmountSat() {
				continue
			}

			add(row, "spent_input")
			for _, receive := range receiveRows[session] {
				add(receive, "change_output")
			}

		case swapclientrpc.SwapDirection_SWAP_DIRECTION_RECEIVE:
			if row.GetAmountSat() != received {
				continue
			}

			add(row, "claim_input")
			for _, receive := range receiveRows[session] {
				add(receive, "materialized_output")
			}
		}
	}
}

// hiddenOORLedgerRows returns the internal OOR ledger rows hidden from the
// friendly activity feed.
func hiddenOORLedgerRows(swaps []*swapclientrpc.SwapSummary,
	rows []*waverpc.TransactionHistoryEntry) map[int64]struct{} {

	return internalOORLedgerEntries(
		rows, swapOORCorrelationsFromSwaps(swaps),
	)
}

// ledgerTraceRows projects daemon transaction rows onto the inspection RPC
// shape.
func ledgerTraceRows(rows []ledgerInspectionRow,
	hidden map[int64]struct{}) []*walletdkrpc.ActivityLedgerTrace {

	out := make([]*walletdkrpc.ActivityLedgerTrace, 0, len(rows))
	for _, item := range rows {
		row := item.row
		_, isHidden := hidden[row.GetEntryId()]

		out = append(out, &walletdkrpc.ActivityLedgerTrace{
			Source:             row.GetSource(),
			Type:               row.GetType(),
			Subtype:            row.GetSubtype(),
			AmountSat:          row.GetAmountSat(),
			FeeSat:             row.GetFeeSat(),
			CreatedAtUnix:      row.GetCreatedAtUnixS(),
			ConfirmationStatus: row.GetConfirmationStatus(),
			Description:        row.GetDescription(),
			EntryId:            row.GetEntryId(),
			Txid:               row.GetTxid(),
			DebitAccount:       row.GetDebitAccount(),
			CreditAccount:      row.GetCreditAccount(),
			RoundId:            hex.EncodeToString(row.GetRoundId()),
			SessionId: hex.EncodeToString(
				row.GetSessionId(),
			),
			ConfirmationHeight: row.GetConfirmationHeight(),
			HiddenFromActivity: isHidden,
			Role:               item.role,
			OutputIndex:        row.GetOutputIndex(),
		})
	}

	return out
}

// vtxoTraceRows projects swap and ledger rows into a best-effort VTXO movement
// trace.
func vtxoTraceRows(swap *swapclientrpc.SwapSummary,
	ledgerRows []*walletdkrpc.ActivityLedgerTrace) []*walletdkrpc.ActivityVTXOTrace {

	var out []*walletdkrpc.ActivityVTXOTrace
	if swap != nil && swap.GetVhtlcOutpoint() != "" {
		amount := swap.GetVhtlcAmountSat()
		if amount == 0 {
			amount = swap.GetAmountSat()
		}

		out = append(out, &walletdkrpc.ActivityVTXOTrace{
			Id:        swap.GetVhtlcOutpoint(),
			AmountSat: amount,
			Role:      "vhtlc_output",
			Ours:      false,
			Source:    "swap",
		})
	}

	for _, row := range ledgerRows {
		switch row.GetSubtype() {
		case ledger.EventVTXOSent:
			out = append(out, &walletdkrpc.ActivityVTXOTrace{
				Id:        row.GetSessionId(),
				AmountSat: row.GetAmountSat(),
				Role: nonEmpty(
					row.GetRole(),
					"spent_input",
				),
				Ours:      true,
				Source:    "ledger",
				SessionId: row.GetSessionId(),
			})

		case ledger.EventVTXOReceived:
			session, index, ok := receiveRefFromLedgerTrace(row)
			if !ok {
				continue
			}

			out = append(out, &walletdkrpc.ActivityVTXOTrace{
				Id:        fmt.Sprintf("%s:%d", session, index),
				AmountSat: row.GetAmountSat(),
				Role: nonEmpty(
					row.GetRole(),
					"received_output",
				),
				Ours:        true,
				Source:      "ledger",
				SessionId:   session,
				OutputIndex: index,
			})
		}
	}

	return dedupeVTXOTrace(out)
}

// receiveRefFromLedgerTrace extracts an OOR receive reference from an
// inspection ledger trace row.
func receiveRefFromLedgerTrace(row *walletdkrpc.ActivityLedgerTrace) (string,
	uint32, bool) {

	if row == nil {
		return "", 0, false
	}
	if row.GetTxid() == "" || row.GetOutputIndex() < 0 {
		return "", 0, false
	}

	return row.GetTxid(), uint32(row.GetOutputIndex()), true
}

// inspectionNotes returns caveats that apply to the actual trace rows instead
// of showing a blanket warning on unrelated activity kinds.
func inspectionNotes(rows []*walletdkrpc.ActivityVTXOTrace) []string {
	for _, row := range rows {
		if row.GetSource() == "ledger" &&
			row.GetId() == row.GetSessionId() {
			return []string{
				"Some sent VTXO inputs are keyed by OOR session " +
					"rather than a full outpoint.",
			}
		}
	}

	return nil
}

// dedupeVTXOTrace removes duplicate VTXO trace rows while preserving order.
func dedupeVTXOTrace(
	rows []*walletdkrpc.ActivityVTXOTrace) []*walletdkrpc.ActivityVTXOTrace {

	seen := make(map[string]struct{})
	out := rows[:0]
	for _, row := range rows {
		key := row.GetSource() + "|" + row.GetRole() + "|" + row.GetId()
		if _, ok := seen[key]; ok {
			continue
		}

		seen[key] = struct{}{}
		out = append(out, row)
	}

	return out
}

// nonEmpty returns s when set, otherwise fallback.
func nonEmpty(s, fallback string) string {
	if s != "" {
		return s
	}

	return fallback
}

// swapTraceFromSummary projects a swap summary onto the inspection RPC shape.
func swapTraceFromSummary(
	swap *swapclientrpc.SwapSummary) *walletdkrpc.ActivitySwapTrace {

	if swap == nil {
		return nil
	}

	return &walletdkrpc.ActivitySwapTrace{
		PaymentHash:      swap.GetPaymentHash(),
		Direction:        swap.GetDirection().String(),
		State:            swap.GetState().String(),
		Pending:          swap.GetPending(),
		AmountSat:        swap.GetAmountSat(),
		FeeSat:           swap.GetFeeSat(),
		Invoice:          swap.GetInvoice(),
		VhtlcOutpoint:    swap.GetVhtlcOutpoint(),
		VhtlcAmountSat:   swap.GetVhtlcAmountSat(),
		FundingSessionId: swap.GetFundingSessionId(),
		ClaimSessionId:   swap.GetClaimSessionId(),
		RefundSessionId:  swap.GetRefundSessionId(),
		TerminalReason:   swap.GetTerminalReason(),
		CreatedAtUnix:    swap.GetCreatedAtUnix(),
		UpdatedAtUnix:    swap.GetUpdatedAtUnix(),
		DeadlineUnix:     swap.GetDeadlineUnix(),
		RefundLocktime:   swap.GetRefundLocktime(),
		SettlementType: swapSettlementTypeString(
			swap.GetSettlementType(),
		),
		SenderPubkey: swap.GetSenderPubkey(),
		Preimage:     swap.GetPreimage(),
	}
}

// swapSettlementTypeString returns the swap settlement enum name while keeping
// unknown older rows quiet on the inspection surface.
func swapSettlementTypeString(
	settlementType swapclientrpc.SwapSettlementType) string {

	if settlementType ==
		swapclientrpc.SwapSettlementType_SWAP_SETTLEMENT_TYPE_UNSPECIFIED {
		return ""
	}

	return settlementType.String()
}
