package darepo

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btclog/v2"
	"github.com/google/uuid"
	"github.com/lightninglabs/darepo/adminrpc"
	"github.com/lightninglabs/darepo/build"
	"github.com/lightninglabs/darepo/db/sqlc"
	"github.com/lightninglabs/darepo/fees"
	"github.com/lightninglabs/darepo/metrics"
	"github.com/lightninglabs/darepo/rounds"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// AdminRPCServer is a gRPC server that serves admin/operator commands.
type AdminRPCServer struct {
	// Required by the grpc-gateway/v2 library for forward compatibility.
	adminrpc.UnimplementedOperatorAdminServer

	cfg        *AdminRPCConfig
	grpcServer *grpc.Server
	listener   net.Listener

	server *Server

	started uint32 // To be used atomically.
	stopped uint32 // To be used atomically.

	quit chan struct{}
	wg   sync.WaitGroup

	log btclog.Logger
}

// NewAdminRPCServer creates a new admin RPC server.
func NewAdminRPCServer(cfg *AdminRPCConfig, operator *Server,
	log btclog.Logger) (*AdminRPCServer, error) {

	// Use existing listener if provided, otherwise bind a new TCP
	// listener.
	listener := cfg.Listener
	if listener == nil {
		var err error
		listener, err = net.Listen("tcp", cfg.ListenAddr)
		if err != nil {
			return nil, fmt.Errorf("admin RPC server unable "+
				"to listen on %s: %w",
				cfg.ListenAddr, err)
		}
	}

	// TODO(security): Add macaroon-based auth or mTLS before
	// any non-regtest deployment. The admin server currently
	// binds to localhost with no authentication — any local
	// process can call TriggerBatch, enumerate VTXOs, etc.
	grpcMetrics := metrics.GRPCServerMetrics

	s := &AdminRPCServer{
		cfg:    cfg,
		server: operator,
		log:    log,
		grpcServer: grpc.NewServer(
			grpc.UnaryInterceptor(
				grpcMetrics.UnaryServerInterceptor(),
			),
			grpc.StreamInterceptor(
				grpcMetrics.StreamServerInterceptor(),
			),
		),
		listener: listener,
		quit:     make(chan struct{}),
	}

	// Register the admin RPC service.
	adminrpc.RegisterOperatorAdminServer(s.grpcServer, s)

	return s, nil
}

// Start starts the admin RPC server.
func (a *AdminRPCServer) Start(ctx context.Context) error {
	if !atomic.CompareAndSwapUint32(&a.started, 0, 1) {
		return nil
	}

	a.log.InfoS(ctx, "Starting Admin RPC server")

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()

		a.log.InfoS(ctx, "Admin RPC server listening",
			"addr", a.listener.Addr())

		err := a.grpcServer.Serve(a.listener)
		if err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			a.log.ErrorS(ctx, "Admin RPC server exited with error",
				err)
		}
	}()

	return nil
}

// Stop stops the admin RPC server.
func (a *AdminRPCServer) Stop(ctx context.Context) error {
	if !atomic.CompareAndSwapUint32(&a.stopped, 0, 1) {
		return nil
	}

	a.log.InfoS(ctx, "Stopping admin RPC server")

	close(a.quit)

	// Attempt a graceful shutdown so in-flight RPCs can
	// complete. Fall back to a hard stop after 5 seconds to
	// avoid blocking shutdown indefinitely.
	done := make(chan struct{})
	go func() {
		a.grpcServer.GracefulStop()
		close(done)
	}()

	select {
	case <-done:

	case <-time.After(5 * time.Second):
		a.grpcServer.Stop()
	}

	a.wg.Wait()

	return nil
}

// Addr returns the address the admin RPC server is listening on.
func (a *AdminRPCServer) Addr() net.Addr {
	if a.listener == nil {
		return nil
	}

	return a.listener.Addr()
}

