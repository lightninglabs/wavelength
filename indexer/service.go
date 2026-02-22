package indexer

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo/db"
	"github.com/lightninglabs/darepo/db/sqlc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Service implements arkrpc.IndexerServiceServer for the mailbox indexer.
//
// This implementation is authoritative for VTXO and tree state and reads from
// the operator database.
//
// Receive-script registrations, OOR recipient events, and VTXO lifecycle
// events are persisted in the operator database to survive restarts.
type Service struct {
	arkrpc.UnimplementedIndexerServiceServer

	serverID string
	now      func() time.Time

	store *db.Store

	authorizer ScriptAuthorizer
}

// NewService creates a new indexer service.
func NewService(serverID string, store *db.Store) *Service {
	return &Service{
		serverID: serverID,
		now:      time.Now,

		store:      store,
		authorizer: NewAllowAllScriptAuthorizer(),
	}
}

// SetScriptAuthorizer overrides the script authorization policy.
//
// Passing nil resets the policy to allow-all.
func (s *Service) SetScriptAuthorizer(authorizer ScriptAuthorizer) {
	if authorizer == nil {
		s.authorizer = NewAllowAllScriptAuthorizer()

		return
	}

	s.authorizer = authorizer
}

// authorizeScripts applies the configured script authorization policy.
func (s *Service) authorizeScripts(ctx context.Context,
	principalMailboxID string, purpose string,
	pkScripts [][]byte) error {

	if s.authorizer == nil {
		return nil
	}

	return s.authorizer.AuthorizeScripts(
		ctx,
		ScriptAuthorizationRequest{
			PrincipalMailboxID: principalMailboxID,
			Purpose:            purpose,
			PkScripts:          pkScripts,
			Now:                s.now(),
		},
	)
}

// RegisterReceiveScript validates proofs and registers a receive script for the
// calling principal.
func (s *Service) RegisterReceiveScript(ctx context.Context,
	req *arkrpc.RegisterReceiveScriptRequest) (
	*arkrpc.RegisterReceiveScriptResponse, error) {

	principal, ok := PrincipalFromContext(ctx)
	if !ok || principal.MailboxID == "" {
		return nil, status.Error(codes.Unauthenticated,
			"missing mailbox principal")
	}

	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "nil request")
	}

	pkScript := req.GetPkScript()
	if len(pkScript) == 0 {
		return nil, status.Error(codes.InvalidArgument,
			"missing pk_script")
	}

	now := s.now()

	switch proof := req.Proof.(type) {
	case *arkrpc.RegisterReceiveScriptRequest_TaprootSchnorr:
		err := verifyTaprootSchnorrProof(
			now, pkScript, proof.TaprootSchnorr,
			s.serverID, principal.MailboxID,
		)
		if err != nil {
			return nil, status.Error(codes.Unauthenticated,
				err.Error())
		}

	case *arkrpc.RegisterReceiveScriptRequest_Bip322:
		return nil, status.Error(codes.Unimplemented,
			"bip322 proofs not implemented")

	default:
		return nil, status.Error(codes.InvalidArgument,
			"missing proof")
	}

	expiresAt := req.ExpiresAtUnixS
	if expiresAt == 0 {
		// Mirror the proof message expiry if the request doesn't
		// specify a retention time. This keeps the transport simple
		// while still bounding replay windows.
		msg, err := parseReceiveScriptProofMessage(
			req.GetTaprootSchnorr().GetMessage(),
		)
		if err == nil && msg.ExpiresAt > 0 {
			expiresAt = uint64(msg.ExpiresAt)
		}
	}

	if s.store == nil {
		return nil, status.Error(
			codes.FailedPrecondition,
			"indexer database not configured",
		)
	}

	err := s.store.Queries.UpsertIndexerReceiveScript(
		ctx,
		sqlc.UpsertIndexerReceiveScriptParams{
			PrincipalMailboxID: principal.MailboxID,
			PkScript:           append([]byte(nil), pkScript...),
			ExpiresAtUnixS:     int64(expiresAt),
			Label:              req.Label,
			UpdatedAt:          s.now().UnixNano(),
		},
	)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &arkrpc.RegisterReceiveScriptResponse{}, nil
}

