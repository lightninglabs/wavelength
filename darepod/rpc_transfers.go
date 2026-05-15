package darepod

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/ledger"
)

const (
	// defaultListTransfersLimit is used when callers omit a transfer list
	// limit.
	defaultListTransfersLimit uint32 = 100

	// maxListTransfersLimit caps transfer list pages so a broad transfer
	// history request cannot force an unbounded response.
	maxListTransfersLimit uint32 = 1000

	// maxTransferSourceOffset mirrors the signed SQL offset bound enforced
	// by ListTransactions when transfer history pagination walks source
	// pages.
	maxTransferSourceOffset uint32 = math.MaxInt32

	// transactionHistoryStatusFailed mirrors the persisted transaction
	// history status copied into
	// TransactionHistoryEntry.confirmation_status.
	transactionHistoryStatusFailed = "failed"
)

// ListTransfers returns a user-facing status view for in-round and OOR
// transfers known to the daemon.
func (r *RPCServer) ListTransfers(ctx context.Context,
	req *daemonrpc.ListTransfersRequest) (*daemonrpc.ListTransfersResponse,
	error) {

	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}

	limit := req.GetLimit()
	switch {
	case limit == 0:
		limit = defaultListTransfersLimit

	case limit > maxListTransfersLimit:
		limit = maxListTransfersLimit
	}

	// The limit is applied after collection, filtering, and deduplication
	// so the returned page is globally newest-first across all sources.
	transfers, err := r.collectTransfers(ctx, req)
	if err != nil {
		return nil, err
	}

	sortTransfersNewestFirst(transfers)

	start := clampedTransferStart(req.GetOffset(), len(transfers))
	end := start + int(limit)
	hasMore := end < len(transfers)
	if end > len(transfers) {
		end = len(transfers)
	}

	resp := &daemonrpc.ListTransfersResponse{
		Transfers: transfers[start:end],
		HasMore:   hasMore,
	}
	if hasMore {
		resp.NextOffset = nextTransferOffset(req.GetOffset(), limit)
	}

	return resp, nil
}

// collectTransfers gathers transfers from existing status/history stores and
// applies shared request filters before caller-side sorting and pagination.
func (r *RPCServer) collectTransfers(ctx context.Context,
	req *daemonrpc.ListTransfersRequest) ([]*daemonrpc.TransferInfo,
	error) {

	var transfers []*daemonrpc.TransferInfo

	if transferModeAllowed(
		req, daemonrpc.TransferMode_TRANSFER_MODE_INROUND,
	) {

		if transferStatusAllowed(
			req, daemonrpc.TransferStatus_TRANSFER_STATUS_PENDING,
		) {

			// Round status and ledger history are separate
			// snapshots. A round that completes between reads can
			// briefly appear once as a pending round row and once
			// as a committed history row; the source-prefixed
			// transfer ids keep those rows distinguishable.
			pending, err := r.pendingRoundTransfers(ctx)
			if err != nil {
				return nil, err
			}

			transfers = append(transfers, pending...)
		}

		if transferHistoryStatusAllowed(req) {
			history, err := r.roundHistoryTransfers(ctx)
			if err != nil {
				return nil, err
			}

			transfers = append(transfers, history...)
		}
	}

	if transferModeAllowed(req, daemonrpc.TransferMode_TRANSFER_MODE_OOR) {
		oorTransfers, err := r.oorTransfers(ctx)
		if err != nil {
			return nil, err
		}

		transfers = append(transfers, oorTransfers...)
	}

	// The first implementation intentionally normalizes all matching local
	// sources in memory before sorting. Source-level mode checks avoid
	// unnecessary scans, while the final predicate keeps the full filter
	// logic in one place until the stores grow a shared pagination model.
	// A future shared transfer store can push limit/filter handling into
	// the source queries if O(total history) scans become too expensive.
	filtered := make([]*daemonrpc.TransferInfo, 0, len(transfers))
	for _, transfer := range transfers {
		if transferMatchesFilters(transfer, req) {
			filtered = append(filtered, transfer)
		}
	}

	return dedupeTransfers(filtered), nil
}