// Info returns basic information about the operator server.
func (a *AdminRPCServer) Info(ctx context.Context,
	_ *adminrpc.InfoRequest) (*adminrpc.InfoResponse, error) {

	var (
		pubkey      string
		alias       string
		blockHeight uint32
	)

	if a.server.lnd != nil {
		alias = a.server.lnd.NodeAlias

		// Best-effort block height from the chain backend.
		if a.server.chainBackend != nil {
			height, _, err := a.server.chainBackend.BestBlock(
				ctx,
			)
			if err == nil {
				blockHeight = uint32(height)
			}
		}
	}

	// Use the Ark operator key (not LND node identity) as the
	// pubkey, since this is what clients use for boarding
	// addresses and round validation.
	if t := a.server.terms; t != nil {
		pubkey = fmt.Sprintf(
			"%x", t.OperatorKey.PubKey.SerializeCompressed(),
		)
	}

	return &adminrpc.InfoResponse{
		Version:     build.Version(),
		Pubkey:      pubkey,
		Network:     a.server.cfg.Network,
		BlockHeight: blockHeight,
		LndAlias:    alias,
	}, nil
}

// TriggerBatch manually triggers a new batch round by sending a
// TriggerBatchMsg to the rounds actor. The actor seals the current
// live round, preventing further registrations and advancing the
// FSM to batch building.
func (a *AdminRPCServer) TriggerBatch(ctx context.Context,
	_ *adminrpc.TriggerBatchRequest) (
	*adminrpc.TriggerBatchResponse, error) {

	roundsRef := a.server.roundsRef
	if roundsRef == nil {
		return nil, fmt.Errorf(
			"rounds subsystem not initialized",
		)
	}

	future := roundsRef.Ask(
		ctx, &rounds.TriggerBatchMsg{},
	)

	resp, err := future.Await(ctx).Unpack()
	if err != nil {
		return nil, fmt.Errorf("trigger batch: %w", err)
	}

	triggerResp, ok := resp.(*rounds.TriggerBatchResp)
	if !ok || triggerResp == nil {
		return &adminrpc.TriggerBatchResponse{
			Status: "sealed",
		}, nil
	}

	return &adminrpc.TriggerBatchResponse{
		Status:  "sealed",
		RoundId: triggerResp.RoundID.String(),
	}, nil
}

// GetRoundStatus returns observability detail for a live round:
// current FSM state, intent count, quote-phase counters, seal pass
// number, and quote expiry. Queries the rounds actor via a
// GetRoundStatusReq snapshot Ask. Returns an empty response with
// round_not_found=true when the FSM is not live for the given
// round_id (already finalized and cleaned up, or never created).
func (a *AdminRPCServer) GetRoundStatus(ctx context.Context,
	req *adminrpc.GetRoundStatusRequest) (
	*adminrpc.GetRoundStatusResponse, error) {

	roundsRef := a.server.roundsRef
	if roundsRef == nil {
		return nil, fmt.Errorf(
			"rounds subsystem not initialized",
		)
	}

	roundID, err := uuid.Parse(req.GetRoundId())
	if err != nil {
		return nil, fmt.Errorf(
			"invalid round_id %q: %w", req.GetRoundId(), err,
		)
	}

	future := roundsRef.Ask(
		ctx, &rounds.GetRoundStatusReq{
			RoundID: rounds.RoundID(roundID),
		},
	)

	resp, err := future.Await(ctx).Unpack()
	if err != nil {
		return nil, fmt.Errorf(
			"get round status: %w", err,
		)
	}

	statusResp, ok := resp.(*rounds.GetRoundStatusResp)
	if !ok || statusResp == nil {
		return &adminrpc.GetRoundStatusResponse{
			RoundId: req.GetRoundId(),
		}, nil
	}

	if statusResp.RoundNotFound {
		return nil, status.Errorf(
			codes.NotFound,
			"round %s not found", req.GetRoundId(),
		)
	}

	return &adminrpc.GetRoundStatusResponse{
		RoundId:         statusResp.RoundID.String(),
		StateName:       statusResp.StateName,
		IntentCount:     statusResp.IntentCount,
		QuotesSent:      statusResp.QuotesSent,
		QuotesAccepted:  statusResp.QuotesAccepted,
		QuotesRejected:  statusResp.QuotesRejected,
		QuotesTimedOut:  statusResp.QuotesTimedOut,
		CurrentSealPass: statusResp.CurrentSealPass,
		QuoteExpiresAt:  statusResp.QuoteExpiresAt,
	}, nil
}

