package waved

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
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

	page, nextToken := pageOORSessions(sessions, req.PageToken, pageSize)

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
	normalizedID := sessionID.String()

	listReq := &waverpc.ListOORSessionsRequest{}
	sessions, err := r.listOORSessions(ctx, listReq)
	if err != nil {
		return nil, err
	}

	for _, session := range sessions {
		if session.GetSessionId() == normalizedID {
			return &waverpc.GetOORSessionResponse{
				Session: session,
			}, nil
		}
	}

	return nil, status.Error(codes.NotFound, "OOR session not found")
}

// listOORSessions merges actor summaries and persisted package artifacts.
func (r *RPCServer) listOORSessions(ctx context.Context,
	req *waverpc.ListOORSessionsRequest) ([]*waverpc.OORSessionInfo,
	error) {

	if req == nil {
		req = &waverpc.ListOORSessionsRequest{}
	}

	allReq := &waverpc.ListOORSessionsRequest{}
	live, err := r.queryOORSessionSummaries(ctx, allReq)
	if err != nil {
		return nil, err
	}

	persisted, err := r.queryPersistedOORSessions(ctx, req)
	if err != nil {
		return nil, err
	}

	if shouldQueryPersistedOORLiveOverlay(req) {
		overlay, err := r.queryPersistedOORSessionsForLive(ctx, live)
		if err != nil {
			return nil, err
		}

		persisted = append(persisted, overlay...)
	}

	return mergeOORSessionLists(live, persisted, req), nil
}

// mergeOORSessionLists combines actor and package-store views for OOR status.
func mergeOORSessionLists(live, persisted []*waverpc.OORSessionInfo,
	req *waverpc.ListOORSessionsRequest) []*waverpc.OORSessionInfo {

	if req == nil {
		req = &waverpc.ListOORSessionsRequest{}
	}

	merged := make(map[string]*waverpc.OORSessionInfo)

	for _, session := range persisted {
		merged[session.GetSessionId()] = session
	}

	for _, session := range live {
		if existing, ok := merged[session.GetSessionId()]; ok {
			mergeOORSessionInfo(existing, session)
			continue
		}

		merged[session.GetSessionId()] = session
	}

	sessions := make([]*waverpc.OORSessionInfo, 0, len(merged))
	for _, session := range merged {
		if !oorSessionMatchesFilters(session, req) {
			continue
		}

		sessions = append(sessions, session)
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].GetSessionId() < sessions[j].GetSessionId()
	})

	return sessions
}

// mergeOORSessionInfo fills artifact-backed fields missing from live state.
func mergeOORSessionInfo(dst *waverpc.OORSessionInfo,
	src *waverpc.OORSessionInfo) {

	if dst == nil || src == nil {
		return
	}

	if dst.CreatedAt == 0 {
		dst.CreatedAt = src.GetCreatedAt()
	}
	if dst.UpdatedAt == 0 {
		dst.UpdatedAt = src.GetUpdatedAt()
	}
	if len(dst.ConsumedOutpoints) == 0 {
		dst.ConsumedOutpoints = append(
			[]string(nil), src.GetConsumedOutpoints()...,
		)
	}
	if len(dst.CreatedOutpoints) == 0 {
		dst.CreatedOutpoints = append(
			[]string(nil), src.GetCreatedOutpoints()...,
		)
	}
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

// queryPersistedOORSessions fetches completed package status from disk.
func (r *RPCServer) queryPersistedOORSessions(ctx context.Context,
	req *waverpc.ListOORSessionsRequest) ([]*waverpc.OORSessionInfo,
	error) {

	if !shouldListPersistedOORPackages(req) {
		return nil, nil
	}

	store := r.newLocalOORArtifactStore()
	if store == nil {
		return nil, nil
	}

	direction := protoToPackageDirection(req.GetDirectionFilter())
	packages, err := store.ListPackages(ctx, direction)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to list "+
			"persisted OOR packages: %v", err)
	}

	out := make([]*waverpc.OORSessionInfo, 0, len(packages))
	for _, pkg := range packages {
		info := oorPackageToProto(pkg)
		if !oorSessionMatchesFilters(info, req) {
			continue
		}

		out = append(out, info)
	}

	return out, nil
}