// pendingRoundTransfers converts non-terminal round FSM states into pending
// in-round transfer status rows.
func (r *RPCServer) pendingRoundTransfers(ctx context.Context) (
	[]*daemonrpc.TransferInfo, error) {

	liveRounds, err := r.queryRoundStates(ctx)
	if err != nil {
		return nil, err
	}

	var (
		transfers  []*daemonrpc.TransferInfo
		roundIndex int
	)
	for _, round := range liveRounds {
		status := transferStatusFromRoundState(round.GetState())
		if status != daemonrpc.TransferStatus_TRANSFER_STATUS_PENDING {
			continue
		}

		transfers = append(
			transfers, transferFromRound(round, status, roundIndex),
		)
		roundIndex++
	}

	var nextToken string
	for {
		// A pending round can exist in both live FSM state and
		// persisted round summaries. Rows with the same round id are
		// deduped after filtering; a temporary live row without a round
		// id can only be kept as a transient extra pending row, which
		// is preferable to hiding active local round state.
		// A round that is live and also persisted as pending appears
		// twice with the same round id; dedupeTransfers keeps the
		// first row when status and id tie.
		// TODO(darepo-client#451): Push pending-status filtering into
		// ListRounds once it supports a proto-level state filter so
		// broad transfer-list calls do not need to scan every
		// persisted page. The scan is exhaustive until then so
		// ListTransfers.HasMore reflects the merged result set rather
		// than a source-scan cap.
		roundResp, err := r.ListRounds(
			ctx, &daemonrpc.ListRoundsRequest{
				PageSize:      int32(maxListTransfersLimit),
				PageToken:     nextToken,
				PersistedOnly: true,
			},
		)
		if err != nil {
			return nil, err
		}

		for _, round := range roundResp.GetRounds() {
			status := transferStatusFromRoundState(round.GetState())
			if status != daemonrpc.
				TransferStatus_TRANSFER_STATUS_PENDING {

				// Terminal persisted rounds are shown through
				// transaction history, which has amount and
				// ledger context that the round summary does
				// not carry.
				continue
			}

			transfers = append(
				transfers, transferFromRound(
					round, status, roundIndex,
				),
			)
			roundIndex++
		}

		nextToken = roundResp.GetNextPageToken()
		if nextToken == "" {
			break
		}
	}

	return transfers, nil
}

// roundHistoryTransfers reads committed round transaction history rows and
// converts them into transfer status rows.
func (r *RPCServer) roundHistoryTransfers(ctx context.Context) (
	[]*daemonrpc.TransferInfo, error) {

	mode := daemonrpc.TransferMode_TRANSFER_MODE_INROUND
	var transfers []*daemonrpc.TransferInfo
	for offset := uint32(0); ; {
		resp, err := r.ListTransactions(
			ctx, &daemonrpc.ListTransactionsRequest{
				Limit:  maxListTransfersLimit,
				Offset: offset,
				Type:   ledger.TransactionTypeRound,
			},
		)
		if err != nil {
			return nil, err
		}

		for _, tx := range resp.GetTransactions() {
			transfers = append(
				transfers, transferFromTransaction(tx, mode),
			)
		}

		if !resp.GetHasMore() {
			break
		}

		nextOffset := resp.GetNextOffset()
		if nextOffset <= offset ||
			nextOffset > maxTransferSourceOffset {

			break
		}

		offset = nextOffset
	}

	return transfers, nil
}

// oorTransfers converts locally known OOR sessions into the shared transfer
// status row type.
func (r *RPCServer) oorTransfers(ctx context.Context) (
	[]*daemonrpc.TransferInfo, error) {

	sessions, err := r.listOORSessions(
		ctx, &daemonrpc.ListOORSessionsRequest{},
	)
	if err != nil {
		return nil, err
	}

	transfers := make([]*daemonrpc.TransferInfo, 0, len(sessions))
	for _, session := range sessions {
		transfers = append(transfers, transferFromOORSession(session))
	}

	return transfers, nil
}