// ListMyReceiveScripts lists scripts registered by the calling principal.
func (s *Service) ListMyReceiveScripts(ctx context.Context,
	req *arkrpc.ListMyReceiveScriptsRequest) (
	*arkrpc.ListMyReceiveScriptsResponse, error) {

	_ = req

	principal, ok := PrincipalFromContext(ctx)
	if !ok || principal.MailboxID == "" {
		return nil, status.Error(codes.Unauthenticated,
			"missing mailbox principal")
	}

	if s.store == nil {
		return nil, status.Error(
			codes.FailedPrecondition,
			"indexer database not configured",
		)
	}

	query := sqlc.ListActiveIndexerReceiveScriptsByPrincipalParams{
		PrincipalMailboxID: principal.MailboxID,
		ExpiresAtUnixS:     s.now().Unix(),
	}
	scripts, err := s.store.Queries.
		ListActiveIndexerReceiveScriptsByPrincipal(
			ctx, query,
		)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	var out []*arkrpc.RegisteredReceiveScript
	for _, reg := range scripts {
		out = append(out, &arkrpc.RegisteredReceiveScript{
			PkScript:       append([]byte(nil), reg.PkScript...),
			ExpiresAtUnixS: uint64(reg.ExpiresAtUnixS),
			Label:          reg.Label,
		})
	}

	sort.Slice(out, func(i, j int) bool {
		return hex.EncodeToString(out[i].PkScript) <
			hex.EncodeToString(out[j].PkScript)
	})

	return &arkrpc.ListMyReceiveScriptsResponse{
		Scripts: out,
	}, nil
}

// UnregisterReceiveScript removes a registration for the calling principal.
func (s *Service) UnregisterReceiveScript(ctx context.Context,
	req *arkrpc.UnregisterReceiveScriptRequest) (
	*arkrpc.UnregisterReceiveScriptResponse, error) {

	principal, ok := PrincipalFromContext(ctx)
	if !ok || principal.MailboxID == "" {
		return nil, status.Error(codes.Unauthenticated,
			"missing mailbox principal")
	}

	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "nil request")
	}

	pkScript := req.GetPkScript()
	if len(pkScript) == 0 {
		return nil, status.Error(codes.InvalidArgument,
			"missing pk_script")
	}

	if s.store == nil {
		return nil, status.Error(
			codes.FailedPrecondition,
			"indexer database not configured",
		)
	}

	_, err := s.store.Queries.DeleteIndexerReceiveScript(
		ctx,
		sqlc.DeleteIndexerReceiveScriptParams{
			PrincipalMailboxID: principal.MailboxID,
			PkScript:           append([]byte(nil), pkScript...),
		},
	)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &arkrpc.UnregisterReceiveScriptResponse{}, nil
}

// ListOORRecipientEventsByScript lists recipient events for a pkScript gated by
// a proof-of-control.
func (s *Service) ListOORRecipientEventsByScript(ctx context.Context,
	req *arkrpc.ListOORRecipientEventsByScriptRequest) (
	*arkrpc.ListOORRecipientEventsByScriptResponse, error) {

	principal, ok := PrincipalFromContext(ctx)
	if !ok || principal.MailboxID == "" {
		return nil, status.Error(codes.Unauthenticated,
			"missing mailbox principal")
	}

	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "nil request")
	}

	pkScript := req.PkScript
	if len(pkScript) == 0 {
		return nil, status.Error(codes.InvalidArgument,
			"missing pk_script")
	}

	now := s.now()

	switch proof := req.Proof.(type) {
	case *arkrpc.ListOORRecipientEventsByScriptRequest_TaprootSchnorr:
		if err := verifyTaprootSchnorrProof(
			now, pkScript, proof.TaprootSchnorr,
			s.serverID, principal.MailboxID,
		); err != nil {
			return nil, status.Error(codes.Unauthenticated,
				err.Error())
		}

	case *arkrpc.ListOORRecipientEventsByScriptRequest_Bip322:
		return nil, status.Error(codes.Unimplemented,
			"bip322 proofs not implemented")

	default:
		return nil, status.Error(codes.InvalidArgument,
			"missing proof")
	}

	limit := req.Limit
	if limit == 0 {
		limit = defaultRecipientEventLimit
	}

	if s.store == nil {
		return nil, status.Error(
			codes.FailedPrecondition,
			"indexer database not configured",
		)
	}

	rows, err := s.store.Queries.ListOORRecipientEventsAfterWithSession(
		ctx,
		sqlc.ListOORRecipientEventsAfterWithSessionParams{
			RecipientPkScript: append([]byte(nil), pkScript...),
			EventID:           int64(req.AfterEventId),
			Limit:             int32(limit),
		},
	)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	var out []*arkrpc.OORRecipientEvent
	var nextCursor uint64

	for _, row := range rows {
		ev := &arkrpc.OORRecipientEvent{
			RecipientPkScript: append([]byte(nil),
				row.RecipientPkScript...),
			EventId:     uint64(row.EventID),
			SessionId:   append([]byte(nil), row.SessionID...),
			OutputIndex: uint32(row.OutputIndex),
			Value:       uint64(row.Value),
		}

		out = append(out, ev)
		nextCursor = ev.EventId
	}

	return &arkrpc.ListOORRecipientEventsByScriptResponse{
		Events:     out,
		NextCursor: nextCursor,
	}, nil
}