// ListRounds returns a paginated list of past and active rounds.
// Pagination is performed server-side in the database via LIMIT/OFFSET.
func (a *AdminRPCServer) ListRounds(ctx context.Context,
	req *adminrpc.ListRoundsRequest) (
	*adminrpc.ListRoundsResponse, error) {

	if a.server.db == nil {
		return nil, fmt.Errorf("database not initialized")
	}

	q := a.server.db.Queries

	limit := int32(req.Limit)
	if limit == 0 {
		limit = 50
	}
	offset := int32(req.Offset)

	unspecified := adminrpc.RoundStatus_ROUND_STATUS_UNSPECIFIED

	// When a status filter is active, use the status-specific
	// query and count. Otherwise list all rounds.
	var (
		dbRounds []sqlc.Round
		total    int64
		err      error
	)

	if req.StatusFilter != unspecified {
		statusStr, err := mapRoundStatusToDBStr(
			req.StatusFilter,
		)
		if err != nil {
			return nil, err
		}

		dbRounds, err = q.ListRoundsByStatus(
			ctx, sqlc.ListRoundsByStatusParams{
				Status: statusStr,
				Limit:  limit,
				Offset: offset,
			},
		)
		if err != nil {
			return nil, fmt.Errorf(
				"list rounds by status: %w", err,
			)
		}

		total, err = q.CountRoundsByStatus(ctx, statusStr)
		if err != nil {
			return nil, fmt.Errorf(
				"count rounds by status: %w", err,
			)
		}
	} else {
		dbRounds, err = q.ListAllRounds(
			ctx, sqlc.ListAllRoundsParams{
				Limit:  limit,
				Offset: offset,
			},
		)
		if err != nil {
			return nil, fmt.Errorf(
				"list all rounds: %w", err,
			)
		}

		total, err = q.CountAllRounds(ctx)
		if err != nil {
			return nil, fmt.Errorf(
				"count all rounds: %w", err,
			)
		}
	}

	summaries := make(
		[]*adminrpc.RoundSummary, 0, len(dbRounds),
	)
	statsByID, err := a.roundStatsByID(ctx, q, dbRounds)
	if err != nil {
		return nil, fmt.Errorf("load round summary stats: %w", err)
	}

	for _, r := range dbRounds {
		summary, err := a.roundToSummary(
			r, statsByID[string(r.RoundID)],
		)
		if err != nil {
			return nil, fmt.Errorf("build round summary: %w", err)
		}

		summaries = append(summaries, summary)
	}

	return &adminrpc.ListRoundsResponse{
		Rounds: summaries,
		Total:  uint32(total),
	}, nil
}

// roundSummaryStats stores aggregate admin data for one persisted round.
type roundSummaryStats struct {
	numParticipants uint32
	totalValueSat   int64
}

// roundStatsByID loads participant counts and output totals for a page of
// rounds with one aggregate query.
func (a *AdminRPCServer) roundStatsByID(ctx context.Context,
	q *sqlc.Queries, rounds []sqlc.Round) (
	map[string]roundSummaryStats, error) {

	statsByID := make(map[string]roundSummaryStats, len(rounds))
	if len(rounds) == 0 {
		return statsByID, nil
	}

	roundIDs := make([][]byte, 0, len(rounds))
	for _, r := range rounds {
		roundIDs = append(roundIDs, r.RoundID)
	}

	switch q.Backend() {
	case sqlc.BackendTypeSqlite:
		rows, err := q.GetRoundSummaryStatsSqlite(ctx, roundIDs)
		if err != nil {
			return nil, fmt.Errorf(
				"get sqlite round stats: %w", err,
			)
		}

		for _, row := range rows {
			statsByID[string(row.RoundID)] = roundSummaryStats{
				numParticipants: uint32(row.NumParticipants),
				totalValueSat:   row.TotalValueSat,
			}
		}

	case sqlc.BackendTypePostgres:
		rows, err := q.GetRoundSummaryStatsPostgres(ctx, roundIDs)
		if err != nil {
			return nil, fmt.Errorf(
				"get postgres round stats: %w", err,
			)
		}

		for _, row := range rows {
			statsByID[string(row.RoundID)] = roundSummaryStats{
				numParticipants: uint32(row.NumParticipants),
				totalValueSat:   row.TotalValueSat,
			}
		}

	default:
		return nil, fmt.Errorf("unknown backend: %v", q.Backend())
	}

	return statsByID, nil
}

