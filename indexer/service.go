package indexer

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo/rounds"
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

	store Store

	authorizer ScriptAuthorizer

	proofConfig atomic.Pointer[taprootProofVerificationConfig]
}

// NewService creates a new indexer service.
//
// The default script authorization policy is allow-all. Callers MUST call
// SetScriptAuthorizer with a restrictive policy (e.g.
// RegistrationScriptAuthorizer) before serving production traffic. The
// server's setupIndexerSubsystem wires this automatically.
func NewService(serverID string, store Store) *Service {
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

// SetVTXOProofPolicy configures verification of owner-key proofs for
// the standardized VTXO tapscript used by Ark receive outputs. Safe
// for concurrent use with RPC handlers that read the config.
func (s *Service) SetVTXOProofPolicy(operatorKey *btcec.PublicKey,
	exitDelay uint32) {

	s.proofConfig.Store(&taprootProofVerificationConfig{
		vtxoOperatorKey: operatorKey,
		vtxoExitDelay:   exitDelay,
	})
}

// loadProofConfig returns the current proof verification config. If
// SetVTXOProofPolicy has not been called, an empty config is returned
// which disables the VTXO tapscript verification path.
func (s *Service) loadProofConfig() taprootProofVerificationConfig {
	cfg := s.proofConfig.Load()
	if cfg == nil {
		return taprootProofVerificationConfig{}
	}

	return *cfg
}

// authorizeScripts applies the configured script authorization policy.
//
// The authorizer is always non-nil: NewService installs AllowAll and
// SetScriptAuthorizer replaces nil with AllowAll.
func (s *Service) authorizeScripts(ctx context.Context,
	principalMailboxID string, purpose string, pkScripts [][]byte) error {

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
		return nil, status.Error(
			codes.Unauthenticated, "missing mailbox principal",
		)
	}

	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "nil request")
	}

	pkScript := req.GetPkScript()
	if len(pkScript) == 0 {
		return nil, status.Error(
			codes.InvalidArgument, "missing pk_script",
		)
	}

	now := s.now()
	proofCfg := s.loadProofConfig()

	var (
		err      error
		proofMsg *receiveScriptProofMessage
	)

	switch proof := req.Proof.(type) {
	case *arkrpc.RegisterReceiveScriptRequest_TaprootSchnorr:
		if proof.TaprootSchnorr == nil {
			return nil, status.Error(
				codes.InvalidArgument,
				"missing taproot_schnorr proof",
			)
		}

		proofMsg, err = parseReceiveScriptProofMessage(
			proof.TaprootSchnorr.GetMessage(),
		)
		if err != nil {
			return nil, status.Error(
				codes.Unauthenticated, err.Error(),
			)
		}

		err = verifyTaprootSchnorrProof(
			now, pkScript, proof.TaprootSchnorr, s.serverID,
			principal.MailboxID, purposeRegisterReceiveScript,
			proofCfg,
		)
		if err != nil {
			return nil, status.Error(
				codes.Unauthenticated, err.Error(),
			)
		}

	case *arkrpc.RegisterReceiveScriptRequest_Bip322:
		return nil, status.Error(
			codes.Unimplemented, "bip322 proofs not implemented",
		)

	default:
		return nil, status.Error(codes.InvalidArgument,
			"missing proof")
	}

	expiresAt := req.ExpiresAtUnixS
	if expiresAt == 0 {
		// Mirror the proof message expiry if the request doesn't
		// specify a retention time. This keeps the transport simple
		// while still bounding replay windows.
		if proofMsg != nil && proofMsg.ExpiresAt > 0 {
			expiresAt = proofMsg.ExpiresAt
		}
	}

	var ownerPubKey, operatorPubKey []byte
	var exitDelay uint32
	if proofMsg != nil {
		var matches bool
		matches, err = matchesStandardVTXOReceiveScript(
			pkScript, proofMsg.OwnerPubKey, proofCfg,
		)
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		if matches {
			ownerPubKey = append(
				[]byte(nil), proofMsg.OwnerPubKey...,
			)
			operatorPubKey = proofCfg.vtxoOperatorKey.
				SerializeCompressed()
			exitDelay = proofCfg.vtxoExitDelay
		}
	}

	if s.store == nil {
		return nil, status.Error(
			codes.FailedPrecondition,
			"indexer database not configured",
		)
	}

	err = s.store.UpsertReceiveScript(
		ctx, principal.MailboxID,
		append(
			[]byte(nil), pkScript...,
		),
		time.Unix(
			int64(expiresAt), 0,
		),
		req.Label,
		s.now(),
		ownerPubKey,
		operatorPubKey,
		exitDelay,
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
		return nil, status.Error(
			codes.Unauthenticated, "missing mailbox principal",
		)
	}

	if s.store == nil {
		return nil, status.Error(
			codes.FailedPrecondition,
			"indexer database not configured",
		)
	}

	scripts, err := s.store.ListActiveReceiveScriptsByPrincipal(
		ctx, principal.MailboxID, s.now(),
	)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	var out []*arkrpc.RegisteredReceiveScript
	for _, reg := range scripts {
		out = append(out, &arkrpc.RegisteredReceiveScript{
			PkScript:       append([]byte(nil), reg.PkScript...),
			ExpiresAtUnixS: uint64(reg.ExpiresAt.Unix()),
			Label:          reg.Label,
		})
	}

	sort.Slice(out, func(i, j int) bool {
		return bytes.Compare(out[i].PkScript, out[j].PkScript) < 0
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
		return nil, status.Error(
			codes.Unauthenticated, "missing mailbox principal",
		)
	}

	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "nil request")
	}

	pkScript := req.GetPkScript()
	if len(pkScript) == 0 {
		return nil, status.Error(
			codes.InvalidArgument, "missing pk_script",
		)
	}

	now := s.now()

	switch proof := req.Proof.(type) {
	case *arkrpc.UnregisterReceiveScriptRequest_TaprootSchnorr:
		err := verifyTaprootSchnorrProof(
			now, pkScript, proof.TaprootSchnorr, s.serverID,
			principal.MailboxID, purposeUnregisterReceiveScript,
			s.loadProofConfig(),
		)
		if err != nil {
			return nil, status.Error(
				codes.Unauthenticated, err.Error(),
			)
		}

	case *arkrpc.UnregisterReceiveScriptRequest_Bip322:
		return nil, status.Error(
			codes.Unimplemented, "bip322 proofs not implemented",
		)

	default:
		return nil, status.Error(codes.InvalidArgument,
			"missing proof")
	}

	if s.store == nil {
		return nil, status.Error(
			codes.FailedPrecondition,
			"indexer database not configured",
		)
	}

	_, err := s.store.DeleteReceiveScript(
		ctx, principal.MailboxID,
		append(
			[]byte(nil), pkScript...,
		),
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
		return nil, status.Error(
			codes.Unauthenticated, "missing mailbox principal",
		)
	}

	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "nil request")
	}

	pkScript := req.PkScript
	if len(pkScript) == 0 {
		return nil, status.Error(
			codes.InvalidArgument, "missing pk_script",
		)
	}

	now := s.now()

	switch proof := req.Proof.(type) {
	case *arkrpc.ListOORRecipientEventsByScriptRequest_TaprootSchnorr:
		if err := verifyTaprootSchnorrScopeProof(
			now, pkScript, proof.TaprootSchnorr, s.serverID,
			principal.MailboxID, purposeOORRecipientEvents,
			s.loadProofConfig(),
		); err != nil {
			return nil, scopeProofToStatus(err)
		}

	case *arkrpc.ListOORRecipientEventsByScriptRequest_Bip322:
		return nil, status.Error(
			codes.Unimplemented, "bip322 proofs not implemented",
		)

	default:
		return nil, status.Error(codes.InvalidArgument,
			"missing proof")
	}

	limit := req.Limit
	if limit == 0 {
		limit = defaultRecipientEventLimit
	}
	if limit > maxQueryLimit {
		limit = maxQueryLimit
	}

	if s.store == nil {
		return nil, status.Error(
			codes.FailedPrecondition,
			"indexer database not configured",
		)
	}

	var (
		out        []*arkrpc.OORRecipientEvent
		nextCursor uint64
	)

	// Run the multi-query flow inside a read transaction so that
	// the event list and per-session checkpoint fetches see a
	// consistent snapshot.
	err := s.store.ExecReadTx(ctx, func(q Store) error {
		rows, err := q.ListOORRecipientEventsAfterWithSession(
			ctx,
			append(
				[]byte(nil), pkScript...,
			),
			int64(req.AfterEventId),
			int32(limit),
		)
		if err != nil {
			return err
		}

		for _, row := range rows {
			ancestors, ancestorErr :=
				loadOORAncestorPackages(
					ctx, q, row.SessionID,
				)
			if ancestorErr != nil {
				return ancestorErr
			}

			ev := &arkrpc.OORRecipientEvent{
				RecipientPkScript: append(
					[]byte(nil), row.RecipientPkScript...,
				),
				EventId: uint64(row.EventID),
				SessionId: append(
					[]byte(nil), row.SessionID...,
				),
				OutputIndex: uint32(row.OutputIndex),
				Value:       uint64(row.Value),
				ArkPsbt: append(
					[]byte(nil), row.ArkPsbt...,
				),
				AncestorPackages: ancestors,
			}

			checkpoints, cpErr :=
				q.GetOORSessionCheckpoints(
					ctx, row.SessionID,
				)
			if cpErr != nil {
				return fmt.Errorf("get oor checkpoints: %w",
					cpErr)
			}

			cpPSBTs := make(
				[][]byte, 0, len(checkpoints),
			)
			for _, cp := range checkpoints {
				cpPSBTs = append(
					cpPSBTs,
					append(
						[]byte(nil),
						cp.CheckpointPsbt...,
					),
				)
			}

			ev.CheckpointPsbts = cpPSBTs

			out = append(out, ev)
			nextCursor = ev.EventId
		}

		return nil
	})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &arkrpc.ListOORRecipientEventsByScriptResponse{
		Events:     out,
		NextCursor: nextCursor,
	}, nil
}

