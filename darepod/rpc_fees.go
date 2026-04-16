package darepod

import (
	"context"
	"fmt"
	"math"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/db/sqlc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// maxFeeHistoryLimit caps the number of ledger entries returned in a
// single GetFeeHistory page. Callers that omit limit (zero) get the
// default; callers that exceed the cap are clamped down rather than
// rejected so a thin CLI can scroll history without knowing the cap.
const (
	maxFeeHistoryLimit     = 1000
	defaultFeeHistoryLimit = 50
)

// EstimateFee proxies the operator's ArkService.EstimateFee RPC over
// the daemon's direct gRPC connection. The daemon does not cache fee
// estimates: each call hits the server so the returned numbers reflect
// the operator's current treasury state.
func (r *RPCServer) EstimateFee(ctx context.Context,
	req *daemonrpc.EstimateFeeRequest) (
	*daemonrpc.EstimateFeeResponse, error) {

	if req == nil {
		return nil, status.Errorf(codes.InvalidArgument,
			"request is required")
	}

	if req.AmountSat <= 0 {
		return nil, status.Errorf(codes.InvalidArgument,
			"amount_sat must be positive")
	}

	// Reject amounts above the Bitcoin money supply so a
	// malformed caller does not trigger upstream overflow or
	// nonsense fee numbers. btcutil.MaxSatoshi is the hard
	// ceiling; anything above it is not a valid VTXO amount.
	if req.AmountSat > int64(btcutil.MaxSatoshi) {
		return nil, status.Errorf(codes.InvalidArgument,
			"amount_sat must be <= %d",
			int64(btcutil.MaxSatoshi))
	}

	if r.server.serverConn == nil {
		return nil, status.Errorf(codes.Unavailable,
			"operator gRPC connection not initialized")
	}

	client := arkrpc.NewArkServiceClient(r.server.serverConn)

	resp, err := client.EstimateFee(ctx, &arkrpc.EstimateFeeRequest{
		AmountSat:       req.AmountSat,
		IsBoarding:      req.IsBoarding,
		RemainingBlocks: req.RemainingBlocks,
	})
	if err != nil {
		return nil, fmt.Errorf("EstimateFee RPC: %w", err)
	}

	return &daemonrpc.EstimateFeeResponse{
		LiquidityFeeSat:     resp.LiquidityFeeSat,
		OnchainShareSat:     resp.OnchainShareSat,
		MarginSat:           resp.MarginSat,
		TotalFeeSat:         resp.TotalFeeSat,
		EffectiveAnnualRate: resp.EffectiveAnnualRate,
		MinViableAmountSat:  resp.MinViableAmountSat,
		BelowDustWarning:    resp.BelowDustWarning,
	}, nil
}

// GetFeeHistory returns paginated double-entry ledger rows from the
// client-side accounting DB plus a cumulative total of fees paid to
// the operator. Each row is one leg of a ledger entry; callers that
// need a netted view of round/OOR activity should aggregate by
// round_id or session_id on the receiving side.
//
// The ledger actor owns the write path: this RPC only reads. Callers
// see entries that have been durably committed, so the cumulative
// total is monotonic across restarts.
func (r *RPCServer) GetFeeHistory(ctx context.Context,
	req *daemonrpc.GetFeeHistoryRequest) (
	*daemonrpc.GetFeeHistoryResponse, error) {

	if req == nil {
		return nil, status.Errorf(codes.InvalidArgument,
			"request is required")
	}

	if r.server.ledgerStore == nil {
		return nil, status.Errorf(codes.Unavailable,
			"ledger store not initialized")
	}

	// Apply default + cap so callers that pass 0 get a sensible
	// page and callers that pass a huge limit get truncated rather
	// than rejected.
	limit := req.Limit
	switch {
	case limit == 0:
		limit = defaultFeeHistoryLimit
	case limit > maxFeeHistoryLimit:
		limit = maxFeeHistoryLimit
	}

	// Reject offsets that would overflow the int32 column the
	// underlying sqlc query uses. Without this guard, a uint32
	// offset >= 2^31 silently wraps to a negative int32 which
	// SQLite/Postgres either rejects or interprets as zero.
	if req.Offset > math.MaxInt32 {
		return nil, status.Errorf(codes.InvalidArgument,
			"offset must be <= %d", math.MaxInt32)
	}

	// Read the page and the cumulative fees-paid total inside the
	// same read transaction so the response is internally
	// consistent: a concurrent insert can't show up in the total
	// without also being on the page (or vice versa).
	rows, totalFees, err := r.server.ledgerStore.
		ListLedgerEntriesWithFeesTotal(
			ctx, int32(limit), int32(req.Offset),
		)
	if err != nil {
		return nil, fmt.Errorf(
			"list ledger entries with total: %w", err,
		)
	}

	entries := make([]*daemonrpc.FeeHistoryEntry, 0, len(rows))
	for i := range rows {
		entries = append(entries, ledgerEntryToProto(&rows[i]))
	}

	return &daemonrpc.GetFeeHistoryResponse{
		Entries:          entries,
		TotalFeesPaidSat: totalFees,
	}, nil
}

// ledgerEntryToProto converts a sqlc-generated LedgerEntry row to the
// proto type returned by GetFeeHistory. Nullable round_id / session_id
// columns surface as zero-length byte slices on the wire so clients
// can rely on length checks to distinguish in-round, OOR, and
// non-keyed events.
func ledgerEntryToProto(
	row *sqlc.LedgerEntry) *daemonrpc.FeeHistoryEntry {

	return &daemonrpc.FeeHistoryEntry{
		EntryId:        row.EntryID,
		EventType:      row.EventType,
		AmountSat:      row.AmountSat,
		Description:    row.Description,
		CreatedAtUnixS: row.CreatedAt,
		DebitAccount:   row.DebitAccount,
		CreditAccount:  row.CreditAccount,
		RoundId:        row.RoundID,
		SessionId:      row.SessionID,
	}
}