// roundToSummary converts a persisted round row and aggregate stats into the
// admin API summary.
func (a *AdminRPCServer) roundToSummary(r sqlc.Round,
	stats roundSummaryStats) (*adminrpc.RoundSummary, error) {

	roundID, err := uuid.FromBytes(r.RoundID)
	if err != nil {
		return nil, err
	}

	return &adminrpc.RoundSummary{
		Id:              roundID.String(),
		Status:          mapDBRoundStatus(r.Status),
		TxId:            r.CommitmentTxid,
		NumParticipants: stats.numParticipants,
		CreatedAtUnixS:  r.CreatedAt,
		TotalValueSat:   stats.totalValueSat,
	}, nil
}

// ListVTXOs returns VTXOs with optional status filtering. Pagination
// is performed server-side in the database via LIMIT/OFFSET,
// matching the ListRounds pattern.
func (a *AdminRPCServer) ListVTXOs(ctx context.Context,
	req *adminrpc.ListVTXOsRequest) (
	*adminrpc.ListVTXOsResponse, error) {

	if a.server.db == nil {
		return nil, fmt.Errorf("database not initialized")
	}

	q := a.server.db.Queries

	limit := int32(req.Limit)
	if limit == 0 {
		limit = 100
	}
	offset := int32(req.Cursor)

	// Convert all requested status filters to DB strings. When
	// multiple statuses are provided, we query each and merge.
	var statusFilters []string
	for _, sf := range req.StatusFilter {
		s, err := mapVTXOStatusToDBStr(sf)
		if err != nil {
			return nil, err
		}

		statusFilters = append(statusFilters, s)
	}

	var (
		dbVTXOs []sqlc.Vtxo
		err     error
	)

	switch {
	case len(statusFilters) == 1:
		// Single status filter: use the paginated query
		// directly.
		dbVTXOs, err = q.ListVTXOsByStatusPaged(
			ctx, sqlc.ListVTXOsByStatusPagedParams{
				Status: statusFilters[0],
				Limit:  limit,
				Offset: offset,
			},
		)
		if err != nil {
			return nil, fmt.Errorf(
				"list vtxos: %w", err,
			)
		}

	case len(statusFilters) > 1:
		// Multiple status filters: query each status and
		// merge. Pagination is approximate across statuses.
		for _, sf := range statusFilters {
			rows, qErr := q.ListVTXOsByStatusPaged(
				ctx,
				sqlc.ListVTXOsByStatusPagedParams{
					Status: sf,
					Limit:  limit,
					Offset: offset,
				},
			)
			if qErr != nil {
				return nil, fmt.Errorf(
					"list vtxos (%s): %w",
					sf, qErr,
				)
			}

			dbVTXOs = append(dbVTXOs, rows...)
		}

	default:
		// No filter: list all VTXOs.
		dbVTXOs, err = q.ListAllVTXOsPaged(
			ctx, sqlc.ListAllVTXOsPagedParams{
				Limit:  limit,
				Offset: offset,
			},
		)
		if err != nil {
			return nil, fmt.Errorf(
				"list all vtxos: %w", err,
			)
		}
	}

	summaries := make(
		[]*adminrpc.VTXOSummary, 0, len(dbVTXOs),
	)
	for _, v := range dbVTXOs {
		summaries = append(
			summaries, vtxoToSummary(v),
		)
	}

	var nextCursor uint64
	if int32(len(dbVTXOs)) >= limit {
		nextCursor = uint64(offset + limit)
	}

	return &adminrpc.ListVTXOsResponse{
		Vtxos:      summaries,
		NextCursor: nextCursor,
	}, nil
}

// vtxoToSummary converts a database VTXO row to a proto summary.
func vtxoToSummary(v sqlc.Vtxo) *adminrpc.VTXOSummary {
	// Use chainhash.Hash to reverse the byte order for
	// display, matching Bitcoin's standard txid format shown
	// in block explorers.
	var txHash chainhash.Hash
	copy(txHash[:], v.OutpointHash)
	outpoint := fmt.Sprintf(
		"%s:%d", txHash.String(), v.OutpointIndex,
	)

	roundID := ""
	if len(v.RoundID) > 0 {
		if rid, err := uuid.FromBytes(
			v.RoundID,
		); err == nil {
			roundID = rid.String()
		}
	}

	return &adminrpc.VTXOSummary{
		Outpoint:    outpoint,
		ValueSat:    v.Amount,
		Status:      mapDBVTXOStatus(v.Status),
		RoundId:     roundID,
		PkScriptHex: hex.EncodeToString(v.PkScript),
	}
}

