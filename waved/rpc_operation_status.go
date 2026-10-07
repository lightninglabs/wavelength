package waved

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/baselib/actor"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/oor"
	"github.com/lightninglabs/wavelength/round"
	"github.com/lightninglabs/wavelength/waverpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// defaultListOORSessionsPageSize is used when callers omit page_size.
	defaultListOORSessionsPageSize = 100
)

// GetRound returns one live or persisted round status entry by round id.
func (r *RPCServer) GetRound(ctx context.Context,
	req *waverpc.GetRoundRequest) (*waverpc.GetRoundResponse, error) {

	if req == nil {
		return nil, status.Error(
			codes.InvalidArgument, "request must be provided",
		)
	}

	if req.RoundId == "" {
		return nil, status.Error(
			codes.InvalidArgument, "round_id must be provided",
		)
	}

	if r.server.actorSystem != nil {
		live, err := r.queryRoundStates(ctx)
		if err != nil {
			return nil, err
		}

		for _, info := range live {
			if info.GetRoundId() == req.RoundId {
				return &waverpc.GetRoundResponse{
					Round: info,
				}, nil
			}
		}
	}

	if r.server.roundStore == nil {
		return nil, status.Error(codes.NotFound, "round not found")
	}

	summary, err := r.server.roundStore.GetRoundSummary(ctx, req.RoundId)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Error(
				codes.NotFound, "round not found",
			)
		}

		return nil, status.Errorf(codes.Internal, "failed to get "+
			"persisted round: %v", err)
	}

	return &waverpc.GetRoundResponse{
		Round: roundSummaryToProto(summary),
	}, nil
}

// ListOORSessions returns locally known OOR operation status entries.
func (r *RPCServer) ListOORSessions(ctx context.Context,
	req *waverpc.ListOORSessionsRequest) (*waverpc.ListOORSessionsResponse,
	error) {

	if req == nil {
		req = &waverpc.ListOORSessionsRequest{}
	}

	pageSize := req.PageSize
	if pageSize <= 0 {
		pageSize = defaultListOORSessionsPageSize
	}

	sessions, err := r.listOORSessions(ctx, req)
	if err != nil {
		return nil, err
	}

	page, nextToken := pageOORSessions(sessions, pageSize)

	return &waverpc.ListOORSessionsResponse{
		Sessions:      page,
		NextPageToken: nextToken,
	}, nil
}

// GetOORSession returns one locally known OOR operation status entry.
func (r *RPCServer) GetOORSession(ctx context.Context,
	req *waverpc.GetOORSessionRequest) (*waverpc.GetOORSessionResponse,
	error) {

	if req == nil {
		return nil, status.Error(
			codes.InvalidArgument, "request must be provided",
		)
	}

	if req.SessionId == "" {
		return nil, status.Error(
			codes.InvalidArgument, "session_id must be provided",
		)
	}

	sessionID, err := parseOORSessionID(req.SessionId)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}

	store := r.newOORStatusStore()
	if store == nil {
		return nil, status.Error(
			codes.NotFound, "OOR session not found",
		)
	}
	summary, err := store.Get(ctx, sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, status.Error(
			codes.NotFound, "OOR session not found",
		)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get OOR "+
			"session: %v", err)
	}

	return &waverpc.GetOORSessionResponse{
		Session: r.oorStatusToProto(ctx, summary),
	}, nil
}

// newOORStatusStore reads the same durable registry state the OOR actor uses
// for summaries, together with authoritative package metadata.
func (r *RPCServer) newOORStatusStore() *db.OORStatusStore {
	if r.server.db == nil {
		return nil
	}

	return db.NewOORStatusStore(
		db.NewStore(
			r.server.db.DB, r.server.db.Queries,
			r.server.db.Backend(), r.server.log,
		),
	)
}

