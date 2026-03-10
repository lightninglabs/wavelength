package darepo

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"

	"github.com/btcsuite/btclog/v2"
	"github.com/google/uuid"
	"github.com/lightninglabs/darepo/adminrpc"
	"github.com/lightninglabs/darepo/build"
	"github.com/lightninglabs/darepo/db/sqlc"
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

	s := &AdminRPCServer{
		cfg:        cfg,
		server:     operator,
		log:        log,
		grpcServer: grpc.NewServer(),
		listener:   listener,
		quit:       make(chan struct{}),
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
	a.grpcServer.Stop()
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
		pubkey = a.server.lnd.NodePubkey.String()
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
		statusStr := mapRoundStatusToDBStr(req.StatusFilter)

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

// ListVTXOs returns VTXOs with optional status filtering.
func (a *AdminRPCServer) ListVTXOs(ctx context.Context,
	req *adminrpc.ListVTXOsRequest) (
	*adminrpc.ListVTXOsResponse, error) {

	if a.server.db == nil {
		return nil, fmt.Errorf("database not initialized")
	}

	q := a.server.db.Queries

	// If a status filter is specified, use ListVTXOsByStatus.
	// Otherwise we'd need a ListAllVTXOs query which doesn't
	// exist yet. For now, default to "live" status.
	statusStr := "live"
	if len(req.StatusFilter) > 0 {
		statusStr = mapVTXOStatusToDBStr(req.StatusFilter[0])
	}

	dbVTXOs, err := q.ListVTXOsByStatus(ctx, statusStr)
	if err != nil {
		return nil, fmt.Errorf("list vtxos: %w", err)
	}

	limit := int(req.Limit)
	if limit == 0 {
		limit = 100
	}

	// Apply cursor-based pagination (cursor = index offset).
	start := int(req.Cursor)
	if start >= len(dbVTXOs) {
		return &adminrpc.ListVTXOsResponse{}, nil
	}
	end := start + limit
	if end > len(dbVTXOs) {
		end = len(dbVTXOs)
	}

	page := dbVTXOs[start:end]
	summaries := make([]*adminrpc.VTXOSummary, 0, len(page))

	for _, v := range page {
		outpointHash := hex.EncodeToString(v.OutpointHash)
		outpoint := fmt.Sprintf(
			"%s:%d", outpointHash, v.OutpointIndex,
		)

		roundID := ""
		if len(v.RoundID) > 0 {
			if rid, err := uuid.FromBytes(
				v.RoundID,
			); err == nil {
				roundID = rid.String()
			}
		}

		summaries = append(summaries, &adminrpc.VTXOSummary{
			Outpoint:    outpoint,
			ValueSat:    v.Amount,
			Status:      mapDBVTXOStatus(v.Status),
			RoundId:     roundID,
			PkScriptHex: hex.EncodeToString(v.PkScript),
		})
	}

	var nextCursor uint64
	if end < len(dbVTXOs) {
		nextCursor = uint64(end)
	}

	return &adminrpc.ListVTXOsResponse{
		Vtxos:      summaries,
		NextCursor: nextCursor,
	}, nil
}

// GetVTXOStats returns aggregate VTXO statistics.
func (a *AdminRPCServer) GetVTXOStats(ctx context.Context,
	_ *adminrpc.GetVTXOStatsRequest) (
	*adminrpc.GetVTXOStatsResponse, error) {

	if a.server.db == nil {
		return nil, fmt.Errorf("database not initialized")
	}

	q := a.server.db.Queries

	// Compute stats by querying each status. A future iteration
	// should use a single aggregate SQL query.
	var (
		total    uint32
		pending  uint32
		live     uint32
		forfeit  uint32
		valueSat int64
	)

	for _, status := range []string{
		"pending", "live", "forfeited",
	} {
		vtxos, err := q.ListVTXOsByStatus(ctx, status)
		if err != nil {
			return nil, fmt.Errorf(
				"list vtxos (%s): %w", status, err,
			)
		}

		count := uint32(len(vtxos))
		total += count

		switch status {
		case "pending":
			pending = count

		case "live":
			live = count
			for _, v := range vtxos {
				valueSat += v.Amount
			}

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
		return adminrpc.RoundStatus_ROUND_STATUS_OPEN

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
// status string for filtered queries.
func mapRoundStatusToDBStr(
	status adminrpc.RoundStatus) string {

	switch status {
	case adminrpc.RoundStatus_ROUND_STATUS_OPEN:
		return "pending"

	case adminrpc.RoundStatus_ROUND_STATUS_SEALED:
		return "sealed"

	case adminrpc.RoundStatus_ROUND_STATUS_SIGNING:
		return "signing"

	case adminrpc.RoundStatus_ROUND_STATUS_BROADCAST:
		return "broadcast"

	case adminrpc.RoundStatus_ROUND_STATUS_CONFIRMED:
		return "confirmed"

	case adminrpc.RoundStatus_ROUND_STATUS_FAILED:
		return "failed"

	default:
		return "pending"
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
// status string.
func mapVTXOStatusToDBStr(
	status adminrpc.VTXOStatus) string {

	switch status {
	case adminrpc.VTXOStatus_VTXO_STATUS_PENDING:
		return "pending"

	case adminrpc.VTXOStatus_VTXO_STATUS_LIVE:
		return "live"

	case adminrpc.VTXOStatus_VTXO_STATUS_FORFEITED:
		return "forfeited"

	default:
		return "live"
	}
}