// GetVTXOStats returns aggregate VTXO statistics using a single SQL
// GROUP BY query instead of loading all rows into memory.
func (a *AdminRPCServer) GetVTXOStats(ctx context.Context,
	_ *adminrpc.GetVTXOStatsRequest) (
	*adminrpc.GetVTXOStatsResponse, error) {

	if a.server.db == nil {
		return nil, fmt.Errorf("database not initialized")
	}

	q := a.server.db.Queries

	rows, err := q.GetVTXOStatsByStatus(ctx)
	if err != nil {
		return nil, fmt.Errorf(
			"get vtxo stats: %w", err,
		)
	}

	var (
		total    uint32
		pending  uint32
		live     uint32
		forfeit  uint32
		valueSat int64
	)

	for _, row := range rows {
		count := uint32(row.Count)
		total += count

		// The TotalValue column uses COALESCE so it is
		// never nil, but sqlc maps it to interface{} due
		// to the aggregate function. Assert to int64.
		val, _ := row.TotalValue.(int64)

		switch row.Status {
		case "pending":
			pending = count

		case "live":
			live = count
			valueSat = val

		case "forfeited":
			forfeit = count
		}
	}

	return &adminrpc.GetVTXOStatsResponse{
		Total:               total,
		Pending:             pending,
		Live:                live,
		Forfeited:           forfeit,
		TotalValueLockedSat: valueSat,
	}, nil
}

// ListClients returns the set of currently registered mailbox clients.
func (a *AdminRPCServer) ListClients(ctx context.Context,
	_ *adminrpc.ListClientsRequest) (
	*adminrpc.ListClientsResponse, error) {

	if a.server.clientBridge == nil {
		return &adminrpc.ListClientsResponse{}, nil
	}

	snapshots := a.server.clientBridge.ListClients()
	clients := make(
		[]*adminrpc.ClientInfo, 0, len(snapshots),
	)

	for _, snap := range snapshots {
		clients = append(clients, &adminrpc.ClientInfo{
			ClientId: string(snap.ID),
			Status:   snap.Status.String(),
		})
	}

	return &adminrpc.ListClientsResponse{
		Clients: clients,
	}, nil
}

// mapDBRoundStatus converts a DB round status string to the proto enum.
func mapDBRoundStatus(status string) adminrpc.RoundStatus {
	switch status {
	case "pending":
		// Persisted rounds only exist after the
		// commitment transaction is finalized and
		// broadcast, so the DB's pending state
		// corresponds to the admin API's
		// broadcast lifecycle phase.
		return adminrpc.RoundStatus_ROUND_STATUS_BROADCAST

	case "sealed":
		return adminrpc.RoundStatus_ROUND_STATUS_SEALED

	case "signing":
		return adminrpc.RoundStatus_ROUND_STATUS_SIGNING

	case "broadcast":
		return adminrpc.RoundStatus_ROUND_STATUS_BROADCAST

	case "confirmed":
		return adminrpc.RoundStatus_ROUND_STATUS_CONFIRMED

	case "failed":
		return adminrpc.RoundStatus_ROUND_STATUS_FAILED

	default:
		return adminrpc.RoundStatus_ROUND_STATUS_UNSPECIFIED
	}
}

// mapRoundStatusToDBStr converts a proto round status enum to the DB
// status string for filtered queries. Returns an error for
// unrecognized enum values instead of silently defaulting.
func mapRoundStatusToDBStr(
	status adminrpc.RoundStatus) (string, error) {

	switch status {
	case adminrpc.RoundStatus_ROUND_STATUS_OPEN:
		return "", fmt.Errorf(
			"round status %s is not persisted in the database",
			status.String(),
		)

	case adminrpc.RoundStatus_ROUND_STATUS_SEALED:
		return "sealed", nil

	case adminrpc.RoundStatus_ROUND_STATUS_SIGNING:
		return "signing", nil

	case adminrpc.RoundStatus_ROUND_STATUS_BROADCAST:
		return "pending", nil

	case adminrpc.RoundStatus_ROUND_STATUS_CONFIRMED:
		return "confirmed", nil

	case adminrpc.RoundStatus_ROUND_STATUS_FAILED:
		return "failed", nil

	default:
		return "", fmt.Errorf(
			"unknown round status: %v", status,
		)
	}
}

