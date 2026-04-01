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
	"github.com/lightninglabs/darepo/metrics"
	"github.com/lightninglabs/darepo/rounds"
	"google.golang.org/grpc"
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
	for _, r := range dbRounds {
		roundID, err := uuid.FromBytes(r.RoundID)
		if err != nil {
			continue
		}

		summaries = append(summaries, &adminrpc.RoundSummary{
			Id:             roundID.String(),
			Status:         mapDBRoundStatus(r.Status),
			TxId:           r.CommitmentTxid,
			CreatedAtUnixS: r.CreatedAt,
		})
	}

	return &adminrpc.ListRoundsResponse{
		Rounds: summaries,
		Total:  uint32(total),
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