// listOORSessions selects one merged, filtered page plus a lookahead row.
// It never asks the actor to scan retained terminal snapshots.
func (r *RPCServer) listOORSessions(ctx context.Context,
	req *waverpc.ListOORSessionsRequest) ([]*waverpc.OORSessionInfo,
	error) {

	cursor, err := decodeOORStatusCursor(req.GetPageToken())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid "+
			"page_token: %v; restart pagination with an empty "+
			"page_token", err)
	}
	store := r.newOORStatusStore()
	if store == nil {
		return nil, nil
	}
	direction := db.OORSessionDirection(req.GetDirectionFilter())
	statusFilter := int32(req.GetStatusFilter()) - 1
	pageSize := req.GetPageSize()
	if pageSize <= 0 {
		pageSize = defaultListOORSessionsPageSize
	}
	summaries, err := store.List(
		ctx, cursor, direction, statusFilter, int64(pageSize)+1,
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to list OOR "+
			"sessions: %v", err)
	}
	sessions := make([]*waverpc.OORSessionInfo, 0, len(summaries))
	for i := range summaries {
		sessions = append(
			sessions, r.oorStatusToProto(ctx, &summaries[i]),
		)
	}

	return sessions, nil
}

// oorStatusToProto projects metadata and only the selected session's details.
// Persisted artifacts override the registry's direction, phase and status.
func (r *RPCServer) oorStatusToProto(ctx context.Context,
	summary *db.OORStatusSummary) *waverpc.OORSessionInfo {

	row := summary.Metadata
	info := &waverpc.OORSessionInfo{
		SessionId:     chainhash.Hash(row.SessionID).String(),
		Direction:     waverpc.OORSessionDirection(row.Direction),
		Status:        waverpc.OORSessionStatus(row.Status + 1),
		Phase:         row.Phase,
		FailureReason: row.LastError,
		CreatedAt:     row.CreatedAt,
		UpdatedAt:     row.UpdatedAt,
		ConsumedOutpoints: outpointsToStrings(
			summary.ConsumedOutpoints,
		),
		CreatedOutpoints: outpointsToStrings(summary.CreatedOutpoints),
	}
	info.FailedBeforePonr = r.outgoingFailedBeforePONR(ctx, info)
	if summary.Registry != nil {
		live := oor.SessionSummary{
			RetryReason: summary.Registry.LastError,
		}
		// Keep the existing coarse status fallback for malformed
		// diagnostic snapshots; they do not invalidate a persisted
		// completion.
		if err := oor.FillOutgoingSummary(
			&live, summary.Registry,
		); err != nil {

			r.server.log.WarnS(
				ctx,
				"Failed to decode outgoing snapshot for listing",
				err,
				slog.String("session_id", info.SessionId),
			)
		}
		if len(info.ConsumedOutpoints) == 0 {
			info.ConsumedOutpoints = outpointsToStrings(
				live.InputOutpoints,
			)
		}
		if row.HasPackage == 0 {
			info.FailureReason = live.RetryReason
		}
	}

	return info
}

// outgoingFailedBeforePONR reports whether a failed outgoing session recorded
// that it failed before the point of no return. It reads the registry row
// directly because the merged status projection does not always carry it, and
// reports false whenever the origin cannot be proven.
func (r *RPCServer) outgoingFailedBeforePONR(ctx context.Context,
	info *waverpc.OORSessionInfo) bool {

	failed := waverpc.OORSessionStatus_OOR_SESSION_STATUS_FAILED
	outgoing := waverpc.OORSessionDirection_OOR_SESSION_DIRECTION_OUTGOING
	if info.Status != failed || info.Direction != outgoing ||
		r.server.oorSessionStore == nil {
		return false
	}

	sessionID, err := chainhash.NewHashFromStr(info.SessionId)
	if err != nil {
		return false
	}

	record, err := r.server.oorSessionStore.GetSession(ctx, *sessionID)
	if err != nil {
		return false
	}

	prePONR, err := oor.OutgoingFailedBeforePONR(record)
	if err != nil {
		sessionAttr := slog.String("session_id", info.SessionId)
		r.server.log.WarnS(ctx, "Unable to classify failed OOR session",
			err,
			sessionAttr,
		)

		return false
	}

	return prePONR
}