// ListVTXOsByScripts returns VTXOs matching the provided scripts, gated by
// proof-of-control for each script.
func (s *Service) ListVTXOsByScripts(ctx context.Context,
	req *arkrpc.ListVTXOsByScriptsRequest) (
	*arkrpc.ListVTXOsByScriptsResponse, error) {

	principal, ok := PrincipalFromContext(ctx)
	if !ok || principal.MailboxID == "" {
		return nil, status.Error(codes.Unauthenticated,
			"missing mailbox principal")
	}

	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "nil request")
	}

	if len(req.Scripts) == 0 {
		return nil, status.Error(codes.InvalidArgument,
			"missing scripts")
	}

	now := s.now()

	allowedScripts := make(map[string]struct{}, len(req.Scripts))
	allowedScriptBytes := make([][]byte, 0, len(req.Scripts))
	for _, scope := range req.Scripts {
		if scope == nil || len(scope.PkScript) == 0 {
			return nil, status.Error(codes.InvalidArgument,
				"missing pk_script")
		}

		err := verifyScriptScopeProof(
			now,
			scope.PkScript,
			scope.Proof,
			s.serverID,
			principal.MailboxID,
			purposeListVTXOsByScripts,
		)
		if err != nil {
			return nil, scopeProofToStatus(err)
		}

		allowedScripts[hex.EncodeToString(scope.PkScript)] = struct{}{}
		pkScriptCopy := append([]byte(nil), scope.PkScript...)
		allowedScriptBytes = append(allowedScriptBytes, pkScriptCopy)
	}
	if err := s.authorizeScripts(
		ctx, principal.MailboxID, purposeListVTXOsByScripts,
		allowedScriptBytes,
	); err != nil {
		return nil, status.Error(codes.PermissionDenied, err.Error())
	}

	statusFilter := make(map[arkrpc.VTXOStatus]struct{})
	for _, st := range req.StatusFilter {
		statusFilter[st] = struct{}{}
	}

	if s.store == nil {
		return nil, status.Error(
			codes.FailedPrecondition,
			"indexer database not configured",
		)
	}

	q := s.store.Queries

	rows, err := q.ListVTXOsByPkScripts(ctx, allowedScriptBytes)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	roundRowByHex := make(map[string]*sqlc.Round)

	for _, row := range rows {
		if len(row.RoundID) == 0 {
			continue
		}

		roundHex := hex.EncodeToString(row.RoundID)
		if _, ok := roundRowByHex[roundHex]; ok {
			continue
		}

		rr, err := q.GetRound(ctx, row.RoundID)
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		roundRowByHex[roundHex] = &rr
	}

	var all []*arkrpc.VTXO
	for _, row := range rows {
		scriptHex := hex.EncodeToString(row.PkScript)
		if _, ok := allowedScripts[scriptHex]; !ok {
			// Defensive: do not leak results outside the proven
			// script set.
			continue
		}

		roundHex := hex.EncodeToString(row.RoundID)
		rr := roundRowByHex[roundHex]

		vtxo, err := rpcVTXOFromDB(row, rr)
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}

		if len(statusFilter) > 0 {
			if _, ok := statusFilter[vtxo.Status]; !ok {
				continue
			}
		}

		all = append(all, vtxo)
	}

	sort.Slice(all, func(i, j int) bool {
		return outpointKey(all[i].Outpoint) <
			outpointKey(all[j].Outpoint)
	})

	limit := req.Limit
	if limit == 0 {
		limit = defaultVTXOLimit
	}

	cursor := req.Cursor
	if cursor > uint64(len(all)) {
		return nil, status.Error(codes.InvalidArgument,
			"cursor out of range")
	}

	start := int(cursor)
	end := start + int(limit)
	if end > len(all) {
		end = len(all)
	}

	out := all[start:end]
	nextCursor := uint64(end)
	if nextCursor >= uint64(len(all)) {
		// End reached; keep next cursor pinned at len(all).
		nextCursor = uint64(len(all))
	}

	return &arkrpc.ListVTXOsByScriptsResponse{
		Vtxos:      out,
		NextCursor: nextCursor,
	}, nil
}

