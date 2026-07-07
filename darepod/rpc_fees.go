package darepod

import (
	"context"
	"fmt"
	"math"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/db/sqlc"
	"github.com/lightninglabs/darepo-client/vtxo"
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

	maxTransactionHistoryLimit     = 1000
	defaultTransactionHistoryLimit = 50
)

// quoteOperatorFee asks the operator's EstimateFee RPC for the
// dynamic per-operation fee that applies to a boarding or refresh
// of `amountSat` at the given remainingBlocks. The returned amount
// is what the client must deduct from the VTXO output so the
// server's validateOperatorFee check accepts the submission.
//
// Back-compat: if the operator is running a zero schedule,
// TotalFeeSat == 0 and this call reduces to the pre-fee flow. If
// the operator is unreachable (no outbound client, RPC failure), the
// caller is expected to fall back to the legacy flat
// terms.MinOperatorFee so boarding remains possible in a degraded
// mode rather than failing outright.
//
// Called from Board and SendVTXO so the client's implicit fee
// matches what the server's validateOperatorFee expects under a
// non-zero schedule. Without this path the client would keep
// paying the legacy flat MinOperatorFee and silently overpay
// under low schedules or trigger ErrOperatorFeeTooLow under
// high schedules.
func (s *Server) quoteOperatorFee(ctx context.Context, amountSat int64,
	isBoarding bool, remainingBlocks uint32) (btcutil.Amount, error) {

	client := s.operatorArkClient()
	if client == nil {
		return 0, status.Errorf(codes.Unavailable, "operator "+
			"connection not initialized")
	}

	resp, err := client.EstimateFee(
		ctx, &arkrpc.EstimateFeeRequest{
			AmountSat:       amountSat,
			IsBoarding:      isBoarding,
			RemainingBlocks: remainingBlocks,
		},
	)
	if err != nil {
		return 0, err
	}

	// Defensive nil-guard: well-behaved gRPC servers always return
	// a non-nil response when err == nil, but a stub or a future
	// server change that violates the convention would panic on
	// the field access below. Surface the missing payload as a
	// clean codes.Internal so the caller can fall back instead of
	// crashing the daemon.
	if resp == nil {
		return 0, status.Errorf(codes.Internal, "operator returned "+
			"empty fee response")
	}

	return btcutil.Amount(resp.TotalFeeSat), nil
}

// autoRefreshFeeQuoter builds the vtxo.RefreshFeeQuoter the VTXO
// manager plumbs into every spawned VTXO actor. Under the seal-time
// fee handshake (#270) the returned closure is a pure advisory hook:
// the value it produces is carried on RefreshVTXORequest.OperatorFee
// for observability but is NOT subtracted from the new VTXO output
// or otherwise persisted into the intent — the server computes the
// authoritative fee at seal time and the client enforces its cap via
// QuoteReceivedState.MaxOperatorFee.
//
// The closure still prefers the live quote from the operator's
// EstimateFee RPC when reachable, and falls back to the legacy
// MinOperatorFee floor when the quote or terms fetch fails, so logs
// and metrics see a sensible number even in degraded mode. A zero
// return (terms + quote both unreachable) is safe: the auto-refresh
// still emits and the seal-time handshake reconciles the actual fee.
func (s *Server) autoRefreshFeeQuoter() vtxo.RefreshFeeQuoter {
	return func(ctx context.Context, amount btcutil.Amount,
		remainingBlocks uint32) btcutil.Amount {

		// Resolve the legacy floor first so every error branch
		// has a consistent fallback. Terms fetch failures log
		// locally so the operator can diagnose a misconfigured
		// outbound client without the VTXO actor spamming duplicate
		// errors from its side.
		var minFee btcutil.Amount
		terms, termsErr := s.fetchOperatorTerms(ctx)
		if termsErr != nil {
			s.log.WarnS(ctx,
				"Auto-refresh fee quoter: "+
					"fetchOperatorTerms failed; "+
					"using zero MinOperatorFee "+
					"fallback",
				termsErr)
		} else {
			minFee = terms.MinOperatorFee
		}

		quoted, qErr := s.quoteOperatorFee(
			ctx, int64(amount),
			false /* isBoarding */, remainingBlocks,
		)
		switch {
		case qErr != nil:
			s.log.WarnS(ctx,
				"Auto-refresh fee quoter: EstimateFee "+
					"unavailable; falling back to "+
					"MinOperatorFee",
				qErr)

			return minFee

		case quoted > minFee:
			// Server expects the dynamic quote; pay it so
			// validateOperatorFee sees the full per-input
			// ComputeForfeitFee amount.
			return quoted

		default:
			// Dynamic quote is at or below the legacy floor.
			// Pay the floor so the client's round-FSM
			// pre-flight check at round/transitions.go
			// still passes — it rejects any submission
			// strictly below MinOperatorFee before the
			// server sees it.
			return minFee
		}
	}
}