// transferFromRound converts one round status snapshot into a transfer row.
func transferFromRound(round *daemonrpc.RoundInfo,
	status daemonrpc.TransferStatus,
	roundIndex int) *daemonrpc.TransferInfo {

	mode := daemonrpc.TransferMode_TRANSFER_MODE_INROUND
	direction := transferDirectionFromRound(round)

	id := round.GetRoundId()
	if id == "" {
		id = temporaryRoundTransferID(round, roundIndex)
	}

	return &daemonrpc.TransferInfo{
		TransferId:         "round:" + id,
		Mode:               mode,
		Direction:          direction,
		Status:             status,
		Phase:              round.GetState().String(),
		RoundId:            round.GetRoundId(),
		InputOutpoints:     round.GetInputOutpoints(),
		OutputOutpoints:    round.GetOutputOutpoints(),
		Txid:               round.GetCommitmentTxid(),
		ConfirmationHeight: round.GetCommitmentHeight(),
		CreatedAtUnixS:     round.GetCreationTime(),
		UpdatedAtUnixS:     round.GetLastUpdateTime(),
		FailureReason:      round.GetFailureReason(),
	}
}

// transferDirectionFromRound infers direction from locally known round
// outpoints when persisted round summaries expose them. Inputs take priority
// because a local round is treated as outgoing once it spends local VTXOs; any
// local outputs in that same summary are change-like context rather than the
// primary transfer direction.
func transferDirectionFromRound(
	round *daemonrpc.RoundInfo) daemonrpc.TransferDirection {

	switch {
	case len(round.GetInputOutpoints()) > 0:
		return daemonrpc.TransferDirection_TRANSFER_DIRECTION_OUTGOING

	case len(round.GetOutputOutpoints()) > 0:
		return daemonrpc.TransferDirection_TRANSFER_DIRECTION_INCOMING

	default:
		// Live in-memory rounds currently expose phase, but not the
		// local input/output ownership needed to classify direction.
		return daemonrpc.
			TransferDirection_TRANSFER_DIRECTION_UNSPECIFIED
	}
}

// temporaryRoundTransferID builds a best-effort response-local identifier for
// live round states that have not yet received a server-assigned round id.
// These temporary ids are not stable across daemon restarts.
func temporaryRoundTransferID(round *daemonrpc.RoundInfo,
	roundIndex int) string {

	parts := []string{
		fmt.Sprintf("is_temp:%v", round.GetIsTemp()),
		round.GetState().String(),
		fmt.Sprintf("created:%d", round.GetCreationTime()),
		fmt.Sprintf("updated:%d", round.GetLastUpdateTime()),
		"tx:" + round.GetCommitmentTxid(),
		"in:" + strings.Join(round.GetInputOutpoints(), ","),
		"out:" + strings.Join(round.GetOutputOutpoints(), ","),
	}

	digest := sha256.Sum256([]byte(strings.Join(parts, "\x00")))

	return fmt.Sprintf("is_temp:%v:%x:index:%d", round.GetIsTemp(),
		digest[:8], roundIndex)
}

// transferFromTransaction converts one committed transaction history row into
// a transfer row.
func transferFromTransaction(tx *daemonrpc.TransactionHistoryEntry,
	mode daemonrpc.TransferMode) *daemonrpc.TransferInfo {

	return &daemonrpc.TransferInfo{
		TransferId:         transactionTransferID(tx, mode),
		Mode:               mode,
		Direction:          transferDirectionFromTransaction(tx),
		Status:             transferStatusFromTransaction(tx),
		Phase:              tx.GetSubtype(),
		AmountSat:          tx.GetAmountSat(),
		RoundId:            hex.EncodeToString(tx.GetRoundId()),
		SessionId:          hex.EncodeToString(tx.GetSessionId()),
		Txid:               tx.GetTxid(),
		ConfirmationHeight: tx.GetConfirmationHeight(),
		CreatedAtUnixS:     tx.GetCreatedAtUnixS(),
		// Transaction history rows are immutable, so created time is
		// also the best available update time for sorting.
		UpdatedAtUnixS: tx.GetCreatedAtUnixS(),
	}
}

// transferStatusFromTransaction infers coarse transfer status from
// transaction-history lifecycle fields.
func transferStatusFromTransaction(
	tx *daemonrpc.TransactionHistoryEntry) daemonrpc.TransferStatus {

	status := strings.ToLower(tx.GetConfirmationStatus())
	if status == transactionHistoryStatusFailed {
		return daemonrpc.TransferStatus_TRANSFER_STATUS_FAILED
	}

	// Ledger-backed round transaction history currently records successful
	// VTXO movement as typed ledger events. Failed transfer lifecycle is
	// represented by confirmation_status rather than fuzzy subtype names.
	return daemonrpc.TransferStatus_TRANSFER_STATUS_COMPLETED
}

