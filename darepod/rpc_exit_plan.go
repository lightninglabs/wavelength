package darepod

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/db"
	"github.com/lightninglabs/darepo-client/unroll"
	"github.com/lightninglabs/darepo-client/wallet"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	defaultExitPlanConfTarget uint32 = 6
)

// ExitPlanRequest asks the daemon to preview unilateral-exit funding
// readiness for a slice of VTXO outpoints.
type ExitPlanRequest struct {
	Outpoints  []string
	ConfTarget uint32
}

// ExitPlanEntry describes the unroll CPFP fee-input readiness for a single
// previewed VTXO outpoint.
type ExitPlanEntry struct {
	Outpoint                   string
	FundingAddress             string
	RequiredConfirmations      uint32
	RequiredFeeUTXOCount       uint32
	UsableFeeUTXOCount         uint32
	RecommendedUTXOAmountSat   int64
	RecommendedTotalFundingSat int64
	FundingShortfallSat        int64
	CanStart                   bool
	ExitJobFound               bool
	ExitStatus                 daemonrpc.UnrollJobStatus
	SweepTxid                  *chainhash.Hash
	LastError                  error

	// Err is a per-outpoint failure (e.g. VTXO not found) so one bad
	// outpoint does not fail the whole batch. Nil on success.
	Err error
}

// ExitPlanResponse describes the combined unroll CPFP fee-input readiness for
// every previewed outpoint plus aggregate totals.
type ExitPlanResponse struct {
	Plans                      []ExitPlanEntry
	FeeRateSatPerVByte         int64
	CanStart                   bool
	TotalFundingShortfallSat   int64
	TotalRecommendedFundingSat int64
}

// WalletSweepInput describes one backing-wallet UTXO selected by SweepWallet.
type WalletSweepInput struct {
	Outpoint  string
	AmountSat int64
}

// SweepWalletRequest asks the daemon to preview or publish a backing-wallet
// sweep.
type SweepWalletRequest struct {
	DestinationAddress string
	Broadcast          bool
	FeeRateSatPerVByte int64
	ConfTarget         uint32
}

// SweepWalletResponse describes the selected inputs and optional broadcast tx.
type SweepWalletResponse struct {
	Inputs             []WalletSweepInput
	TotalInputSat      int64
	EstimatedFeeSat    int64
	NetAmountSat       int64
	FeeRateSatPerVByte int64
	CanBroadcast       bool
	Txid               *chainhash.Hash
	FailureReason      error
}

// GetExitPlan previews the wallet-side resources needed before admitting a
// unilateral exit for a slice of VTXO outpoints. Per-outpoint failures are
// recorded on the corresponding entry rather than failing the whole batch;
// only request-wide failures return a top-level error.
func (r *RPCServer) GetExitPlan(ctx context.Context, req *ExitPlanRequest) (
	*ExitPlanResponse, error) {

	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}
	if req == nil {
		return nil, status.Error(
			codes.InvalidArgument, "request is nil",
		)
	}
	if len(req.Outpoints) == 0 {
		return nil, status.Error(
			codes.InvalidArgument, "outpoints is required",
		)
	}
	if r.server.vtxoStore == nil {
		return nil, status.Error(
			codes.Unavailable, "VTXO store not initialized",
		)
	}

	// Trace the previewed batch shape on entry so the aggregate verdict
	// logged on exit can be correlated with the request that produced it.
	r.server.log.DebugS(ctx, "GetExitPlan requested",
		slog.Int("num_outpoints", len(req.Outpoints)),
		slog.Uint64("conf_target", uint64(req.ConfTarget)),
	)

	// The fee estimate and wallet snapshot are wallet-wide, so compute
	// them once and reuse them for every previewed outpoint.
	feeRate, err := r.estimateWalletFeeRate(ctx, req.ConfTarget)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "estimate fee: %v",
			err)
	}

	walletSnapshot, err := r.walletExitFundingSnapshot(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list wallet "+
			"unspent: %v", err)
	}

	resp := &ExitPlanResponse{
		FeeRateSatPerVByte: feeRate,
	}
	resp.Plans = make([]ExitPlanEntry, 0, len(req.Outpoints))

	// Entries are previewed in request order against a running wallet
	// allocation: each feasible exit reserves its fee inputs out of
	// `remaining` before the next entry is assessed. Without this, a batch
	// of N VTXOs that each fit the full wallet -- but not the wallet split
	// N ways -- would every one report ready, so the aggregate can_start /
	// shortfall would under-report the cross-VTXO overlap.
	remaining := walletSnapshot

	// canStart is the AND over every entry that has no per-outpoint
	// error. It is only meaningful once at least one entry succeeds.
	canStart := true
	sawEntry := false
	for _, raw := range req.Outpoints {
		entry, verdict := r.exitPlanEntry(ctx, raw, feeRate, remaining)
		resp.Plans = append(resp.Plans, entry)
		if entry.Err != nil {
			continue
		}

		sawEntry = true
		canStart = canStart && entry.CanStart
		resp.TotalFundingShortfallSat += entry.FundingShortfallSat

		recommended := entry.RecommendedTotalFundingSat
		resp.TotalRecommendedFundingSat += recommended

		// A feasible exit notionally claims its fee inputs, so shrink
		// the wallet the remaining entries get to plan against.
		if entry.CanStart {
			remaining = claimExitFunding(remaining, verdict)
		}
	}

	resp.CanStart = sawEntry && canStart

	// Summarize the aggregate readiness verdict on exit.
	r.server.log.DebugS(ctx, "GetExitPlan computed",
		slog.Bool("can_start", resp.CanStart),
		slog.Int64("total_shortfall_sat",
			resp.TotalFundingShortfallSat),
	)

	return resp, nil
}