// GetSubtreeByScripts returns a minimal spanning subtree for all leaves
// matching the provided scripts.
func (s *Service) GetSubtreeByScripts(ctx context.Context,
	req *arkrpc.GetSubtreeByScriptsRequest) (
	*arkrpc.GetSubtreeByScriptsResponse, error) {

	principal, ok := PrincipalFromContext(ctx)
	if !ok || principal.MailboxID == "" {
		return nil, status.Error(codes.Unauthenticated,
			"missing mailbox principal")
	}

	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "nil request")
	}

	if len(req.Scripts) == 0 {
		return nil, status.Error(codes.InvalidArgument,
			"missing scripts")
	}

	now := s.now()

	allowedScripts := make(map[string]struct{}, len(req.Scripts))
	allowedScriptBytes := make([][]byte, 0, len(req.Scripts))
	for _, scope := range req.Scripts {
		if scope == nil || len(scope.PkScript) == 0 {
			return nil, status.Error(codes.InvalidArgument,
				"missing pk_script")
		}

		err := verifyScriptScopeProof(
			now,
			scope.PkScript,
			scope.Proof,
			s.serverID,
			principal.MailboxID,
			purposeGetSubtreeByScripts,
		)
		if err != nil {
			return nil, scopeProofToStatus(err)
		}

		allowedScripts[hex.EncodeToString(scope.PkScript)] = struct{}{}
		pkScriptCopy := append([]byte(nil), scope.PkScript...)
		allowedScriptBytes = append(allowedScriptBytes, pkScriptCopy)
	}
	if err := s.authorizeScripts(
		ctx, principal.MailboxID, purposeGetSubtreeByScripts,
		allowedScriptBytes,
	); err != nil {
		return nil, status.Error(codes.PermissionDenied, err.Error())
	}

	if s.store == nil {
		return nil, status.Error(
			codes.FailedPrecondition,
			"indexer database not configured",
		)
	}

	q := s.store.Queries

	inputs, err := loadSubtreeInputs(
		ctx, q, allowedScripts, allowedScriptBytes,
	)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	nodesByTxid := make(map[string]*arkrpc.TreeNode)
	edgesByKey := make(map[string]*arkrpc.TreeEdge)
	leafTXByTxid := make(map[string][]byte)

	for key, targets := range inputs.targetOutpointsByTree {
		roundID := inputs.roundIDByHex[key.roundIDHex]

		fullTree, err := loadRoundVTXOTree(
			ctx, q, roundID, key.batchIdx,
		)
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}

		extracted, err := extractTreeForOutpoints(fullTree, targets)
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}

		leafTXs, err := collectLeafProofTXs(extracted)
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		for txid, serializedLeafTX := range leafTXs {
			leafTXByTxid[txid] = serializedLeafTX
		}

		if err := recordSubtreeRPCView(
			extracted,
			req.IncludeInternalNodes,
			inputs.leafTxids,
			nodesByTxid,
			edgesByKey,
		); err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
	}

	if err := enrichVirtualLeafProofs(
		ctx, q, inputs.virtualLeaves,
	); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	for _, leaf := range inputs.leaves {
		if leaf == nil || leaf.Outpoint == nil {
			continue
		}

		leafTxid := hex.EncodeToString(leaf.Outpoint.Txid)
		leafTx, ok := leafTXByTxid[leafTxid]
		if !ok {
			continue
		}

		leaf.LeafTx = append([]byte(nil), leafTx...)
	}

	var outNodes []*arkrpc.TreeNode
	for _, n := range nodesByTxid {
		outNodes = append(outNodes, n)
	}
	sort.Slice(outNodes, func(i, j int) bool {
		return hex.EncodeToString(outNodes[i].Txid) <
			hex.EncodeToString(outNodes[j].Txid)
	})

	var outEdges []*arkrpc.TreeEdge
	for _, e := range edgesByKey {
		outEdges = append(outEdges, e)
	}
	sort.Slice(outEdges, func(i, j int) bool {
		ai := fmt.Sprintf("%s:%d:%s",
			hex.EncodeToString(outEdges[i].ParentTxid),
			outEdges[i].ParentOutputIndex,
			hex.EncodeToString(outEdges[i].ChildTxid),
		)
		aj := fmt.Sprintf("%s:%d:%s",
			hex.EncodeToString(outEdges[j].ParentTxid),
			outEdges[j].ParentOutputIndex,
			hex.EncodeToString(outEdges[j].ChildTxid),
		)

		return ai < aj
	})

	sort.Slice(inputs.leaves, func(i, j int) bool {
		return outpointKey(inputs.leaves[i].Outpoint) <
			outpointKey(inputs.leaves[j].Outpoint)
	})

	return &arkrpc.GetSubtreeByScriptsResponse{
		Vtxos: inputs.leaves,
		Nodes: outNodes,
		Edges: outEdges,
	}, nil
}