// transferFromOORSession converts one OOR session status into a transfer row.
func transferFromOORSession(
	session *daemonrpc.OORSessionInfo) *daemonrpc.TransferInfo {

	return &daemonrpc.TransferInfo{
		TransferId: "oor:" + session.GetSessionId(),
		Mode:       daemonrpc.TransferMode_TRANSFER_MODE_OOR,
		Direction: transferDirectionFromOOR(
			session.GetDirection(),
		),
		Status: transferStatusFromOOR(session.GetStatus()),
		Phase:  session.GetPhase(),
		// TODO: Populate AmountSat when OORSessionInfo or a
		// history-backed merge exposes the package amount. The live OOR
		// session source currently carries outpoints and status, but
		// not the value moved by the transfer.
		SessionId:       session.GetSessionId(),
		InputOutpoints:  session.GetConsumedOutpoints(),
		OutputOutpoints: session.GetCreatedOutpoints(),
		CreatedAtUnixS:  session.GetCreatedAt(),
		UpdatedAtUnixS:  session.GetUpdatedAt(),
		FailureReason:   session.GetFailureReason(),
	}
}

// transactionTransferID builds a stable local identifier for transaction
// history rows surfaced as transfer entries.
func transactionTransferID(tx *daemonrpc.TransactionHistoryEntry,
	mode daemonrpc.TransferMode) string {

	switch mode {
	case daemonrpc.TransferMode_TRANSFER_MODE_INROUND:
		roundID := hex.EncodeToString(tx.GetRoundId())
		if roundID != "" {
			return fmt.Sprintf("round:%s:%d", roundID,
				tx.GetEntryId())
		}

	case daemonrpc.TransferMode_TRANSFER_MODE_OOR:
		sessionID := hex.EncodeToString(tx.GetSessionId())
		if sessionID != "" {
			return fmt.Sprintf("oor:%s:%d", sessionID,
				tx.GetEntryId())
		}

	default:
		// Defensive fallback for future callers that pass a history row
		// without first choosing an in-round or OOR transfer mode. The
		// ledger prefix keeps that caller error visible in the response
		// identifier without making this pure conversion helper log.
	}

	return fmt.Sprintf("ledger:%d", tx.GetEntryId())
}

// transferDirectionFromTransaction infers local direction from ledger event
// names and accounts.
func transferDirectionFromTransaction(
	tx *daemonrpc.TransactionHistoryEntry) daemonrpc.TransferDirection {

	// These constants are the ledger package's canonical labels for the
	// VTXO transfer events and accounts emitted into transaction history.
	switch tx.GetSubtype() {
	case ledger.EventVTXOReceived:
		return daemonrpc.TransferDirection_TRANSFER_DIRECTION_INCOMING

	case ledger.EventVTXOSent:
		return daemonrpc.TransferDirection_TRANSFER_DIRECTION_OUTGOING
	}

	if tx.GetCreditAccount() == ledger.AccountTransfersIn {
		return daemonrpc.TransferDirection_TRANSFER_DIRECTION_INCOMING
	}

	if tx.GetDebitAccount() == ledger.AccountTransfersOut {
		return daemonrpc.TransferDirection_TRANSFER_DIRECTION_OUTGOING
	}

	return daemonrpc.
		TransferDirection_TRANSFER_DIRECTION_UNSPECIFIED
}

// transferDirectionFromOOR converts an OOR direction enum into the shared
// transfer direction enum.
func transferDirectionFromOOR(
	direction daemonrpc.OORSessionDirection) daemonrpc.TransferDirection {

	switch direction {
	case daemonrpc.OORSessionDirection_OOR_SESSION_DIRECTION_INCOMING:
		return daemonrpc.TransferDirection_TRANSFER_DIRECTION_INCOMING

	case daemonrpc.OORSessionDirection_OOR_SESSION_DIRECTION_OUTGOING:
		return daemonrpc.TransferDirection_TRANSFER_DIRECTION_OUTGOING

	default:
		return daemonrpc.
			TransferDirection_TRANSFER_DIRECTION_UNSPECIFIED
	}
}