// loadOORAncestorPackages returns finalized OOR packages that produced
// OOR inputs consumed by the supplied session. Results are ordered
// ancestor-first so receive-side persistence can replay the chain in
// dependency order.
//
// Both the depth bound and cycle protection are inherited from
// walkOORSessionAncestryDriver, which is also consumed by the cap-
// arithmetic path; sharing the driver guarantees a chain that the
// recipient path rejects cannot silently pass the cap path.
func loadOORAncestorPackages(ctx context.Context, q Store,
	sessionID []byte) ([]*arkrpc.OORSessionPackage, error) {

	ancestors := make([]*arkrpc.OORSessionPackage, 0)

	// Sessions discovered in pre-order are cached so post-order can
	// emit their packages without re-fetching the row.
	sessionByID := make(map[string]OORSession)

	pre := func(ctx context.Context, curID []byte, _ int) ([][]byte,
		error) {

		checkpoints, err := q.GetOORSessionCheckpoints(ctx, curID)
		if err != nil {
			return nil, err
		}

		parentIDs, err := checkpointParentSessionIDs(checkpoints)
		if err != nil {
			return nil, err
		}

		// Filter parents to those with persisted sessions and
		// stash each session so the post visitor can build its
		// package without a second store hit.
		keepers := make([][]byte, 0, len(parentIDs))
		for _, parentID := range parentIDs {
			session, err := q.GetOORSession(ctx, parentID)
			if errors.Is(err, ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}

			sessionByID[hex.EncodeToString(parentID)] = session
			keepers = append(keepers, parentID)
		}

		return keepers, nil
	}

	post := func(ctx context.Context, curID []byte, depth int) error {
		// Skip the root: only its ancestors are emitted.
		if depth == 0 {
			return nil
		}

		session, ok := sessionByID[hex.EncodeToString(curID)]
		if !ok {
			return nil
		}

		pkg, err := rpcOORSessionPackage(ctx, q, curID, session)
		if err != nil {
			return err
		}

		ancestors = append(ancestors, pkg)

		return nil
	}

	if err := walkOORSessionAncestryDriver(
		ctx, sessionID, pre, post,
	); err != nil {
		return nil, err
	}

	return ancestors, nil
}