// exitPlanEntry previews a single VTXO outpoint against the shared fee rate
// and wallet snapshot. Per-outpoint failures are recorded on entry.Err so the
// caller can continue previewing the rest of the batch.
func (r *RPCServer) exitPlanEntry(ctx context.Context, raw string,
	feeRate int64, walletSnapshot unroll.ExitFundingSnapshot) (
	ExitPlanEntry, unroll.ExitFeasibility) {

	entry := ExitPlanEntry{Outpoint: raw}

	// verdict is the feasibility breakdown the caller uses to decrement the
	// running wallet allocation between batch entries. It stays zero on the
	// error paths below (the caller only consumes it for a feasible entry).
	var verdict unroll.ExitFeasibility

	outpoint, err := parseOutpointString(raw)
	if err != nil {
		entry.Err = fmt.Errorf("invalid outpoint: %w", err)

		return entry, verdict
	}
	entry.Outpoint = outpoint.String()

	desc, err := r.server.vtxoStore.GetVTXO(ctx, outpoint)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			entry.Err = fmt.Errorf("VTXO %s not found", outpoint)

			return entry, verdict
		}

		entry.Err = fmt.Errorf("get VTXO: %w", err)

		return entry, verdict
	}

	mat := r.resolveExitLineage(ctx, outpoint, desc)
	plan := unroll.PlanExitFunding(
		desc, mat, btcutil.Amount(feeRate), walletSnapshot,
	)
	verdict = plan.Feasibility
	if verdict.RequiredWalletInputs == 0 {
		entry.Err = fmt.Errorf("VTXO %s has no unilateral-exit "+
			"ancestry", outpoint)

		return entry, verdict
	}

	entry.RequiredConfirmations = plan.RequiredConfirmations
	entry.RequiredFeeUTXOCount = uint32(verdict.RequiredWalletInputs)
	entry.UsableFeeUTXOCount = uint32(verdict.WalletUsableInputs)
	entry.RecommendedUTXOAmountSat = int64(plan.RecommendedUTXOAmountSat)
	entry.RecommendedTotalFundingSat = int64(
		plan.RecommendedTotalFundingSat,
	)
	entry.FundingShortfallSat = int64(plan.FundingShortfallSat)
	entry.CanStart = verdict.Feasible

	// Only allocate a funding address when the outpoint is NOT feasible
	// (there is a shortfall). When feasible, no address is needed so the
	// preview must not persist one.
	if !verdict.Feasible {
		fundingAddress, err := r.server.exitPlanFundingAddress(
			ctx, outpoint.String(), true,
		)
		if err != nil {
			entry.Err = fmt.Errorf("exit plan funding address: %w",
				err)

			return entry, verdict
		}

		entry.FundingAddress = fundingAddress
	}

	statusResp, err := r.GetUnrollStatus(
		ctx, &daemonrpc.GetUnrollStatusRequest{
			Outpoint: outpoint.String(),
		},
	)
	if err != nil {
		entry.Err = fmt.Errorf("exit status: %w", err)

		return entry, verdict
	}
	entry.ExitJobFound = statusResp.GetFound()
	entry.ExitStatus = statusResp.GetStatus()
	if sweepTxid := statusResp.GetSweepTxid(); sweepTxid != "" {
		txid, err := chainhash.NewHashFromStr(sweepTxid)
		if err != nil {
			entry.Err = fmt.Errorf("exit status sweep txid: %w",
				err)

			return entry, verdict
		}

		entry.SweepTxid = txid
	}
	if lastErr := statusResp.GetLastError(); lastErr != "" {
		entry.LastError = errors.New(lastErr)
	}

	return entry, verdict
}