// ListVTXOEventsByScripts returns a monotonic event feed for VTXOs matching the
// provided scripts.
func (s *Service) ListVTXOEventsByScripts(ctx context.Context,
	req *arkrpc.ListVTXOEventsByScriptsRequest) (
	*arkrpc.ListVTXOEventsByScriptsResponse, error) {

	principal, ok := PrincipalFromContext(ctx)
	if !ok || principal.MailboxID == "" {
		return nil, status.Error(codes.Unauthenticated,
			"missing mailbox principal")
	}

	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "nil request")
	}

	if len(req.Scripts) == 0 {
		return nil, status.Error(codes.InvalidArgument,
			"missing scripts")
	}

	now := s.now()

	allowedScriptBytes := make([][]byte, 0, len(req.Scripts))
	for _, scope := range req.Scripts {
		if scope == nil || len(scope.PkScript) == 0 {
			return nil, status.Error(codes.InvalidArgument,
				"missing pk_script")
		}

		err := verifyScriptScopeProof(
			now,
			scope.PkScript,
			scope.Proof,
			s.serverID,
			principal.MailboxID,
			purposeListVTXOEventsByScripts,
		)
		if err != nil {
			return nil, scopeProofToStatus(err)
		}

		allowedScriptBytes = append(
			allowedScriptBytes,
			append([]byte(nil), scope.PkScript...),
		)
	}
	if err := s.authorizeScripts(
		ctx, principal.MailboxID, purposeListVTXOEventsByScripts,
		allowedScriptBytes,
	); err != nil {
		return nil, status.Error(codes.PermissionDenied, err.Error())
	}

	limit := req.Limit
	if limit == 0 {
		limit = defaultVTXOEventLimit
	}

	if s.store == nil {
		return nil, status.Error(
			codes.FailedPrecondition,
			"indexer database not configured",
		)
	}

	rows, err := s.store.Queries.ListIndexerVTXOEventsAfterByScripts(
		ctx,
		sqlc.ListIndexerVTXOEventsAfterByScriptsParams{
			EventID:   int64(req.AfterEventId),
			Limit:     int32(limit),
			PkScripts: allowedScriptBytes,
		},
	)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	var out []*arkrpc.VTXOEvent
	var nextCursor uint64
	for _, row := range rows {
		outpoint, err := wireOutPointFromDBRow(
			row.OutpointHash, row.OutpointIndex,
		)
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}

		event := &arkrpc.VTXOEvent{
			EventId: uint64(row.EventID),
			Type:    vtxoEventTypeFromStore(row.EventType),
			Outpoint: &arkrpc.OutPoint{
				Txid: outpoint.Hash[:],
				Vout: outpoint.Index,
			},
			Status: VTXOStatusFromStore(row.Status),
			CreatedAtUnixMs: row.CreatedAt /
				int64(time.Millisecond),
		}

		out = append(out, event)
		nextCursor = uint64(row.EventID)
	}

	return &arkrpc.ListVTXOEventsByScriptsResponse{
		Events:     out,
		NextCursor: nextCursor,
	}, nil
}