// queryOORSessionSummaries fetches live OOR summaries from the actor.
func (r *RPCServer) queryOORSessionSummaries(ctx context.Context,
	req *waverpc.ListOORSessionsRequest) ([]*waverpc.OORSessionInfo,
	error) {

	if r.server.actorSystem == nil {
		return nil, nil
	}

	oorRef := oor.NewServiceKey().Ref(r.server.actorSystem)
	listReq := &oor.ListSessionsRequest{
		Direction: protoToOORSessionDirection(
			req.GetDirectionFilter(),
		),
		PendingOnly: req.GetStatusFilter() ==
			waverpc.OORSessionStatus_OOR_SESSION_STATUS_PENDING,
	}

	future := oorRef.Ask(ctx, listReq)
	result := future.Await(ctx)

	actorResp, err := result.Unpack()
	if err != nil {
		if errors.Is(err, actor.ErrNoActorsAvailable) {
			return nil, nil
		}

		return nil, status.Errorf(codes.Internal, "failed to query "+
			"OOR actor: %v", err)
	}

	resp, ok := actorResp.(*oor.ListSessionsResponse)
	if !ok {
		return nil, status.Errorf(codes.Internal, "unexpected OOR "+
			"list response type: %T", actorResp)
	}

	out := make([]*waverpc.OORSessionInfo, 0, len(resp.Sessions))
	for _, summary := range resp.Sessions {
		info := oorSessionSummaryToProto(summary)

		// Keep the RPC-level filter pass even though the actor also
		// receives the filter request. This keeps persisted and live
		// filtering behavior identical, and protects callers if future
		// actors return extra fields that should still be filtered.
		if !oorSessionMatchesFilters(info, req) {
			continue
		}

		out = append(out, info)
	}

	return out, nil
}

// roundSummaryToProto converts one persisted round summary to waverpc.
func roundSummaryToProto(summary *db.RoundSummary) *waverpc.RoundInfo {
	if summary == nil {
		return nil
	}

	info := &waverpc.RoundInfo{
		RoundId:        summary.RoundID.String(),
		State:          dbStatusToProto(summary.Status),
		IsTemp:         false,
		CreationTime:   summary.CreationTime,
		LastUpdateTime: summary.LastUpdateTime,
	}

	if summary.CommitmentTxID.IsSome() {
		txid := summary.CommitmentTxID.UnwrapOr(chainhash.Hash{})
		info.CommitmentTxid = txid.String()
	}

	if summary.ConfirmationHeight.IsSome() {
		height := summary.ConfirmationHeight.UnwrapOr(0)
		info.CommitmentHeight = height
	}

	info.InputOutpoints = outpointsToStrings(summary.InputOutpoints)
	info.OutputOutpoints = make([]string, 0, len(summary.VTXOs))
	for _, v := range summary.VTXOs {
		outpoint := v.Outpoint.String()
		info.OutputOutpoints = append(info.OutputOutpoints, outpoint)
		info.Vtxos = append(info.Vtxos, &waverpc.RoundVTXOInfo{
			Outpoint:  outpoint,
			AmountSat: int64(v.Amount),
		})
	}

	return info
}

// roundInfoMatchesFilters reports whether a round should be returned.
func roundInfoMatchesFilters(info *waverpc.RoundInfo,
	req *waverpc.ListRoundsRequest) bool {

	if info == nil || req == nil {
		return false
	}

	if req.GetStateFilter() != waverpc.RoundState_ROUND_STATE_UNKNOWN &&
		info.GetState() != req.GetStateFilter() {
		return false
	}

	if info.GetCreationTime() != 0 {
		created := time.Unix(info.GetCreationTime(), 0)
		if req.GetCreatedAfter() != 0 &&
			created.Before(time.Unix(req.GetCreatedAfter(), 0)) {
			return false
		}

		if req.GetCreatedBefore() != 0 &&
			created.After(time.Unix(req.GetCreatedBefore(), 0)) {
			return false
		}
	}

	return true
}

// roundFailureReason returns a human-readable reason for failed live rounds.
func roundFailureReason(state round.ClientState) string {
	failed, ok := state.(*round.ClientFailedState)
	if !ok || failed == nil {
		return ""
	}

	if failed.Reason != "" {
		return failed.Reason
	}

	if failed.Error != nil {
		return failed.Error.Error()
	}

	return ""
}

// oorSessionSummaryToProto converts an in-memory OOR actor summary.
func oorSessionSummaryToProto(
	summary oor.SessionSummary) *waverpc.OORSessionInfo {

	status := waverpc.OORSessionStatus_OOR_SESSION_STATUS_PENDING
	if !summary.Pending {
		status = waverpc.OORSessionStatus_OOR_SESSION_STATUS_COMPLETED
	}
	if summary.Phase == string(oor.OutgoingPhaseFailed) ||
		summary.Phase == string(oor.IncomingPhaseFailed) {

		status = waverpc.OORSessionStatus_OOR_SESSION_STATUS_FAILED
	}

	return &waverpc.OORSessionInfo{
		SessionId:         summary.SessionID.String(),
		Direction:         oorDirectionToProto(summary.Direction),
		Status:            status,
		Phase:             summary.Phase,
		ConsumedOutpoints: outpointsToStrings(summary.InputOutpoints),
		FailureReason:     summary.RetryReason,
	}
}