// claimExitFunding returns the wallet snapshot that remains after a feasible
// exit reserves its fee inputs. A started exit pins RequiredWalletInputs of
// the usable confirmed UTXOs (one per ancestry path) and spends CPFPFeeTotalSat
// of the confirmed balance on CPFP fees, so a batch preview must hand each
// later entry the shrunken wallet rather than the full one. Both fields clamp
// at zero so an over-subscribed batch reports the next entry as unfunded
// rather than wrapping negative.
func claimExitFunding(snapshot unroll.ExitFundingSnapshot,
	verdict unroll.ExitFeasibility) unroll.ExitFundingSnapshot {

	snapshot.WalletUsableInputs -= verdict.RequiredWalletInputs
	if snapshot.WalletUsableInputs < 0 {
		snapshot.WalletUsableInputs = 0
	}

	snapshot.WalletConfirmedSat -= verdict.CPFPFeeTotalSat
	if snapshot.WalletConfirmedSat < 0 {
		snapshot.WalletConfirmedSat = 0
	}

	return snapshot
}

// walletExitFundingSnapshot returns the confirmed backing-wallet balance and
// the count of confirmed UTXOs that satisfy the unroll preflight floor.
func (r *RPCServer) walletExitFundingSnapshot(ctx context.Context) (
	unroll.ExitFundingSnapshot, error) {

	utxos, err := r.server.ListWalletUnspent(
		ctx, int32(unroll.RequiredFeeInputConfirmations),
		wallet.MaxConfsForListUnspent,
	)
	if err != nil {
		return unroll.ExitFundingSnapshot{}, err
	}

	var confirmed btcutil.Amount
	usable := 0
	for _, utxo := range utxos {
		if utxo == nil {
			continue
		}

		confirmed += utxo.Amount
		if utxo.Amount >= unroll.DefaultFeeInputMinAmountSat {
			usable++
		}
	}

	return unroll.ExitFundingSnapshot{
		WalletConfirmedSat: confirmed,
		WalletUsableInputs: usable,
	}, nil
}