// checkpointParentSessionIDs extracts checkpoint input txids. When a parent is
// OOR-created, that txid is also the producing session id.
func checkpointParentSessionIDs(checkpoints []OORSessionCheckpoint) ([][]byte,
	error) {

	seen := make(map[string]struct{}, len(checkpoints))
	parentIDs := make([][]byte, 0, len(checkpoints))

	for i := range checkpoints {
		tx, err := parsePsbtTx(checkpoints[i].CheckpointPsbt)
		if err != nil {
			return nil, fmt.Errorf("parse checkpoint %d: %w", i,
				err)
		}

		for _, txIn := range tx.TxIn {
			parentID := txIn.PreviousOutPoint.Hash
			key := parentID.String()
			if _, ok := seen[key]; ok {
				continue
			}

			seen[key] = struct{}{}
			parentIDs = append(
				parentIDs,
				append(
					[]byte(nil), parentID[:]...,
				),
			)
		}
	}

	return parentIDs, nil
}

// rpcOORSessionPackage builds the RPC artifact payload for one OOR session.
func rpcOORSessionPackage(ctx context.Context, q Store, sessionID []byte,
	session OORSession) (*arkrpc.OORSessionPackage, error) {

	checkpoints, err := q.GetOORSessionCheckpoints(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	cpPSBTs := make([][]byte, 0, len(checkpoints))
	for _, cp := range checkpoints {
		cpPSBTs = append(
			cpPSBTs,
			append(
				[]byte(nil), cp.CheckpointPsbt...,
			),
		)
	}

	return &arkrpc.OORSessionPackage{
		SessionId:       append([]byte(nil), sessionID...),
		ArkPsbt:         append([]byte(nil), session.ArkPsbt...),
		CheckpointPsbts: cpPSBTs,
	}, nil
}

// ListVTXOsByScripts returns VTXOs matching the provided scripts, gated by
// proof-of-control for each script.
func (s *Service) ListVTXOsByScripts(ctx context.Context,
	req *arkrpc.ListVTXOsByScriptsRequest) (
	*arkrpc.ListVTXOsByScriptsResponse, error) {

	principal, ok := PrincipalFromContext(ctx)
	if !ok || principal.MailboxID == "" {
		return nil, status.Error(
			codes.Unauthenticated, "missing mailbox principal",
		)
	}

	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "nil request")
	}

	if len(req.Scripts) == 0 {
		return nil, status.Error(
			codes.InvalidArgument, "missing scripts",
		)
	}
	if len(req.Scripts) > maxScriptsPerRequest {
		return nil, status.Errorf(codes.InvalidArgument, "too many "+
			"scripts: %d (max %d)", len(req.Scripts),
			maxScriptsPerRequest)
	}

	now := s.now()

	if s.store == nil {
		return nil, status.Error(
			codes.FailedPrecondition,
			"indexer database not configured",
		)
	}

	authQuery, err := s.authorizeScriptScopeQuery(
		ctx, s.store, now, principal.MailboxID, req.Scripts,
		purposeListVTXOsByScripts,
	)
	if err != nil {
		return nil, scopeProofToStatus(err)
	}
	allowedScriptBytes := authQuery.AllowedScriptBytes
	allowedScripts := make(map[string]struct{}, len(allowedScriptBytes))
	for i := 0; i < len(allowedScriptBytes); i++ {
		allowedScripts[hex.EncodeToString(allowedScriptBytes[i])] =
			struct{}{}
	}

	statusFilter := make([]string, 0, len(req.StatusFilter))
	for _, st := range req.StatusFilter {
		storeStatus, err := storeVTXOStatusFromRPC(st)
		if err != nil {
			return nil, status.Error(
				codes.InvalidArgument, err.Error(),
			)
		}

		statusFilter = append(statusFilter, storeStatus)
	}

	after, err := decodeVTXOCursor(req.Cursor)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	limit := req.Limit
	if limit == 0 {
		limit = defaultVTXOLimit
	}
	if limit > maxQueryLimit {
		limit = maxQueryLimit
	}

	// Run the entire VTXO query + lineage resolution inside a read
	// transaction so all queries see a consistent snapshot. The
	// lineage resolver's recursive round/session/checkpoint lookups
	// all run within the same transaction.
	var (
		out        []*arkrpc.VTXO
		nextCursor []byte
	)
	err = s.store.ExecReadTx(ctx, func(q Store) error {
		rows, qErr := q.ListVTXOsByPkScriptsAfter(
			ctx, allowedScriptBytes, statusFilter, after,
			int32(limit+1),
		)
		if qErr != nil {
			return qErr
		}

		pageLimit := int(limit)
		hasMore := len(rows) > pageLimit
		if hasMore {
			rows = rows[:pageLimit]
			lastOutpoint := rows[len(rows)-1].Outpoint
			nextCursor = encodeVTXOCursor(lastOutpoint)
		}

		// Collect unique round IDs for batch metadata fetch.
		roundIDSet := make(map[rounds.RoundID]struct{})
		for _, row := range rows {
			if row.RoundID == nil {
				continue
			}

			roundIDSet[*row.RoundID] = struct{}{}
		}

		roundRowByID := make(
			map[rounds.RoundID]*RoundRow, len(roundIDSet),
		)

		if len(roundIDSet) > 0 {
			uniqueIDs := make(
				[]rounds.RoundID, 0, len(roundIDSet),
			)
			for id := range roundIDSet {
				uniqueIDs = append(uniqueIDs, id)
			}

			roundRows, lErr := q.ListRoundsByIDs(
				ctx, uniqueIDs,
			)
			if lErr != nil {
				return lErr
			}

			for i := range roundRows {
				rr := &roundRows[i]
				roundRowByID[rr.RoundID] = rr
			}
		}

		lineage := newLineageResolver(q, roundRowByID)

		for _, row := range rows {
			scriptHex := hex.EncodeToString(row.PkScript)
			if _, ok := allowedScripts[scriptHex]; !ok {
				continue
			}

			var rr *RoundRow
			if row.RoundID != nil {
				rr = roundRowByID[*row.RoundID]
			}

			vtxo, vErr := rpcVTXOFromDB(row, rr)
			if vErr != nil {
				return vErr
			}

			lineageMeta, lErr := lineage.Resolve(
				ctx, row,
			)
			if lErr != nil {
				return lErr
			}
			if mErr := applyLineageMetadata(
				vtxo, lineageMeta,
			); mErr != nil {
				return mErr
			}

			if vtxo.Status == arkrpc.VTXOStatus_VTXO_STATUS_SPENT {
				spentByTxid, sErr := lineage.resolveSpentByTxid(
					ctx, row.Outpoint,
				)
				if sErr != nil {
					return sErr
				}

				if len(spentByTxid) > 0 {
					vtxo.SpentByTxid = spentByTxid
				}
			}

			out = append(out, vtxo)
		}

		return nil
	})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &arkrpc.ListVTXOsByScriptsResponse{
		Vtxos:      out,
		NextCursor: nextCursor,
	}, nil
}