// oorSessionMatchesFilters reports whether a session should be returned.
func oorSessionMatchesFilters(info *waverpc.OORSessionInfo,
	req *waverpc.ListOORSessionsRequest) bool {

	if info == nil || req == nil {
		return false
	}

	unspecified := waverpc.
		OORSessionDirection_OOR_SESSION_DIRECTION_UNSPECIFIED
	if req.GetDirectionFilter() != unspecified &&
		info.GetDirection() != req.GetDirectionFilter() {
		return false
	}

	if req.GetStatusFilter() !=
		waverpc.OORSessionStatus_OOR_SESSION_STATUS_UNSPECIFIED &&
		info.GetStatus() != req.GetStatusFilter() {
		return false
	}

	return true
}

// pageOORSessions trims the lookahead row and encodes the last returned key.
// SQL already applied the cursor and deterministic creation-time ordering.
func pageOORSessions(sessions []*waverpc.OORSessionInfo,
	pageSize int32) ([]*waverpc.OORSessionInfo, string) {

	if len(sessions) <= int(pageSize) {
		return sessions, ""
	}
	page := sessions[:pageSize]
	last := page[len(page)-1]
	token := fmt.Sprintf("v1:%d:%s", last.CreatedAt, last.SessionId)

	return page, base64.RawURLEncoding.EncodeToString([]byte(token))
}

// decodeOORStatusCursor accepts only the creation-time cursor format. Legacy
// ID-only cursors use another ordering and must restart rather than skip rows.
func decodeOORStatusCursor(token string) (*db.OORStatusCursor, error) {
	if token == "" {
		return nil, nil
	}
	data, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, fmt.Errorf("malformed cursor encoding")
	}
	parts := strings.Split(string(data), ":")
	if len(parts) != 3 || parts[0] != "v1" || len(parts[2]) != 64 {
		return nil, fmt.Errorf("unsupported cursor format")
	}
	createdAt, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("malformed cursor timestamp")
	}
	id, err := chainhash.NewHashFromStr(parts[2])
	if err != nil {
		return nil, fmt.Errorf("malformed cursor session ID")
	}

	return &db.OORStatusCursor{
		CreatedAt: createdAt, SessionID: *id,
	}, nil
}

// outpointsToStrings converts wire outpoints to their canonical strings.
func outpointsToStrings(outpoints []wire.OutPoint) []string {
	strings := make([]string, 0, len(outpoints))
	for _, outpoint := range outpoints {
		strings = append(strings, outpoint.String())
	}

	sort.Strings(strings)

	return strings
}

// protoToOORSessionDirection maps the daemon RPC enum to the actor enum.
func protoToOORSessionDirection(
	direction waverpc.OORSessionDirection) oor.SessionDirection {

	switch direction {
	case waverpc.OORSessionDirection_OOR_SESSION_DIRECTION_OUTGOING:
		return oor.SessionDirectionOutgoing

	case waverpc.OORSessionDirection_OOR_SESSION_DIRECTION_INCOMING:
		return oor.SessionDirectionIncoming

	default:
		return oor.SessionDirectionAll
	}
}

// oorDirectionToProto maps the actor session direction to daemon RPC.
func oorDirectionToProto(
	direction oor.SessionDirection) waverpc.OORSessionDirection {

	switch direction {
	case oor.SessionDirectionOutgoing:
		return waverpc.
			OORSessionDirection_OOR_SESSION_DIRECTION_OUTGOING

	case oor.SessionDirectionIncoming:
		return waverpc.
			OORSessionDirection_OOR_SESSION_DIRECTION_INCOMING

	default:
		return waverpc.
			OORSessionDirection_OOR_SESSION_DIRECTION_UNSPECIFIED
	}
}

// parseOORSessionID converts a user-supplied hex session id string.
func parseOORSessionID(sessionID string) (chainhash.Hash, error) {
	hash, err := chainhash.NewHashFromStr(sessionID)
	if err != nil {
		return chainhash.Hash{}, fmt.Errorf("parse session id: %w", err)
	}

	return *hash, nil
}