// SweepWallet previews or broadcasts a normal backing-wallet sweep that
// excludes boarding outputs. The RPC is a thin shim: it validates the request
// surface, Asks the wallet actor (which owns input locking, fee capping,
// signing, and broadcast routing through txconfirm), and maps the actor reply
// onto the RPC response shape.
func (r *RPCServer) SweepWallet(ctx context.Context, req *SweepWalletRequest) (
	*SweepWalletResponse, error) {

	if err := r.requireWalletReady(); err != nil {
		return nil, err
	}
	if req == nil {
		return nil, status.Error(
			codes.InvalidArgument, "request is nil",
		)
	}
	if req.DestinationAddress == "" {
		return nil, status.Error(
			codes.InvalidArgument,
			"destination_address is required",
		)
	}
	if req.FeeRateSatPerVByte < 0 {
		return nil, status.Error(
			codes.InvalidArgument,
			"fee_rate_sat_per_vbyte must be non-negative",
		)
	}
	if !r.server.walletRef.IsSome() {
		return nil, status.Errorf(codes.Unavailable, "wallet actor "+
			"unavailable")
	}

	// Trace the request shape on entry; the wallet actor owns the detailed
	// preview/broadcast logging, so this shim only bookends the call.
	r.server.log.DebugS(ctx, "SweepWallet requested",
		slog.String("destination", req.DestinationAddress),
		slog.Bool("broadcast", req.Broadcast),
	)

	walletReq := &wallet.SweepWalletFundsRequest{
		DestinationAddress: req.DestinationAddress,
		Broadcast:          req.Broadcast,
		FeeRateSatPerVByte: req.FeeRateSatPerVByte,
		ConfTarget:         req.ConfTarget,
	}

	wRef := r.server.walletRef.UnsafeFromSome()
	future := wRef.Ask(ctx, walletReq)
	result := future.Await(ctx)
	if result.IsErr() {
		return nil, status.Errorf(codes.Internal, "sweep wallet: %v",
			result.Err())
	}

	raw := result.UnwrapOr(nil)
	walletResp, ok := raw.(*wallet.SweepWalletFundsResponse)
	if !ok || walletResp == nil {
		return nil, status.Errorf(codes.Internal, "unexpected sweep "+
			"response from wallet actor")
	}

	// Summarize the outcome on exit: whether the sweep was publishable and
	// whether a tx was actually broadcast.
	r.server.log.DebugS(ctx, "SweepWallet completed",
		slog.Bool("can_broadcast", walletResp.CanBroadcast),
		slog.Bool("has_txid", walletResp.HasTxid),
	)

	return walletSweepFundsResponseToRPC(walletResp), nil
}

// walletSweepFundsResponseToRPC maps the wallet actor's sweep reply onto the
// RPC response shape consumed by the swapwallet proto boundary.
func walletSweepFundsResponseToRPC(
	resp *wallet.SweepWalletFundsResponse) *SweepWalletResponse {

	rpcResp := &SweepWalletResponse{
		Inputs: make(
			[]WalletSweepInput, 0, len(resp.Inputs),
		),
		TotalInputSat:      resp.TotalInputSat,
		EstimatedFeeSat:    resp.EstimatedFeeSat,
		NetAmountSat:       resp.NetAmountSat,
		FeeRateSatPerVByte: resp.FeeRateSatPerVByte,
		CanBroadcast:       resp.CanBroadcast,
	}
	for _, in := range resp.Inputs {
		rpcResp.Inputs = append(rpcResp.Inputs, WalletSweepInput{
			Outpoint:  in.Outpoint.String(),
			AmountSat: in.AmountSat,
		})
	}
	if resp.HasTxid {
		txid := resp.Txid
		rpcResp.Txid = &txid
	}
	if resp.FailureReason != "" {
		rpcResp.FailureReason = errors.New(resp.FailureReason)
	}

	return rpcResp
}

func (r *RPCServer) estimateWalletFeeRate(ctx context.Context,
	confTarget uint32) (int64, error) {

	if confTarget == 0 {
		confTarget = defaultExitPlanConfTarget
	}

	const minFeeRateSatPerVByte = int64(2)

	if r.server.chainBackend == nil {
		return minFeeRateSatPerVByte, nil
	}

	feeRate, err := r.server.chainBackend.EstimateFee(ctx, confTarget)
	if err != nil {

		//nolint:nilerr // Fresh regtest wallets often lack fee history.
		return minFeeRateSatPerVByte, nil
	}

	rate := int64(feeRate)
	if rate < minFeeRateSatPerVByte {
		return minFeeRateSatPerVByte, nil
	}

	if maxRate := r.server.unrollMaxFeeRate(); maxRate > 0 &&
		rate > maxRate {
		return maxRate, nil
	}

	return rate, nil
}

func (s *Server) exitPlanFundingAddress(ctx context.Context, outpoint string,
	create bool) (string, error) {

	if !create {
		return "", nil
	}

	return s.exitFundingAddresses.Address(
		ctx, outpoint, s.NewWalletAddress,
	)
}

// ExitSummaryEntry is one in-progress exit's coarse contribution to the
// wallet-wide exit portfolio: its phase plus the amount and estimated fees
// derived from the persisted descriptor. It deliberately avoids querying each
// live actor, so the summary stays cheap regardless of how many exits are in
// flight.
type ExitSummaryEntry struct {
	Outpoint           string
	Status             daemonrpc.UnrollJobStatus
	VTXOAmountSat      int64
	EstTotalFeeSat     int64
	EstNetRecoveredSat int64
}