// queryPersistedOORSessionsForLive fetches persisted package entries only for
// live actor sessions. This preserves artifact-backed corrections for filtered
// list calls without forcing every pending or failed query to materialize the
// full package store.
func (r *RPCServer) queryPersistedOORSessionsForLive(ctx context.Context,
	live []*waverpc.OORSessionInfo) ([]*waverpc.OORSessionInfo, error) {

	if len(live) == 0 {
		return nil, nil
	}

	store := r.newLocalOORArtifactStore()
	if store == nil {
		return nil, nil
	}

	seen := make(map[string]struct{}, len(live))
	out := make([]*waverpc.OORSessionInfo, 0, len(live))
	for _, session := range live {
		sessionID := session.GetSessionId()
		if _, ok := seen[sessionID]; ok {
			continue
		}
		seen[sessionID] = struct{}{}

		hash, err := parseOORSessionID(sessionID)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "parse live "+
				"OOR session id: %v", err)
		}

		pkg, err := store.GetPackage(ctx, hash)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to "+
				"get persisted OOR package: %v", err)
		}

		out = append(out, oorPackageToProto(pkg))
	}

	return out, nil
}

// shouldListPersistedOORPackages reports whether a list request can include
// completed package entries from a bounded store scan.
func shouldListPersistedOORPackages(req *waverpc.ListOORSessionsRequest) bool {
	switch req.GetStatusFilter() {
	case waverpc.OORSessionStatus_OOR_SESSION_STATUS_PENDING,
		waverpc.OORSessionStatus_OOR_SESSION_STATUS_FAILED:
		return false

	default:
		return true
	}
}

// shouldQueryPersistedOORLiveOverlay reports whether live summaries need
// targeted package lookups before final filter evaluation.
func shouldQueryPersistedOORLiveOverlay(
	req *waverpc.ListOORSessionsRequest) bool {

	if req.GetDirectionFilter() != waverpc.
		OORSessionDirection_OOR_SESSION_DIRECTION_UNSPECIFIED {
		return true
	}

	switch req.GetStatusFilter() {
	case waverpc.OORSessionStatus_OOR_SESSION_STATUS_PENDING,
		waverpc.OORSessionStatus_OOR_SESSION_STATUS_FAILED:
		return true

	default:
		return false
	}
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

// oorPackageToProto converts one persisted OOR package artifact to waverpc.
func oorPackageToProto(pkg *db.OORPackageBundle) *waverpc.OORSessionInfo {
	if pkg == nil {
		return nil
	}

	completed := waverpc.OORSessionStatus_OOR_SESSION_STATUS_COMPLETED
	info := &waverpc.OORSessionInfo{
		SessionId: pkg.SessionID.String(),
		Direction: packageDirectionToProto(pkg.Direction),
		Status:    completed,
		Phase:     "completed",
		CreatedAt: pkg.CreatedAt.Unix(),
		UpdatedAt: pkg.UpdatedAt.Unix(),
	}

	for _, binding := range pkg.Bindings {
		switch binding.LinkKind {
		case db.OORPackageLinkKindConsumedInput:
			info.ConsumedOutpoints = append(
				info.ConsumedOutpoints,
				binding.Outpoint.String(),
			)

		case db.OORPackageLinkKindCreatedOutput:
			info.CreatedOutpoints = append(
				info.CreatedOutpoints,
				binding.Outpoint.String(),
			)
		}
	}

	sort.Strings(info.ConsumedOutpoints)
	sort.Strings(info.CreatedOutpoints)

	return info
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

// pageOORSessions slices sorted OOR sessions using a session-id cursor.
func pageOORSessions(sessions []*waverpc.OORSessionInfo, pageToken string,
	pageSize int32) ([]*waverpc.OORSessionInfo, string) {

	start := 0
	if pageToken != "" {
		for i, session := range sessions {
			if session.GetSessionId() > pageToken {
				start = i
				break
			}
			start = i + 1
		}
	}

	end := start + int(pageSize)
	if end > len(sessions) {
		end = len(sessions)
	}

	page := sessions[start:end]
	if end >= len(sessions) {
		return page, ""
	}

	return page, page[len(page)-1].GetSessionId()
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

// protoToPackageDirection maps an optional daemon RPC direction filter.
func protoToPackageDirection(
	direction waverpc.OORSessionDirection) *db.OORPackageDirection {

	switch direction {
	case waverpc.OORSessionDirection_OOR_SESSION_DIRECTION_OUTGOING:
		outgoing := db.OORPackageDirectionOutgoing

		return &outgoing

	case waverpc.OORSessionDirection_OOR_SESSION_DIRECTION_INCOMING:
		incoming := db.OORPackageDirectionIncoming

		return &incoming

	default:
		return nil
	}
}

// packageDirectionToProto maps persisted OOR package direction to RPC.
func packageDirectionToProto(
	direction db.OORPackageDirection) waverpc.OORSessionDirection {

	switch direction {
	case db.OORPackageDirectionOutgoing:
		return waverpc.
			OORSessionDirection_OOR_SESSION_DIRECTION_OUTGOING

	case db.OORPackageDirectionIncoming:
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
