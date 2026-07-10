//go:build walletdkrpc && swapruntime

package swapwallet

import (
	"context"
	"fmt"
	"strings"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/darepod"
	"github.com/lightninglabs/darepo-client/rpc/walletdkrpc"
	"github.com/lightninglabs/darepo-client/unroll"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const forceUnrollAck = "I_KNOW_WHAT_I_AM_DOING"

// walletUnspentMaxConfs is intentionally high enough to behave as unbounded
// for a local wallet UTXO fee-input preflight.
const walletUnspentMaxConfs int32 = 9999999

// create is the implementation of WalletService.Create. It proxies the
// daemonrpc admin surface: if the caller supplied a mnemonic, we treat the
// call as a recovery flow and only call InitWallet; otherwise we run the
// GenSeed → InitWallet sequence and return the freshly generated mnemonic
// so the caller can persist it offline.
//
// The handler is admin-shape: it runs BEFORE the swap subsystem is live
// and MUST NOT depend on Runtime, router, recv, or history. The only
// daemon dependency is RPCServer (GenSeed + InitWallet).
func (s *Service) create(ctx context.Context, req *walletdkrpc.CreateRequest) (
	*walletdkrpc.CreateResponse, error) {

	if s.deps == nil || s.deps.RPCServer == nil {
		return nil, statusSwapBackendUnavailable()
	}

	if len(req.GetWalletPassword()) == 0 {
		return nil, status.Error(
			codes.InvalidArgument, "wallet_password is required",
		)
	}

	mnemonic := req.GetMnemonic()
	if len(mnemonic) == 0 {
		// Fresh wallet: generate a seed and echo the mnemonic back so
		// the caller can record it. Recording the mnemonic is the
		// caller's responsibility; once Create returns, the wallet is
		// committed to disk with that seed.
		genResp, err := s.deps.RPCServer.GenSeed(
			ctx, &daemonrpc.GenSeedRequest{
				SeedPassphrase: req.GetSeedPassphrase(),
			},
		)
		if err != nil {
			return nil, status.Errorf(status.Code(err), "gen "+
				"seed: %v", err)
		}
		mnemonic = genResp.GetMnemonic()
	}

	initResp, err := s.deps.RPCServer.InitWallet(
		ctx, &daemonrpc.InitWalletRequest{
			Mnemonic:       mnemonic,
			WalletPassword: req.GetWalletPassword(),
			SeedPassphrase: req.GetSeedPassphrase(),
			RecoverState:   req.GetRecoverState(),
			RecoveryWindow: req.GetRecoveryWindow(),
		},
	)
	if err != nil {
		return nil, status.Errorf(status.Code(err), "init wallet: %v",
			err)
	}

	return &walletdkrpc.CreateResponse{
		Mnemonic:                   mnemonic,
		IdentityPubkey:             initResp.GetIdentityPubkey(),
		RecoveryRan:                initResp.GetRecoveryRan(),
		RecoveredBoardingAddresses: initResp.GetRecoveredBoardingAddresses(),
		RecoveredBoardingUtxos:     initResp.GetRecoveredBoardingUtxos(),
		RecoveredVtxos:             initResp.GetRecoveredVtxos(),
		RecoveredOorReceiveScripts: initResp.GetRecoveredOorReceiveScripts(),
		RecoveredOorEvents:         initResp.GetRecoveredOorEvents(),
	}, nil
}

// unlock proxies daemonrpc.UnlockWallet. This is an admin-shape handler
// that runs before the swap runtime is live.
func (s *Service) unlock(ctx context.Context, req *walletdkrpc.UnlockRequest) (
	*walletdkrpc.UnlockResponse, error) {

	if s.deps == nil || s.deps.RPCServer == nil {
		return nil, statusSwapBackendUnavailable()
	}

	if len(req.GetWalletPassword()) == 0 {
		return nil, status.Error(
			codes.InvalidArgument, "wallet_password is required",
		)
	}

	resp, err := s.deps.RPCServer.UnlockWallet(
		ctx, &daemonrpc.UnlockWalletRequest{
			WalletPassword: req.GetWalletPassword(),
		},
	)
	if err != nil {
		return nil, status.Errorf(status.Code(err), "unlock wallet: %v",
			err)
	}

	return &walletdkrpc.UnlockResponse{
		IdentityPubkey: resp.GetIdentityPubkey(),
	}, nil
}

// exit queues cooperative leave by default. Forced unilateral unroll is gated
// by an exact acknowledgement string and a local backing-wallet UTXO preflight.
func (s *Service) exit(ctx context.Context, req *walletdkrpc.ExitRequest) (
	*walletdkrpc.ExitResponse, error) {

	if s.deps == nil || s.deps.RPCServer == nil {
		return nil, statusSwapBackendUnavailable()
	}

	if req.GetOutpoint() == "" {
		return nil, status.Error(
			codes.InvalidArgument, "outpoint is required",
		)
	}

	ack := strings.TrimSpace(req.GetForceUnrollAck())
	if ack != "" {
		return s.forceUnroll(ctx, req, ack)
	}

	return s.cooperativeExit(ctx, req)
}

// cooperativeExit routes wallet-facing Exit through LeaveVTXOs. If the caller
// did not supply a destination, the daemon generates a fresh backing-wallet
// address so the default path stays cooperative without asking the user for
// an on-chain address.
func (s *Service) cooperativeExit(ctx context.Context,
	req *walletdkrpc.ExitRequest) (*walletdkrpc.ExitResponse, error) {

	destination := strings.TrimSpace(req.GetOnchainAddress())
	if destination == "" {
		addr, err := s.deps.RPCServer.NewWalletAddress(ctx)
		if err != nil {
			return nil, status.Errorf(status.Code(err), "new "+
				"wallet address: %v", err)
		}
		destination = addr
	}

	resp, err := s.deps.RPCServer.LeaveVTXOs(
		ctx, &daemonrpc.LeaveVTXOsRequest{
			Selection: &daemonrpc.LeaveVTXOsRequest_Outpoints{
				Outpoints: &daemonrpc.OutpointSelection{
					Outpoints: []string{
						req.GetOutpoint(),
					},
				},
			},
			DefaultDestination: &daemonrpc.LeaveDestination{
				Target: &daemonrpc.LeaveDestination_Address{
					Address: destination,
				},
			},
		},
	)
	if err != nil {
		return nil, status.Errorf(status.Code(err), "exit: %v", err)
	}

	queued := resp.GetQueuedOutpoints()
	if !outpointQueued(req.GetOutpoint(), queued) {
		return nil, status.Errorf(codes.Internal, "exit: cooperative "+
			"leave did not echo outpoint %s", req.GetOutpoint())
	}

	return &walletdkrpc.ExitResponse{
		Mode:            walletdkrpc.ExitMode_EXIT_MODE_COOPERATIVE,
		QueuedOutpoints: queued,
		OnchainAddress:  destination,
	}, nil
}

// forceUnroll runs the legacy unilateral path only after the caller opts in
// with the exact acknowledgement string and the local backing wallet has at
// least one confirmed UTXO available for sweep fees.
func (s *Service) forceUnroll(ctx context.Context, req *walletdkrpc.ExitRequest,
	ack string) (*walletdkrpc.ExitResponse, error) {

	if req.GetOnchainAddress() != "" {
		return nil, status.Error(
			codes.InvalidArgument,
			"onchain_address cannot be set with force_unroll_ack",
		)
	}
	if ack != forceUnrollAck {
		return nil, status.Errorf(codes.InvalidArgument,
			"force_unroll_ack must be exactly %q", forceUnrollAck)
	}
	if err := s.requireLocalUnrollBalance(ctx); err != nil {
		return nil, err
	}

	resp, err := s.deps.RPCServer.Unroll(
		ctx, &daemonrpc.UnrollRequest{
			Outpoint: req.GetOutpoint(),
		},
	)
	if err != nil {
		return nil, status.Errorf(status.Code(err), "exit: %v", err)
	}

	// Project the new EXIT row into the canonical store as well as fanning
	// it out, mirroring the cooperative-leave and credit-pay paths. Without
	// this the store-backed List would miss a user-initiated unilateral
	// exit until the next startup backfill. Use a cancel-safe context so a
	// client disconnect after the accepted Unroll cannot drop the store
	// write.
	entry := unilateralExitEntryStub(req.GetOutpoint())
	s.runtime.trackPendingEntryWithoutTimeout(entry)
	s.runtime.projectAndEmit(context.WithoutCancel(ctx), entry)

	return &walletdkrpc.ExitResponse{
		Created: resp.GetCreated(),
		ActorId: resp.GetActorId(),
		Mode:    walletdkrpc.ExitMode_EXIT_MODE_UNILATERAL,
	}, nil
}

// requireLocalUnrollBalance checks that the local backing wallet has at least
// one confirmed UTXO before unilateral unroll starts. The lower-level unroll
// path still owns target-specific recovery validation; this wallet-facing gate
// rejects the obvious no-fee-input case before entering the emergency path.
func (s *Service) requireLocalUnrollBalance(ctx context.Context) error {
	utxos, err := s.deps.RPCServer.ListWalletUnspent(
		ctx, 1, walletUnspentMaxConfs,
	)
	if err != nil {
		return status.Errorf(status.Code(err), "list wallet "+
			"unspent: %v", err)
	}
	if len(utxos) > 0 {
		return nil
	}

	return status.Errorf(codes.FailedPrecondition, "no confirmed local "+
		"wallet UTXOs available for forced unroll")
}

// outpointQueued reports whether the daemon echoed target in a cooperative
// leave response. A missing echo means the daemon accepted the RPC but did not
// admit this outpoint into the round.
func outpointQueued(target string, queued []string) bool {
	for _, outpoint := range queued {
		if outpoint == target {
			return true
		}
	}

	return false
}

// getExitPlan projects the daemon's backing-wallet unroll plan onto the
// wallet-facing RPC response.
func (s *Service) getExitPlan(ctx context.Context,
	req *walletdkrpc.GetExitPlanRequest) (*walletdkrpc.GetExitPlanResponse,
	error) {

	if s.deps == nil || s.deps.RPCServer == nil {
		return nil, statusSwapBackendUnavailable()
	}

	if len(req.GetOutpoints()) == 0 {
		return nil, status.Error(
			codes.InvalidArgument, "outpoints is required",
		)
	}

	resp, err := s.deps.RPCServer.GetExitPlan(
		ctx, &darepod.ExitPlanRequest{
			Outpoints:  req.GetOutpoints(),
			ConfTarget: req.GetConfTarget(),
		},
	)
	if err != nil {
		return nil, status.Errorf(status.Code(err), "get exit plan: %v",
			err)
	}

	plans := make([]*walletdkrpc.ExitPlanEntry, 0, len(resp.Plans))
	for _, entry := range resp.Plans {
		plans = append(plans, &walletdkrpc.ExitPlanEntry{
			Outpoint:                   entry.Outpoint,
			FundingAddress:             entry.FundingAddress,
			RequiredConfirmations:      entry.RequiredConfirmations,
			RequiredFeeUtxoCount:       entry.RequiredFeeUTXOCount,
			UsableFeeUtxoCount:         entry.UsableFeeUTXOCount,
			RecommendedUtxoAmountSat:   entry.RecommendedUTXOAmountSat,
			RecommendedTotalFundingSat: entry.RecommendedTotalFundingSat,
			FundingShortfallSat:        entry.FundingShortfallSat,
			CanStart:                   entry.CanStart,
			ExitJobFound:               entry.ExitJobFound,
			ExitStatus: exitStatusFromDaemon(
				entry.ExitStatus,
			),
			SweepTxid: hashString(entry.SweepTxid),
			LastError: errorString(entry.LastError),
			Error:     errorString(entry.Err),
			InfeasibilityReason: infeasibilityReasonFromUnroll(
				entry.InfeasibilityReason,
			),
		})
	}

	return &walletdkrpc.GetExitPlanResponse{
		Plans:                      plans,
		FeeRateSatPerVbyte:         resp.FeeRateSatPerVByte,
		CanStart:                   resp.CanStart,
		TotalFundingShortfallSat:   resp.TotalFundingShortfallSat,
		TotalRecommendedFundingSat: resp.TotalRecommendedFundingSat,
	}, nil
}

// sweepWallet projects a backing-wallet sweep preview or broadcast onto the
// wallet-facing RPC response.
func (s *Service) sweepWallet(ctx context.Context,
	req *walletdkrpc.SweepWalletRequest) (*walletdkrpc.SweepWalletResponse,
	error) {

	if s.deps == nil || s.deps.RPCServer == nil {
		return nil, statusSwapBackendUnavailable()
	}

	resp, err := s.deps.RPCServer.SweepWallet(
		ctx, &darepod.SweepWalletRequest{
			DestinationAddress: req.GetDestinationAddress(),
			Broadcast:          req.GetBroadcast(),
			FeeRateSatPerVByte: req.GetFeeRateSatPerVbyte(),
			ConfTarget:         req.GetConfTarget(),
		},
	)
	if err != nil {
		return nil, status.Errorf(status.Code(err), "sweep wallet: %v",
			err)
	}

	inputs := make(
		[]*walletdkrpc.WalletSweepInput, 0, len(resp.Inputs),
	)
	for _, input := range resp.Inputs {
		inputs = append(inputs, &walletdkrpc.WalletSweepInput{
			Outpoint:  input.Outpoint,
			AmountSat: input.AmountSat,
		})
	}

	return &walletdkrpc.SweepWalletResponse{
		Inputs:             inputs,
		TotalInputSat:      resp.TotalInputSat,
		EstimatedFeeSat:    resp.EstimatedFeeSat,
		NetAmountSat:       resp.NetAmountSat,
		FeeRateSatPerVbyte: resp.FeeRateSatPerVByte,
		CanBroadcast:       resp.CanBroadcast,
		Txid:               hashString(resp.Txid),
		FailureReason:      errorString(resp.FailureReason),
	}, nil
}

// exitStatus proxies daemonrpc.GetUnrollStatus and projects the daemon's
// UnrollJobStatus onto the wallet-facing ExitJobStatus.
func (s *Service) exitStatus(ctx context.Context,
	req *walletdkrpc.ExitStatusRequest) (*walletdkrpc.ExitStatusResponse,
	error) {

	if s.deps == nil || s.deps.RPCServer == nil {
		return nil, statusSwapBackendUnavailable()
	}

	if req.GetOutpoint() == "" {
		return nil, status.Error(
			codes.InvalidArgument, "outpoint is required",
		)
	}

	resp, err := s.deps.RPCServer.GetUnrollStatus(
		ctx, &daemonrpc.GetUnrollStatusRequest{
			Outpoint: req.GetOutpoint(),
			Detailed: req.GetDetailed(),
		},
	)
	if err != nil {
		return nil, status.Errorf(status.Code(err), "exit status: %v",
			err)
	}

	return &walletdkrpc.ExitStatusResponse{
		Found:       resp.GetFound(),
		Status:      exitStatusFromDaemon(resp.GetStatus()),
		SweepTxid:   resp.GetSweepTxid(),
		LastError:   resp.GetLastError(),
		PhaseDetail: resp.GetPhaseDetail(),
		Progress: exitProgressFromDaemon(
			resp.GetProgress(),
		),
		Csv:                     exitCSVFromDaemon(resp.GetCsv()),
		Fees:                    exitFeesFromDaemon(resp.GetFees()),
		BestCaseBlocksRemaining: resp.GetBestCaseBlocksRemaining(),
		CurrentHeight:           resp.GetCurrentHeight(),
	}, nil
}

// exitProgressFromDaemon projects the daemon's UnrollProgress onto the
// wallet-facing ExitProgress. It returns nil when the daemon supplied no
// progress (a coarse query, or a terminal job with no live actor).
func exitProgressFromDaemon(
	p *daemonrpc.UnrollProgress) *walletdkrpc.ExitProgress {

	if p == nil {
		return nil
	}

	return &walletdkrpc.ExitProgress{
		ConfirmedTxs:      p.GetConfirmedTxs(),
		InFlightTxs:       p.GetInFlightTxs(),
		ReadyTxs:          p.GetReadyTxs(),
		BlockedTxs:        p.GetBlockedTxs(),
		TotalTxs:          p.GetTotalTxs(),
		CurrentLayer:      p.GetCurrentLayer(),
		TotalLayers:       p.GetTotalLayers(),
		TargetConfirmed:   p.GetTargetConfirmed(),
		AllProofConfirmed: p.GetAllProofConfirmed(),
	}
}

// exitCSVFromDaemon projects the daemon's UnrollCSV onto the wallet-facing
// ExitCSV. It returns nil until the target confirms (no CSV countdown yet).
func exitCSVFromDaemon(c *daemonrpc.UnrollCSV) *walletdkrpc.ExitCSV {
	if c == nil {
		return nil
	}

	return &walletdkrpc.ExitCSV{
		TargetConfirmHeight: c.GetTargetConfirmHeight(),
		MaturityHeight:      c.GetMaturityHeight(),
		BlocksRemaining:     c.GetBlocksRemaining(),
		Mature:              c.GetMature(),
	}
}

// exitFeesFromDaemon projects the daemon's UnrollFees onto the wallet-facing
// ExitFees. It returns nil when the daemon supplied no fee breakdown.
func exitFeesFromDaemon(f *daemonrpc.UnrollFees) *walletdkrpc.ExitFees {
	if f == nil {
		return nil
	}

	return &walletdkrpc.ExitFees{
		CpfpFeeSat:      f.GetCpfpFeeSat(),
		SweepFeeSat:     f.GetSweepFeeSat(),
		TotalCostSat:    f.GetTotalCostSat(),
		VtxoAmountSat:   f.GetVtxoAmountSat(),
		NetRecoveredSat: f.GetNetRecoveredSat(),
		FeeRateSatVbyte: f.GetFeeRateSatVbyte(),
		SweepFeeActual:  f.GetSweepFeeActual(),
		SpentSoFarSat:   f.GetSpentSoFarSat(),
	}
}

// exitSummary proxies the daemon's aggregate exit portfolio and projects it
// onto the wallet-facing ExitSummaryResponse.
func (s *Service) exitSummary(ctx context.Context,
	_ *walletdkrpc.ExitSummaryRequest) (*walletdkrpc.ExitSummaryResponse,
	error) {

	if s.deps == nil || s.deps.RPCServer == nil {
		return nil, statusSwapBackendUnavailable()
	}

	result, err := s.deps.RPCServer.ExitSummary(ctx)
	if err != nil {
		return nil, status.Errorf(status.Code(err), "exit summary: %v",
			err)
	}

	resp := &walletdkrpc.ExitSummaryResponse{
		Exits: make(
			[]*walletdkrpc.ExitSummaryItem, 0, len(result.Entries),
		),
		TotalExits:              result.TotalExits,
		TotalVtxoAmountSat:      result.TotalVTXOAmountSat,
		TotalEstFeeSat:          result.TotalEstFeeSat,
		TotalEstNetRecoveredSat: result.TotalEstNetRecoveredSat,
	}
	for i := range result.Entries {
		entry := result.Entries[i]
		resp.Exits = append(resp.Exits, &walletdkrpc.ExitSummaryItem{
			Outpoint:           entry.Outpoint,
			Status:             exitStatusFromDaemon(entry.Status),
			VtxoAmountSat:      entry.VTXOAmountSat,
			EstTotalFeeSat:     entry.EstTotalFeeSat,
			EstNetRecoveredSat: entry.EstNetRecoveredSat,
		})
	}

	return resp, nil
}

// exitStatusFromDaemon maps daemonrpc.UnrollJobStatus onto the
// wallet-facing ExitJobStatus enum. The mapping is 1:1 today; the
// projection sits at the wallet boundary so daemon-side renumbering or
// new internal phases don't leak into the user surface.
func exitStatusFromDaemon(
	s daemonrpc.UnrollJobStatus) walletdkrpc.ExitJobStatus {

	switch s {
	case daemonrpc.UnrollJobStatus_UNROLL_JOB_STATUS_PENDING:
		return walletdkrpc.ExitJobStatus_EXIT_JOB_STATUS_PENDING

	case daemonrpc.UnrollJobStatus_UNROLL_JOB_STATUS_MATERIALIZING:
		return walletdkrpc.ExitJobStatus_EXIT_JOB_STATUS_MATERIALIZING

	case daemonrpc.UnrollJobStatus_UNROLL_JOB_STATUS_CSV_PENDING:
		return walletdkrpc.ExitJobStatus_EXIT_JOB_STATUS_CSV_PENDING

	case daemonrpc.UnrollJobStatus_UNROLL_JOB_STATUS_SWEEPING:
		return walletdkrpc.ExitJobStatus_EXIT_JOB_STATUS_SWEEPING

	case daemonrpc.UnrollJobStatus_UNROLL_JOB_STATUS_COMPLETED:
		return walletdkrpc.ExitJobStatus_EXIT_JOB_STATUS_COMPLETED

	case daemonrpc.UnrollJobStatus_UNROLL_JOB_STATUS_FAILED:
		return walletdkrpc.ExitJobStatus_EXIT_JOB_STATUS_FAILED

	default:
		return walletdkrpc.ExitJobStatus_EXIT_JOB_STATUS_UNSPECIFIED
	}
}

// infeasibilityReasonFromUnroll maps the daemon's unroll feasibility reason
// onto the wallet-facing ExitInfeasibilityReason enum. The projection sits at
// the wallet boundary so daemon-side renumbering or new internal reasons don't
// leak into the user surface.
func infeasibilityReasonFromUnroll(
	r unroll.ExitInfeasibilityReason) walletdkrpc.ExitInfeasibilityReason {

	switch r {
	case unroll.ExitSweepBelowDust:
		return walletdkrpc.ExitInfeasibilityReason_EXIT_INFEASIBILITY_REASON_SWEEP_BELOW_DUST //nolint:ll

	case unroll.ExitUneconomical:
		return walletdkrpc.ExitInfeasibilityReason_EXIT_INFEASIBILITY_REASON_UNECONOMICAL //nolint:ll

	case unroll.ExitWalletUnderfunded:
		return walletdkrpc.ExitInfeasibilityReason_EXIT_INFEASIBILITY_REASON_WALLET_UNDERFUNDED //nolint:ll

	case unroll.ExitWalletTooFewInputs:
		return walletdkrpc.ExitInfeasibilityReason_EXIT_INFEASIBILITY_REASON_WALLET_TOO_FEW_INPUTS //nolint:ll

	default:
		return walletdkrpc.ExitInfeasibilityReason_EXIT_INFEASIBILITY_REASON_UNSPECIFIED //nolint:ll
	}
}

func hashString(hash fmt.Stringer) string {
	if hash == nil {
		return ""
	}

	if h, ok := hash.(*chainhash.Hash); ok && h == nil {
		return ""
	}

	return hash.String()
}

func errorString(err error) string {
	if err == nil {
		return ""
	}

	return err.Error()
}