// transferStatusFromOOR converts an OOR status enum into the shared transfer
// status enum.
func transferStatusFromOOR(
	status daemonrpc.OORSessionStatus) daemonrpc.TransferStatus {

	switch status {
	case daemonrpc.OORSessionStatus_OOR_SESSION_STATUS_PENDING:
		return daemonrpc.TransferStatus_TRANSFER_STATUS_PENDING

	case daemonrpc.OORSessionStatus_OOR_SESSION_STATUS_FAILED:
		return daemonrpc.TransferStatus_TRANSFER_STATUS_FAILED

	case daemonrpc.OORSessionStatus_OOR_SESSION_STATUS_COMPLETED:
		return daemonrpc.TransferStatus_TRANSFER_STATUS_COMPLETED

	default:
		// Preserve forward compatibility: new OOR statuses surface as
		// unspecified until this projection learns their terminality.
		return daemonrpc.TransferStatus_TRANSFER_STATUS_UNSPECIFIED
	}
}

// transferStatusFromRoundState converts a round FSM state into the shared
// transfer status enum.
func transferStatusFromRoundState(
	state daemonrpc.RoundState) daemonrpc.TransferStatus {

	switch state {
	case daemonrpc.RoundState_ROUND_STATE_CONFIRMED:
		return daemonrpc.TransferStatus_TRANSFER_STATUS_COMPLETED

	case daemonrpc.RoundState_ROUND_STATE_FAILED:
		return daemonrpc.TransferStatus_TRANSFER_STATUS_FAILED

	default:
		return daemonrpc.TransferStatus_TRANSFER_STATUS_PENDING
	}
}

// transferModeAllowed reports whether a source with mode should be included
// for the request's mode filter.
func transferModeAllowed(req *daemonrpc.ListTransfersRequest,
	mode daemonrpc.TransferMode) bool {

	return req.GetModeFilter() ==
		daemonrpc.TransferMode_TRANSFER_MODE_UNSPECIFIED ||
		req.GetModeFilter() == mode
}

// transferStatusAllowed reports whether a source with status should be
// included for the request's status filter.
func transferStatusAllowed(req *daemonrpc.ListTransfersRequest,
	status daemonrpc.TransferStatus) bool {

	return req.GetStatusFilter() ==
		daemonrpc.TransferStatus_TRANSFER_STATUS_UNSPECIFIED ||
		req.GetStatusFilter() == status
}

// transferHistoryStatusAllowed reports whether committed history rows can
// match the request's status filter.
func transferHistoryStatusAllowed(req *daemonrpc.ListTransfersRequest) bool {
	return transferStatusAllowed(
		req, daemonrpc.TransferStatus_TRANSFER_STATUS_COMPLETED,
	) || transferStatusAllowed(
		req, daemonrpc.TransferStatus_TRANSFER_STATUS_FAILED,
	)
}

// transferMatchesFilters reports whether a transfer survives the request's
// mode, direction, and status filters.
func transferMatchesFilters(transfer *daemonrpc.TransferInfo,
	req *daemonrpc.ListTransfersRequest) bool {

	if transfer == nil {
		return false
	}

	if req.GetModeFilter() !=
		daemonrpc.TransferMode_TRANSFER_MODE_UNSPECIFIED &&
		transfer.GetMode() != req.GetModeFilter() {
		return false
	}

	if req.GetDirectionFilter() !=
		daemonrpc.TransferDirection_TRANSFER_DIRECTION_UNSPECIFIED &&
		!transferDirectionMatchesFilter(
			transfer.GetDirection(), req.GetDirectionFilter(),
		) {
		return false
	}

	if req.GetStatusFilter() !=
		daemonrpc.TransferStatus_TRANSFER_STATUS_UNSPECIFIED &&
		transfer.GetStatus() != req.GetStatusFilter() {
		return false
	}

	return true
}

// transferDirectionMatchesFilter reports whether a known or unknown transfer
// direction should survive a requested direction filter.
func transferDirectionMatchesFilter(
	direction, filter daemonrpc.TransferDirection) bool {

	if direction == daemonrpc.
		TransferDirection_TRANSFER_DIRECTION_UNSPECIFIED {

		// Live in-round rows can be useful even before the local
		// input/output hints needed for direction classification exist.
		return true
	}

	return direction == filter
}