// GetOORSessionByTxid returns the Ark package and finalized checkpoints for
// an OOR session identified by its deterministic txid, gated by proof of a
// script that the session consumed.
func (s *Service) GetOORSessionByTxid(ctx context.Context,
	req *arkrpc.GetOORSessionByTxidRequest) (
	*arkrpc.GetOORSessionByTxidResponse, error) {

	principal, ok := PrincipalFromContext(ctx)
	if !ok || principal.MailboxID == "" {
		return nil, status.Error(
			codes.Unauthenticated, "missing mailbox principal",
		)
	}

	principalMailboxID := principal.MailboxID

	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "nil request")
	}

	if req.Script == nil || len(req.Script.PkScript) == 0 {
		return nil, status.Error(
			codes.InvalidArgument, "missing pk_script",
		)
	}
	if len(req.SessionTxid) == 0 {
		return nil, status.Error(
			codes.InvalidArgument, "missing session_txid",
		)
	}

	if s.store == nil {
		return nil, status.Error(
			codes.FailedPrecondition,
			"indexer database not configured",
		)
	}

	// Route the single-scope request through the same two-step
	// authorizer the list-based RPCs use. Wrapping the single
	// ScriptScope into a one-element slice means this path cannot
	// drift away from the list-based path's authorization rules
	// (proof verification + row-based policy-auth + registration
	// fallback) as those rules evolve.
	_, err := s.authorizeScriptScopeQuery(
		ctx, s.store, s.now(), principalMailboxID,
		[]*arkrpc.ScriptScope{req.Script}, purposeGetOORSessionByTxid,
	)
	if err != nil {
		return nil, scopeProofToStatus(err)
	}

	var resp *arkrpc.GetOORSessionByTxidResponse
	err = s.store.ExecReadTx(ctx, func(q Store) error {
		ok, err := q.OORSessionSpendsScript(
			ctx, req.SessionTxid, req.Script.PkScript,
		)
		if err != nil {
			return err
		}
		if !ok {
			return ErrNotFound
		}

		session, err := q.GetOORSession(ctx, req.SessionTxid)
		if err != nil {
			return err
		}

		checkpoints, err := q.GetOORSessionCheckpoints(
			ctx, req.SessionTxid,
		)
		if err != nil {
			return err
		}

		cpPSBTs := make([][]byte, 0, len(checkpoints))
		for _, cp := range checkpoints {
			cpPSBTs = append(
				cpPSBTs,
				append(
					[]byte(nil), cp.CheckpointPsbt...,
				),
			)
		}

		resp = &arkrpc.GetOORSessionByTxidResponse{
			ArkPsbt: append(
				[]byte(nil), session.ArkPsbt...,
			),
			CheckpointPsbts: cpPSBTs,
		}

		return nil
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, status.Error(codes.NotFound, err.Error())
		}

		return nil, status.Error(codes.Internal, err.Error())
	}

	return resp, nil
}