// EstimateFee proxies the operator's ArkService.EstimateFee RPC over
// the daemon's configured direct outbound transport. The daemon does not cache
// fee estimates: each call hits the server so the returned numbers reflect the
// operator's current treasury state.
func (r *RPCServer) EstimateFee(ctx context.Context,
	req *daemonrpc.EstimateFeeRequest) (*daemonrpc.EstimateFeeResponse,
	error) {

	if req == nil {
		return nil, status.Errorf(codes.InvalidArgument, "request is "+
			"required")
	}

	if req.AmountSat <= 0 {
		return nil, status.Errorf(codes.InvalidArgument, "amount_sat "+
			"must be positive")
	}

	// Reject amounts above the Bitcoin money supply so a
	// malformed caller does not trigger upstream overflow or
	// nonsense fee numbers. btcutil.MaxSatoshi is the hard
	// ceiling; anything above it is not a valid VTXO amount.
	if req.AmountSat > int64(btcutil.MaxSatoshi) {
		return nil, status.Errorf(codes.InvalidArgument, "amount_sat "+
			"must be <= %d", int64(btcutil.MaxSatoshi))
	}

	client := r.server.operatorArkClient()
	if client == nil {
		return nil, status.Errorf(codes.Unavailable, "operator "+
			"connection not initialized")
	}

	resp, err := client.EstimateFee(ctx, &arkrpc.EstimateFeeRequest{
		AmountSat:       req.AmountSat,
		IsBoarding:      req.IsBoarding,
		RemainingBlocks: req.RemainingBlocks,
	})
	if err != nil {
		// Log the full upstream detail locally and return a
		// sanitized status to the caller. fmt.Errorf with %w
		// would both erase the gRPC code and leak operator
		// internals verbatim; passing the code through (with a
		// generic message) preserves client-side retry logic
		// without exposing server-side error text.
		r.server.log.WarnS(ctx,
			"EstimateFee upstream failed", err)

		return nil, proxyUpstreamError(err,
			"EstimateFee failed")
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
	req *daemonrpc.GetFeeHistoryRequest) (*daemonrpc.GetFeeHistoryResponse,
	error) {

	if req == nil {
		return nil, status.Errorf(codes.InvalidArgument, "request is "+
			"required")
	}

	// Reject offsets that would overflow the int32 column the
	// underlying sqlc query uses before touching the store so a
	// bad argument always returns InvalidArgument regardless of
	// infrastructure state. Without this guard, a uint32 offset
	// >= 2^31 silently wraps to a negative int32 which
	// SQLite/Postgres either rejects or interprets as zero.
	if req.Offset > math.MaxInt32 {
		return nil, status.Errorf(codes.InvalidArgument, "offset must "+
			"be <= %d", math.MaxInt32)
	}

	if r.server.ledgerStore == nil {
		return nil, status.Errorf(codes.Unavailable, "ledger store "+
			"not initialized")
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

	// Read the page and the cumulative fees-paid total inside the
	// same read transaction so the response is internally
	// consistent: a concurrent insert can't show up in the total
	// without also being on the page (or vice versa).
	rows, totalFees, err := r.server.ledgerStore.
		ListLedgerEntriesWithFeesTotal(
			ctx, int32(limit), int32(req.Offset),
		)
	if err != nil {
		r.server.log.WarnS(ctx,
			"GetFeeHistory ledger read failed", err)

		return nil, status.Error(codes.Internal,
			"ledger read failed")
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

// ListTransactions returns a unified newest-first history page from the
// client's local accounting and sweep databases. The ledger actor owns all
// writes; this RPC only reads committed rows and applies filters before
// pagination in SQL.
func (r *RPCServer) ListTransactions(ctx context.Context,
	req *daemonrpc.ListTransactionsRequest) (
	*daemonrpc.ListTransactionsResponse, error) {

	if req == nil {
		return nil, status.Errorf(codes.InvalidArgument, "request is "+
			"required")
	}
	if req.Offset > math.MaxInt32 {
		return nil, status.Errorf(codes.InvalidArgument, "offset must "+
			"be <= %d", math.MaxInt32)
	}
	if req.FromUnixS < 0 {
		return nil, status.Errorf(codes.InvalidArgument,
			"from_unix_s must be non-negative")
	}
	if req.ToUnixS < 0 {
		return nil, status.Errorf(codes.InvalidArgument, "to_unix_s "+
			"must be non-negative")
	}
	if req.FromUnixS != 0 && req.ToUnixS != 0 &&
		req.FromUnixS > req.ToUnixS {
		return nil, status.Errorf(codes.InvalidArgument,
			"from_unix_s must be <= to_unix_s")
	}
	if !transactionTypeFilterValid(req.Type) {
		return nil, status.Errorf(codes.InvalidArgument, "unknown "+
			"transaction type %q", req.Type)
	}
	if r.server.ledgerStore == nil {
		return nil, status.Errorf(codes.Unavailable, "ledger store "+
			"not initialized")
	}

	limit := req.Limit
	switch {
	case limit == 0:
		limit = defaultTransactionHistoryLimit

	case limit > maxTransactionHistoryLimit:
		limit = maxTransactionHistoryLimit
	}

	rows, err := r.server.ledgerStore.ListTransactionHistory(
		ctx, req.Type, req.FromUnixS, req.ToUnixS, int32(limit)+1,
		int32(req.Offset),
	)
	if err != nil {
		r.server.log.WarnS(ctx, "ListTransactions history read failed",
			err,
		)

		return nil, status.Error(
			codes.Internal, "transaction history read failed",
		)
	}

	hasMore := len(rows) > int(limit)
	if hasMore {
		rows = rows[:limit]
	}

	entries := make([]*daemonrpc.TransactionHistoryEntry, 0, len(rows))
	for i := range rows {
		entry, err := transactionHistoryRowToProto(&rows[i])
		if err != nil {
			r.server.log.WarnS(
				ctx,
				"ListTransactions row conversion failed",
				err,
			)

			return nil, status.Error(
				codes.Internal,
				"transaction history conversion failed",
			)
		}

		entries = append(entries, entry)
	}

	resp := &daemonrpc.ListTransactionsResponse{
		Transactions: entries,
		HasMore:      hasMore,
	}
	if hasMore {
		resp.NextOffset = req.Offset + limit
	}

	return resp, nil
}

// proxyUpstreamError returns a gRPC status error for a proxied
// upstream call. When the upstream error carries a gRPC status,
// its code is preserved so client-side retry/backoff logic still
// works; the message is replaced with a generic string so
// operator-internal details (DB error text, internal stack
// frames) are not leaked over the wire. When the upstream error
// has no gRPC status (e.g. a network failure before the RPC
// ran), Unavailable is used so the caller can retry against a
// different connection.
func proxyUpstreamError(err error, msg string) error {
	if err == nil {
		return nil
	}

	if st, ok := status.FromError(err); ok && st.Code() != codes.OK {
		return status.Error(st.Code(), msg)
	}

	return status.Error(codes.Unavailable, msg)
}

// ledgerEntryToProto converts a sqlc-generated LedgerEntry row to the
// proto type returned by GetFeeHistory. Nullable round_id / session_id
// columns surface as zero-length byte slices on the wire so clients
// can rely on length checks to distinguish in-round, OOR, and
// non-keyed events.
func ledgerEntryToProto(row *sqlc.LedgerEntry) *daemonrpc.FeeHistoryEntry {
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

// transactionTypeFilterValid returns true for the public high-level
// transaction history categories accepted by ListTransactions.
func transactionTypeFilterValid(filter string) bool {
	switch filter {
	case "", "boarding", "round", "oor", "sweep":
		return true

	default:
		return false
	}
}

// transactionHistoryRowToProto converts the sqlc union row into its
// public RPC representation.
func transactionHistoryRowToProto(row *sqlc.ListTransactionHistoryRow) (
	*daemonrpc.TransactionHistoryEntry, error) {

	txid, err := transactionHistoryTxID(row.Txid)
	if err != nil {
		return nil, err
	}

	return &daemonrpc.TransactionHistoryEntry{
		Source:             row.Source,
		Type:               row.TransactionType,
		Subtype:            row.Subtype,
		AmountSat:          row.AmountSat,
		FeeSat:             row.FeeSat,
		CreatedAtUnixS:     row.CreatedAt,
		ConfirmationStatus: row.Status,
		Description:        row.Description,
		EntryId:            row.EntryID,
		Txid:               txid,
		DebitAccount:       row.DebitAccount,
		CreditAccount:      row.CreditAccount,
		RoundId:            row.RoundID,
		SessionId:          row.SessionID,
		ConfirmationHeight: row.ConfirmationHeight,
		OutputIndex:        row.OutputIndex,
		BoardingAddress:    row.BoardingAddress,
	}, nil
}

// transactionHistoryTxID normalizes the nullable sqlc txid column into
// the human-readable chainhash string used elsewhere in the RPC API.
func transactionHistoryTxID(txidBytes []byte) (string, error) {
	if len(txidBytes) == 0 {
		return "", nil
	}

	txid, err := chainhash.NewHash(txidBytes)
	if err != nil {
		return "", fmt.Errorf("decode txid: %w", err)
	}

	return txid.String(), nil
}