// mapDBVTXOStatus converts a DB VTXO status string to the proto enum.
func mapDBVTXOStatus(status string) adminrpc.VTXOStatus {
	switch status {
	case "pending":
		return adminrpc.VTXOStatus_VTXO_STATUS_PENDING

	case "live":
		return adminrpc.VTXOStatus_VTXO_STATUS_LIVE

	case "forfeited":
		return adminrpc.VTXOStatus_VTXO_STATUS_FORFEITED

	default:
		return adminrpc.VTXOStatus_VTXO_STATUS_UNSPECIFIED
	}
}

// mapVTXOStatusToDBStr converts a proto VTXO status enum to the DB
// status string. Returns an error for unrecognized enum values
// instead of silently defaulting.
func mapVTXOStatusToDBStr(
	status adminrpc.VTXOStatus) (string, error) {

	switch status {
	case adminrpc.VTXOStatus_VTXO_STATUS_PENDING:
		return "pending", nil

	case adminrpc.VTXOStatus_VTXO_STATUS_LIVE:
		return "live", nil

	case adminrpc.VTXOStatus_VTXO_STATUS_FORFEITED:
		return "forfeited", nil

	default:
		return "", fmt.Errorf(
			"unknown VTXO status: %v", status,
		)
	}
}

// GetFeeSchedule returns the current fee schedule parameters.
func (a *AdminRPCServer) GetFeeSchedule(_ context.Context,
	_ *adminrpc.GetFeeScheduleRequest) (
	*adminrpc.GetFeeScheduleResponse, error) {

	calc := a.server.feeCalculator
	if calc == nil {
		return nil, fmt.Errorf("fee calculator not configured")
	}

	sched := calc.Schedule()

	params := &adminrpc.FeeScheduleParams{
		AnnualRate:    sched.AnnualRate,
		BaseMarginSat: sched.BaseMarginSat,
		UtilizationThresholdBps: sched.
			UtilizationThresholdBPS,
		UtilizationSpreadDelta0Bps: sched.
			UtilizationSpreadDelta0BPS,
		UtilizationSpreadDelta1Bps: sched.
			UtilizationSpreadDelta1BPS,
		MinViablePolicy:       sched.MinViableVTXOPolicy.String(),
		MinViablePct:          sched.MinViableVTXOPct,
		MinRefreshDeltaBlocks: sched.MinRefreshDeltaBlocks,
	}

	return &adminrpc.GetFeeScheduleResponse{
		Schedule: params,
	}, nil
}

// UpdateFeeSchedule hot-reloads the fee schedule. The new schedule
// takes effect on the next round.
func (a *AdminRPCServer) UpdateFeeSchedule(ctx context.Context,
	req *adminrpc.UpdateFeeScheduleRequest) (
	*adminrpc.UpdateFeeScheduleResponse, error) {

	calc := a.server.feeCalculator
	if calc == nil {
		return nil, fmt.Errorf("fee calculator not configured")
	}

	if req.Schedule == nil {
		return nil, fmt.Errorf("schedule is required")
	}

	p := req.Schedule

	policy, err := fees.ParseDustPolicy(p.MinViablePolicy)
	if err != nil {
		return nil, fmt.Errorf("invalid dust policy: %w", err)
	}

	newSched := &fees.Schedule{
		AnnualRate:                 p.AnnualRate,
		BaseMarginSat:              p.BaseMarginSat,
		UtilizationThresholdBPS:    p.UtilizationThresholdBps,
		UtilizationSpreadDelta0BPS: p.UtilizationSpreadDelta0Bps,
		UtilizationSpreadDelta1BPS: p.UtilizationSpreadDelta1Bps,
		MinViableVTXOPolicy:        policy,
		MinViableVTXOPct:           p.MinViablePct,
		MinRefreshDeltaBlocks:      p.MinRefreshDeltaBlocks,
	}

	if err := calc.UpdateSchedule(newSched); err != nil {
		return nil, fmt.Errorf("update schedule: %w", err)
	}

	// The in-memory calculator is now running the new schedule and
	// is the source of truth for the remainder of this process.
	// Log this before attempting the persist so operators can
	// correlate the live-schedule flip with any subsequent persist
	// failure.
	a.log.InfoS(ctx, "Fee schedule updated in-memory",
		"annual_rate", newSched.AnnualRate,
		"base_margin_sat", newSched.BaseMarginSat,
	)

	// The schedule store is wired unconditionally by
	// setupFeesSubsystem, so a nil here indicates boot was skipped
	// or partially failed. Surface it as a hard error rather than
	// silently skipping the persist — a silent skip would let the
	// in-memory update live out the process lifetime with no DB
	// record, so the very next restart would revert to the config
	// schedule.
	if a.server.scheduleStore == nil {
		return nil, fmt.Errorf("schedule store not initialized")
	}

	// Persist the hot-reloaded schedule so it survives daemon
	// restart. The write runs AFTER the in-memory UpdateSchedule
	// succeeds, so a DB failure cannot leave the calculator and
	// the history table disagreeing. A persist failure is surfaced
	// to the caller as an error, but the in-memory schedule is
	// already live for this process; the operator is expected to
	// retry UpdateFeeSchedule to re-attempt the persist.
	if err := a.server.scheduleStore.InsertFeeSchedule(
		ctx, newSched,
	); err != nil {
		return nil, fmt.Errorf(
			"persist fee schedule history: %w", err,
		)
	}

	a.log.InfoS(ctx, "Fee schedule persisted",
		"annual_rate", newSched.AnnualRate,
		"base_margin_sat", newSched.BaseMarginSat,
	)

	return &adminrpc.UpdateFeeScheduleResponse{}, nil
}