// AddOORRecipientEvent stores an incoming OOR recipient event and returns a
// bounded notification payload plus the set of principals currently registered
// for that script.
func (s *Service) AddOORRecipientEvent(ctx context.Context,
	ev *arkrpc.OORRecipientEvent) (
	*arkrpc.IncomingOOREvent, []string, error) {

	if ev == nil {
		return nil, nil, fmt.Errorf("nil event")
	}
	if len(ev.RecipientPkScript) == 0 {
		return nil, nil, fmt.Errorf("missing recipient pk_script")
	}
	if len(ev.SessionId) == 0 {
		return nil, nil, fmt.Errorf("missing session id")
	}
	if s.store == nil {
		return nil, nil, fmt.Errorf("indexer database not configured")
	}

	q := s.store.Queries

	params := sqlc.GetOORRecipientEventBySessionOutputParams{
		RecipientPkScript: append([]byte(nil),
			ev.RecipientPkScript...),
		SessionID: append([]byte(nil),
			ev.SessionId...),
		OutputIndex: int32(ev.OutputIndex),
	}
	storedEvent, err := q.GetOORRecipientEventBySessionOutput(
		ctx, params,
	)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, nil, fmt.Errorf(
			"get existing recipient event: %w", err,
		)
	}

	if errors.Is(err, sql.ErrNoRows) {
		// Look up the OOR session's internal DB ID from the
		// session_id bytes so we can reference it as a foreign key
		// in the recipient event row.
		sessionRow, sessionErr := q.GetOORSession(
			ctx, append([]byte(nil), ev.SessionId...),
		)
		if sessionErr != nil {
			return nil, nil, fmt.Errorf(
				"get oor session: %w", sessionErr,
			)
		}

		eventID, insertErr := s.insertRecipientEvent(
			ctx,
			append([]byte(nil), ev.RecipientPkScript...),
			int32(sessionRow.ID),
			int32(ev.OutputIndex),
			int64(ev.Value),
			s.now().UnixNano(),
		)
		if insertErr != nil {
			return nil, nil, insertErr
		}

		storedEvent, err = q.GetOORRecipientEventBySessionOutput(
			ctx,
			sqlc.GetOORRecipientEventBySessionOutputParams{
				RecipientPkScript: append([]byte(nil),
					ev.RecipientPkScript...),
				SessionID: append([]byte(nil),
					ev.SessionId...),
				OutputIndex: int32(ev.OutputIndex),
			},
		)
		if err != nil {
			return nil, nil, fmt.Errorf(
				"load inserted recipient event: %w", err,
			)
		}
		storedEvent.EventID = int64(eventID)
	}

	rows, err := q.ListActiveIndexerReceivePrincipalsByScript(
		ctx,
		sqlc.ListActiveIndexerReceivePrincipalsByScriptParams{
			PkScript: append([]byte(nil),
				ev.RecipientPkScript...),
			ExpiresAtUnixS: s.now().Unix(),
		},
	)
	if err != nil {
		return nil, nil, fmt.Errorf("list active principals: %w", err)
	}

	principals := make([]string, 0, len(rows))
	for _, row := range rows {
		principals = append(principals, row.PrincipalMailboxID)
	}

	return &arkrpc.IncomingOOREvent{
		RecipientPkScript: append([]byte(nil),
			storedEvent.RecipientPkScript...),
		RecipientEventId: uint64(storedEvent.EventID),
		SessionId:        append([]byte(nil), ev.SessionId...),
		OutputIndex:      uint32(storedEvent.OutputIndex),
		Value:            uint64(storedEvent.Value),
	}, principals, nil
}