// GetSubtreeByScripts returns a minimal spanning subtree for all leaves
// matching the provided scripts.
func (s *Service) GetSubtreeByScripts(ctx context.Context,
	req *arkrpc.GetSubtreeByScriptsRequest) (
	*arkrpc.GetSubtreeByScriptsResponse, error) {

	principal, ok := PrincipalFromContext(ctx)
	if !ok || principal.MailboxID == "" {
		return nil, status.Error(
			codes.Unauthenticated, "missing mailbox principal",
		)
	}

	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "nil request")
	}

	if len(req.Scripts) == 0 {
		return nil, status.Error(
			codes.InvalidArgument, "missing scripts",
		)
	}
	if len(req.Scripts) > maxScriptsPerRequest {
		return nil, status.Errorf(codes.InvalidArgument, "too many "+
			"scripts: %d (max %d)", len(req.Scripts),
			maxScriptsPerRequest)
	}

	now := s.now()

	if s.store == nil {
		return nil, status.Error(
			codes.FailedPrecondition,
			"indexer database not configured",
		)
	}

	authQuery, err := s.authorizeScriptScopeQuery(
		ctx, s.store, now, principal.MailboxID, req.Scripts,
		purposeGetSubtreeByScripts,
	)
	if err != nil {
		return nil, scopeProofToStatus(err)
	}
	allowedScriptBytes := authQuery.AllowedScriptBytes
	allowedScripts := make(map[string]struct{}, len(allowedScriptBytes))
	for i := 0; i < len(allowedScriptBytes); i++ {
		allowedScripts[hex.EncodeToString(allowedScriptBytes[i])] =
			struct{}{}
	}

	// Run the subtree query + tree extraction + virtual leaf
	// enrichment inside a read transaction so all queries see a
	// consistent snapshot.
	var inputs *subtreeDBInputs

	nodesByTxid := make(map[string]*arkrpc.TreeNode)
	edgesByKey := make(map[string]*arkrpc.TreeEdge)
	leafTXByTxid := make(map[string][]byte)

	err = s.store.ExecReadTx(ctx, func(q Store) error {
		var loadErr error
		inputs, loadErr = loadSubtreeInputs(
			ctx, q, allowedScripts, allowedScriptBytes,
		)
		if loadErr != nil {
			return loadErr
		}

		for key, targets := range inputs.targetOutpointsByTree {
			roundID := inputs.roundIDByHex[key.roundIDHex]

			fullTree, tErr := q.LoadVTXOTree(
				ctx, roundID, key.batchIdx,
			)
			if tErr != nil {
				return tErr
			}

			extracted, eErr := extractTreeForOutpoints(
				fullTree, targets,
			)
			if eErr != nil {
				return eErr
			}

			leafTXs, lErr := collectLeafProofTXs(extracted)
			if lErr != nil {
				return lErr
			}
			for txid, serializedLeafTX := range leafTXs {
				leafTXByTxid[txid] = serializedLeafTX
			}

			if rErr := recordSubtreeRPCView(
				extracted, req.IncludeInternalNodes,
				inputs.leafTxids, nodesByTxid, edgesByKey,
			); rErr != nil {
				return rErr
			}
		}

		return enrichVirtualLeafProofs(
			ctx, q, inputs.virtualLeaves,
		)
	})
	if err != nil {
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
			hex.EncodeToString(outEdges[i].ChildTxid))
		aj := fmt.Sprintf("%s:%d:%s",
			hex.EncodeToString(outEdges[j].ParentTxid),
			outEdges[j].ParentOutputIndex,
			hex.EncodeToString(outEdges[j].ChildTxid))

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
		return nil, status.Error(
			codes.Unauthenticated, "missing mailbox principal",
		)
	}

	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "nil request")
	}

	if len(req.Scripts) == 0 {
		return nil, status.Error(
			codes.InvalidArgument, "missing scripts",
		)
	}
	if len(req.Scripts) > maxScriptsPerRequest {
		return nil, status.Errorf(codes.InvalidArgument, "too many "+
			"scripts: %d (max %d)", len(req.Scripts),
			maxScriptsPerRequest)
	}

	now := s.now()

	limit := req.Limit
	if limit == 0 {
		limit = defaultVTXOEventLimit
	}
	if limit > maxQueryLimit {
		limit = maxQueryLimit
	}

	if s.store == nil {
		return nil, status.Error(
			codes.FailedPrecondition,
			"indexer database not configured",
		)
	}

	authQuery, err := s.authorizeScriptScopeQuery(
		ctx, s.store, now, principal.MailboxID, req.Scripts,
		purposeListVTXOEventsByScripts,
	)
	if err != nil {
		return nil, scopeProofToStatus(err)
	}
	allowedScriptBytes := authQuery.AllowedScriptBytes

	rows, err := s.store.ListVTXOEventsAfterByScripts(
		ctx, int64(req.AfterEventId), allowedScriptBytes, int32(limit),
	)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	var out []*arkrpc.VTXOEvent
	var nextCursor uint64
	for _, row := range rows {
		event := &arkrpc.VTXOEvent{
			EventId: uint64(row.EventID),
			Type:    vtxoEventTypeFromStore(row.EventType),
			Outpoint: &arkrpc.OutPoint{
				Txid: row.Outpoint.Hash[:],
				Vout: row.Outpoint.Index,
			},
			Status:          VTXOStatusFromStore(row.Status),
			CreatedAtUnixMs: row.CreatedAt.UnixMilli(),
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
	ev *arkrpc.OORRecipientEvent) (*arkrpc.IncomingOOREvent, []string,
	error) {

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

	q := s.store

	storedEvent, err := q.GetOORRecipientEventBySessionOutput(
		ctx,
		append(
			[]byte(nil), ev.RecipientPkScript...,
		),
		append(
			[]byte(nil), ev.SessionId...,
		),
		int32(ev.OutputIndex),
	)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, nil, fmt.Errorf("get existing recipient event: %w",
			err)
	}

	if errors.Is(err, ErrNotFound) {
		// Look up the OOR session's internal DB ID from the
		// session_id bytes so we can reference it as a foreign key
		// in the recipient event row.
		sessionRow, sessionErr := q.GetOORSession(
			ctx,
			append(
				[]byte(nil), ev.SessionId...,
			),
		)
		if sessionErr != nil {
			return nil, nil, fmt.Errorf("get oor session: %w",
				sessionErr)
		}

		eventID, insertErr := s.insertRecipientEvent(
			ctx,
			append(
				[]byte(nil), ev.RecipientPkScript...,
			),
			int32(sessionRow.ID),
			int32(ev.OutputIndex),
			int64(ev.Value),
			s.now(),
		)
		if insertErr != nil {
			// If the CAS loop exhausted retries, it may be
			// because a concurrent goroutine already inserted
			// the row for this exact VTXO (same
			// session+output). Re-check once: if the row now
			// exists, the event was successfully stored by the
			// other writer and we can return it.
			storedEvent, err = q.GetOORRecipientEventBySessionOutput( //nolint:ll
				ctx,
				append([]byte(nil), ev.RecipientPkScript...),
				append([]byte(nil), ev.SessionId...),
				int32(ev.OutputIndex),
			)
			if err == nil {
				// Concurrent insert won — use its row.
				goto buildResponse
			}

			return nil, nil, insertErr
		}

		storedEvent, err = q.GetOORRecipientEventBySessionOutput(
			ctx,
			append(
				[]byte(nil), ev.RecipientPkScript...,
			),
			append(
				[]byte(nil), ev.SessionId...,
			),
			int32(ev.OutputIndex),
		)
		if err != nil {
			return nil, nil, fmt.Errorf("load inserted recipient "+
				"event: %w", err)
		}
		// Use the event ID from our insert, which is the
		// authoritative monotonic ID for this recipient script.
		storedEvent.EventID = int64(eventID)
	}

buildResponse:
	rows, err := q.ListActiveReceivePrincipalsByScript(
		ctx,
		append(
			[]byte(nil), ev.RecipientPkScript...,
		),
		s.now(),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("list active principals: %w", err)
	}

	principals := make([]string, 0, len(rows))
	for _, row := range rows {
		principals = append(principals, row.PrincipalMailboxID)
	}

	return &arkrpc.IncomingOOREvent{
		RecipientPkScript: append(
			[]byte(nil), storedEvent.RecipientPkScript...,
		),
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
	st arkrpc.VTXOStatus, valueSat uint64, roundID string,
	batchExpiry int32, relativeExpiry uint32, origin arkrpc.VTXOOrigin,
	commitmentTxid []byte) (*arkrpc.IncomingVTXOEvent, []string, error) {

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

	var op wire.OutPoint
	copy(op.Hash[:], outpoint.Txid)
	op.Index = outpoint.Vout

	vtxoStatus, err := storeVTXOStatusFromRPC(st)
	if err != nil {
		return nil, nil, fmt.Errorf("map vtxo status: %w", err)
	}

	insertedEventID, err := s.store.InsertVTXOEvent(
		ctx,
		append([]byte(nil), pkScript...),
		vtxoEventTypeToStore(evType),
		op,
		vtxoStatus,
		now,
		VTXOEventMetadata{
			ValueSat:          valueSat,
			RoundID:           roundID,
			BatchExpiryHeight: batchExpiry,
			RelativeExpiry:    relativeExpiry,
			Origin:            origin.String(),
			CommitmentTxid: append(
				[]byte(nil), commitmentTxid...,
			),
		},
	)
	if err != nil {
		return nil, nil, fmt.Errorf("insert vtxo event: %w", err)
	}

	rows, err := s.store.ListActiveReceivePrincipalsByScript(
		ctx,
		append(
			[]byte(nil), pkScript...,
		),
		now,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("list active principals: %w", err)
	}

	principals := make([]string, 0, len(rows))
	for _, row := range rows {
		principals = append(principals, row.PrincipalMailboxID)
	}

	return &arkrpc.IncomingVTXOEvent{
		EventId:           uint64(insertedEventID),
		Type:              evType,
		Outpoint:          cloneOutPoint(outpoint),
		Status:            st,
		PkScript:          append([]byte(nil), pkScript...),
		ValueSat:          valueSat,
		RoundId:           roundID,
		BatchExpiryHeight: batchExpiry,
		RelativeExpiry:    relativeExpiry,
		Origin:            origin,
		CommitmentTxid:    append([]byte(nil), commitmentTxid...),
	}, principals, nil
}

// outpointKey returns a stable string key for an RPC outpoint.
//
// Nil outpoints map to the empty string so callers can use this helper in
// de-duplication maps without a separate nil check.
func outpointKey(op *arkrpc.OutPoint) string {
	if op == nil {
		return ""
	}

	return fmt.Sprintf("%s:%d", hex.EncodeToString(op.Txid), op.Vout)
}

const vtxoCursorLen = chainhash.HashSize + 4

// encodeVTXOCursor serializes an outpoint into the opaque ListVTXOsByScripts
// keyset cursor format.
func encodeVTXOCursor(outpoint wire.OutPoint) []byte {
	cursor := make([]byte, vtxoCursorLen)
	copy(cursor[:chainhash.HashSize], outpoint.Hash[:])
	binary.BigEndian.PutUint32(
		cursor[chainhash.HashSize:], outpoint.Index,
	)

	return cursor
}

// decodeVTXOCursor parses the opaque ListVTXOsByScripts keyset cursor.
func decodeVTXOCursor(cursor []byte) (*wire.OutPoint, error) {
	if len(cursor) == 0 {
		return nil, nil
	}

	if len(cursor) != vtxoCursorLen {
		return nil, fmt.Errorf("invalid cursor length: %d", len(cursor))
	}

	var outpoint wire.OutPoint
	copy(outpoint.Hash[:], cursor[:chainhash.HashSize])
	outpoint.Index = binary.BigEndian.Uint32(cursor[chainhash.HashSize:])
	if outpoint.Index > math.MaxInt32 {
		return nil, fmt.Errorf("invalid cursor outpoint index: %d",
			outpoint.Index)
	}

	return &outpoint, nil
}

// scopeProofToStatus maps proof-validation errors to the RPC status codes
// expected by the script-scoped query surface.
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

// storeVTXOStatusFromRPC converts the public RPC status enum into the string
// representation persisted by the backing store.
func storeVTXOStatusFromRPC(st arkrpc.VTXOStatus) (string, error) {
	switch st {
	case arkrpc.VTXOStatus_VTXO_STATUS_UNCONFIRMED:
		return storeVTXOStatusPending, nil

	case arkrpc.VTXOStatus_VTXO_STATUS_LIVE:
		return storeVTXOStatusLive, nil

	case arkrpc.VTXOStatus_VTXO_STATUS_FORFEITING:
		return storeVTXOStatusInFlight, nil

	case arkrpc.VTXOStatus_VTXO_STATUS_FORFEITED:
		return storeVTXOStatusForfeited, nil

	case arkrpc.VTXOStatus_VTXO_STATUS_SPENT:
		return storeVTXOStatusSpent, nil

	default:
		return "", fmt.Errorf("unrecognized VTXOStatus: %v", st)
	}
}

// cloneOutPoint deep-copies an RPC outpoint so response assembly never
// aliases request or store-backed memory.
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
func (s *Service) insertRecipientEvent(ctx context.Context, pkScript []byte,
	sessionDBID, outputIndex int32, value int64, createdAt time.Time) (
	uint64, error) {

	// maxRecipientInsertAttempts bounds the CAS retry loop for allocating
	// a per-script monotonic event ID. Under contention multiple writers
	// may race for the same next ID; 32 attempts provides ample headroom
	// for realistic concurrency without spinning indefinitely.
	const maxRecipientInsertAttempts = 32

	nextID, err := s.store.GetMaxOORRecipientEventID(ctx, pkScript)
	if err != nil {
		return 0, fmt.Errorf("get max recipient event id: %w", err)
	}

	nextID++

	for i := 0; i < maxRecipientInsertAttempts; i++ {
		_, err = s.store.InsertOORRecipientEvent(
			ctx,
			append(
				[]byte(nil), pkScript...,
			),
			nextID,
			sessionDBID,
			outputIndex,
			value,
			createdAt,
		)
		if err == nil {
			return uint64(nextID), nil
		}

		if errors.Is(err, ErrUniqueViolation) {
			nextID++
			continue
		}

		return 0, err
	}

	return 0, fmt.Errorf("unable to insert recipient event after %d "+
		"attempts", maxRecipientInsertAttempts)
}