// GetTreasuryStatus returns the operator's current capital
// position.
func (a *AdminRPCServer) GetTreasuryStatus(_ context.Context,
	_ *adminrpc.GetTreasuryStatusRequest) (
	*adminrpc.GetTreasuryStatusResponse, error) {

	treasury := a.server.treasury
	if treasury == nil {
		return nil, fmt.Errorf(
			"treasury tracker not configured",
		)
	}

	snap := treasury.Snapshot()

	return &adminrpc.GetTreasuryStatusResponse{
		DeployedCapitalSat: snap.DeployedCapitalSat,
		WalletBalanceSat:   snap.WalletBalanceSat,
		KMaxSat:            snap.KMaxSat,
		Utilization:        snap.Utilization,
		LiveVtxoCount:      int32(snap.LiveVTXOCount),
	}, nil
}

// ListFeeEvents returns paginated ledger entries from the
// double-entry accounting system.
func (a *AdminRPCServer) ListFeeEvents(ctx context.Context,
	req *adminrpc.ListFeeEventsRequest) (
	*adminrpc.ListFeeEventsResponse, error) {

	if a.server.db == nil {
		return nil, fmt.Errorf("database not available")
	}

	limit := int32(req.Limit)
	if limit == 0 {
		limit = 50
	}
	offset := int32(req.Offset)

	var (
		entries []sqlc.LedgerEntry
		total   int64
		err     error
	)

	if req.EventTypeFilter != "" {
		entries, err = a.server.db.ListLedgerEntriesByEventType(
			ctx, sqlc.ListLedgerEntriesByEventTypeParams{
				EventType: req.EventTypeFilter,
				Limit:     limit,
				Offset:    offset,
			},
		)
		if err != nil {
			return nil, fmt.Errorf(
				"list ledger entries: %w", err,
			)
		}

		// Count with the same filter predicate so the paginated
		// total matches the filtered result set.
		total, err = a.server.db.CountLedgerEntriesByEventType(
			ctx, req.EventTypeFilter,
		)
		if err != nil {
			return nil, fmt.Errorf(
				"count ledger entries: %w", err,
			)
		}
	} else {
		entries, err = a.server.db.ListLedgerEntries(
			ctx, sqlc.ListLedgerEntriesParams{
				Limit:  limit,
				Offset: offset,
			},
		)
		if err != nil {
			return nil, fmt.Errorf(
				"list ledger entries: %w", err,
			)
		}

		total, err = a.server.db.CountLedgerEntries(ctx)
		if err != nil {
			return nil, fmt.Errorf(
				"count ledger entries: %w", err,
			)
		}
	}

	events := make([]*adminrpc.FeeEvent, len(entries))
	for i, e := range entries {
		events[i] = &adminrpc.FeeEvent{
			EntryId:        e.EntryID,
			DebitAccount:   e.DebitAccount,
			CreditAccount:  e.CreditAccount,
			AmountSat:      e.AmountSat,
			RoundId:        hex.EncodeToString(e.RoundID),
			EventType:      e.EventType,
			Description:    e.Description,
			CreatedAtUnixS: e.CreatedAt,
		}
	}

	return &adminrpc.ListFeeEventsResponse{
		Events: events,
		Total:  uint32(total),
	}, nil
}