// AddVTXOEvent appends a VTXO event and returns a bounded notification payload
// plus the set of principals currently registered for pkScript.
func (s *Service) AddVTXOEvent(ctx context.Context, pkScript []byte,
	evType arkrpc.VTXOEventType, outpoint *arkrpc.OutPoint,
	st arkrpc.VTXOStatus) (*arkrpc.IncomingVTXOEvent, []string, error) {

	if len(pkScript) == 0 {
		return nil, nil, fmt.Errorf("missing pk_script")
	}
	if outpoint == nil || len(outpoint.Txid) == 0 {
		return nil, nil, fmt.Errorf("missing outpoint")
	}
	if len(outpoint.Txid) != 32 {
		return nil, nil, fmt.Errorf("unexpected outpoint txid length")
	}
	if s.store == nil {
		return nil, nil, fmt.Errorf("indexer database not configured")
	}

	now := s.now()
	insertedEventID, err := s.store.Queries.InsertIndexerVTXOEvent(
		ctx,
		sqlc.InsertIndexerVTXOEventParams{
			PkScript:      append([]byte(nil), pkScript...),
			EventType:     vtxoEventTypeToStore(evType),
			OutpointHash:  append([]byte(nil), outpoint.Txid...),
			OutpointIndex: int32(outpoint.Vout),
			Status:        storeVTXOStatusFromRPC(st),
			CreatedAt:     now.UnixNano(),
		},
	)
	if err != nil {
		return nil, nil, fmt.Errorf("insert vtxo event: %w", err)
	}

	rows, err := s.store.Queries.ListActiveIndexerReceivePrincipalsByScript(
		ctx,
		sqlc.ListActiveIndexerReceivePrincipalsByScriptParams{
			PkScript:       append([]byte(nil), pkScript...),
			ExpiresAtUnixS: now.Unix(),
		},
	)
	if err != nil {
		return nil, nil, fmt.Errorf("list active principals: %w", err)
	}

	principals := make([]string, 0, len(rows))
	for _, row := range rows {
		principals = append(principals, row.PrincipalMailboxID)
	}

	return &arkrpc.IncomingVTXOEvent{
		EventId:  uint64(insertedEventID),
		Type:     evType,
		Outpoint: cloneOutPoint(outpoint),
		Status:   st,
	}, principals, nil
}

func outpointKey(op *arkrpc.OutPoint) string {
	if op == nil {
		return ""
	}

	return fmt.Sprintf("%s:%d", hex.EncodeToString(op.Txid), op.Vout)
}

func scopeProofToStatus(err error) error {
	if err == nil {
		return nil
	}

	if errors.Is(err, ErrMissingProof) {
		return status.Error(codes.InvalidArgument, err.Error())
	}

	if errors.Is(err, ErrBIP322Unimplemented) {
		return status.Error(codes.Unimplemented, err.Error())
	}

	return status.Error(codes.Unauthenticated, err.Error())
}

func storeVTXOStatusFromRPC(st arkrpc.VTXOStatus) string {
	switch st {
	case arkrpc.VTXOStatus_VTXO_STATUS_UNCONFIRMED:
		return storeVTXOStatusPending

	case arkrpc.VTXOStatus_VTXO_STATUS_LIVE:
		return storeVTXOStatusLive

	case arkrpc.VTXOStatus_VTXO_STATUS_FORFEITING:
		return storeVTXOStatusInFlight

	case arkrpc.VTXOStatus_VTXO_STATUS_FORFEITED:
		return storeVTXOStatusForfeited

	case arkrpc.VTXOStatus_VTXO_STATUS_SPENT:
		return storeVTXOStatusSpent

	default:
		return storeVTXOStatusPending
	}
}

func cloneOutPoint(op *arkrpc.OutPoint) *arkrpc.OutPoint {
	if op == nil {
		return nil
	}

	return &arkrpc.OutPoint{
		Txid: append([]byte(nil), op.Txid...),
		Vout: op.Vout,
	}
}

// insertRecipientEvent inserts a per-script monotonic recipient event id with
// retry on unique-constraint races.
func (s *Service) insertRecipientEvent(ctx context.Context,
	pkScript []byte, sessionDbID, outputIndex int32,
	value, createdAt int64) (uint64, error) {

	const maxRecipientInsertAttempts = 32

	nextID, err := s.store.Queries.GetMaxOORRecipientEventID(ctx, pkScript)
	if err != nil {
		return 0, fmt.Errorf("get max recipient event id: %w", err)
	}

	nextID++

	for i := 0; i < maxRecipientInsertAttempts; i++ {
		_, err = s.store.Queries.InsertOORRecipientEvent(
			ctx,
			sqlc.InsertOORRecipientEventParams{
				RecipientPkScript: append([]byte(nil),
					pkScript...),
				EventID:     nextID,
				SessionDbID: sessionDbID,
				OutputIndex: outputIndex,
				Value:       value,
				CreatedAt:   createdAt,
			},
		)
		if err == nil {
			return uint64(nextID), nil
		}

		mapped := db.MapSQLError(err)
		var uniqueErr *db.ErrSQLUniqueConstraintViolation
		if errors.As(mapped, &uniqueErr) {
			nextID++
			continue
		}

		return 0, mapped
	}

	return 0, fmt.Errorf(
		"unable to insert recipient event after %d attempts",
		maxRecipientInsertAttempts,
	)
}