// ExitSummaryResult is the wallet-wide portfolio of in-progress exits plus
// aggregate totals. Only non-terminal exits are included: a completed or failed
// exit has no amount still being recovered.
type ExitSummaryResult struct {
	Entries                 []ExitSummaryEntry
	TotalExits              uint32
	TotalVTXOAmountSat      int64
	TotalEstFeeSat          int64
	TotalEstNetRecoveredSat int64
}

// ExitSummary returns the wallet-wide portfolio of in-progress unilateral
// exits. It lists every non-terminal exit job and, for each, projects the VTXO
// value and the estimated on-chain cost from the persisted descriptor using the
// same cost model as GetExitPlan, then sums them so a caller can see the total
// amount still being recovered across all exits at once. A per-outpoint
// descriptor lookup failure records a zero-cost entry rather than failing the
// whole summary.
func (r *RPCServer) ExitSummary(ctx context.Context) (*ExitSummaryResult,
	error) {

	result := &ExitSummaryResult{Entries: []ExitSummaryEntry{}}
	if r.server.ueStore == nil {
		return result, nil
	}

	jobs, err := r.server.ueStore.ListNonTerminalJobs(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list exit jobs: %v",
			err)
	}
	if len(jobs) == 0 {
		return result, nil
	}

	// The fee estimate is wallet-wide, so compute it once and reuse it for
	// every previewed exit.
	feeRate, err := r.estimateWalletFeeRate(ctx, 0)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "estimate fee: %v",
			err)
	}

	result.Entries = make([]ExitSummaryEntry, 0, len(jobs))
	for i := range jobs {
		entry := r.exitSummaryEntry(ctx, jobs[i], feeRate)
		result.Entries = append(result.Entries, entry)

		result.TotalVTXOAmountSat += entry.VTXOAmountSat
		result.TotalEstFeeSat += entry.EstTotalFeeSat
		result.TotalEstNetRecoveredSat += entry.EstNetRecoveredSat
	}
	result.TotalExits = uint32(len(result.Entries))

	r.server.log.DebugS(ctx, "ExitSummary computed",
		slog.Int("num_exits", len(result.Entries)),
		slog.Int64("total_amount_sat", result.TotalVTXOAmountSat),
	)

	return result, nil
}

// exitSummaryEntry projects one non-terminal exit job into a coarse portfolio
// entry. A descriptor lookup failure yields a phase-only entry with zeroed
// amounts so one missing descriptor cannot fail the whole summary.
func (r *RPCServer) exitSummaryEntry(ctx context.Context,
	job db.UnilateralExitJobRecord, feeRate int64) ExitSummaryEntry {

	entry := ExitSummaryEntry{
		Outpoint: job.TargetOutpoint.String(),
		Status:   unrollJobStatusToProto(job.Status),
	}

	if r.server.vtxoStore == nil {
		return entry
	}

	desc, err := r.server.vtxoStore.GetVTXO(ctx, job.TargetOutpoint)
	if err != nil {
		r.server.log.DebugS(ctx, "exit summary entry cost skipped",
			slog.String("outpoint", job.TargetOutpoint.String()),
			slog.String("err", err.Error()),
		)

		return entry
	}

	// A zero wallet snapshot is intentional: the cost fields depend only on
	// the fee rate, recovery-tx count, and VTXO value. A nil lineage keeps
	// the aggregate cheap: the summary approximates the OOR chain from the
	// descriptor's ChainDepth rather than resolving artifacts per exit, and
	// the single-exit `exit status`/`exit plan` views are the exact source.
	plan := unroll.PlanExitFunding(
		desc, nil, btcutil.Amount(feeRate),
		unroll.ExitFundingSnapshot{},
	)
	f := plan.Feasibility
	entry.VTXOAmountSat = int64(f.VTXOAmountSat)
	entry.EstTotalFeeSat = int64(f.TotalRecoveryCostSat)
	entry.EstNetRecoveredSat = int64(f.NetRecoveredSat)

	return entry
}