// dedupeTransfers collapses rows that describe the same logical transfer
// across live status and committed transaction-history snapshots.
func dedupeTransfers(
	transfers []*daemonrpc.TransferInfo) []*daemonrpc.TransferInfo {

	// TODO(darepo-client#451): If OOR transfer history rows are added,
	// dedupe OOR transfers by session id in the same pass.
	roundIndexes := make(map[string]int)
	deduped := make([]*daemonrpc.TransferInfo, 0, len(transfers))
	for _, transfer := range transfers {
		roundID := transfer.GetRoundId()
		if transfer.GetMode() !=
			daemonrpc.TransferMode_TRANSFER_MODE_INROUND ||
			roundID == "" {

			deduped = append(deduped, transfer)
			continue
		}

		index, ok := roundIndexes[roundID]
		if !ok {
			roundIndexes[roundID] = len(deduped)
			deduped = append(deduped, transfer)
			continue
		}

		if transferSupersedesRoundDuplicate(transfer, deduped[index]) {
			deduped[index] = transfer
		}
	}

	return deduped
}

// transferSupersedesRoundDuplicate reports whether a later duplicate round
// row carries more authoritative status or timing information.
func transferSupersedesRoundDuplicate(
	candidate, current *daemonrpc.TransferInfo) bool {

	candidateStatus := candidate.GetStatus()
	currentStatus := current.GetStatus()
	pending := daemonrpc.TransferStatus_TRANSFER_STATUS_PENDING
	if currentStatus == pending && candidateStatus != pending {
		return true
	}

	if currentStatus != pending && candidateStatus == pending {
		return false
	}

	candidateTime := transferSortTime(candidate)
	currentTime := transferSortTime(current)
	if candidateTime != currentTime {
		return candidateTime > currentTime
	}

	candidateEntryID, candidateOK := roundHistoryEntryID(candidate)
	currentEntryID, currentOK := roundHistoryEntryID(current)
	if candidateOK && currentOK && candidateEntryID != currentEntryID {
		return candidateEntryID > currentEntryID
	}

	// Fall back to the full id for temporary or future row formats that do
	// not carry a parseable numeric ledger suffix.
	return candidate.GetTransferId() > current.GetTransferId()
}

// roundHistoryEntryID extracts the numeric ledger entry suffix from a
// history-backed in-round transfer id.
func roundHistoryEntryID(transfer *daemonrpc.TransferInfo) (int64, bool) {
	id, ok := strings.CutPrefix(transfer.GetTransferId(), "round:")
	if !ok {
		return 0, false
	}

	_, suffix, ok := strings.Cut(id, ":")
	if !ok {
		return 0, false
	}

	entryID, err := strconv.ParseInt(suffix, 10, 64)
	if err != nil {
		return 0, false
	}

	return entryID, true
}

// clampedTransferStart converts a uint32 transfer-list offset into a safe
// slice start index without overflowing int on 32-bit platforms.
func clampedTransferStart(offset uint32, transferCount int) int {
	if uint64(offset) > uint64(transferCount) {
		return transferCount
	}

	return int(offset)
}

// nextTransferOffset advances a transfer-list offset, capping on uint32
// overflow so a response never wraps the pagination cursor.
func nextTransferOffset(offset, limit uint32) uint32 {
	if ^uint32(0)-offset < limit {
		return ^uint32(0)
	}

	return offset + limit
}

// sortTransfersNewestFirst orders transfer rows by their best known update
// time, breaking ties by stable transfer id.
func sortTransfersNewestFirst(transfers []*daemonrpc.TransferInfo) {
	sort.SliceStable(transfers, func(i, j int) bool {
		left := transferSortTime(transfers[i])
		right := transferSortTime(transfers[j])
		if left == right {
			return transfers[i].GetTransferId() >
				transfers[j].GetTransferId()
		}

		return left > right
	})
}

// transferSortTime returns the timestamp used for newest-first ordering.
func transferSortTime(transfer *daemonrpc.TransferInfo) int64 {
	if transfer.GetUpdatedAtUnixS() != 0 {
		return transfer.GetUpdatedAtUnixS()
	}

	return transfer.GetCreatedAtUnixS()
}
