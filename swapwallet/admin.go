//go:build walletdkrpc && swapruntime

package swapwallet

import (
	"context"
	"strings"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/rpc/walletdkrpc"
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
		return nil, status.Error(
			codes.Unavailable, ErrSwapBackendUnavailable.Error(),
		)
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
		RecoveredVhtlcs:            initResp.GetRecoveredVhtlcs(),
		RecoveredVhtlcRefunds:      initResp.GetRecoveredVhtlcRefunds(),
		RecoveredVhtlcClaims:       initResp.GetRecoveredVhtlcClaims(),
	}, nil
}

// unlock proxies daemonrpc.UnlockWallet. This is an admin-shape handler
// that runs before the swap runtime is live.
func (s *Service) unlock(ctx context.Context, req *walletdkrpc.UnlockRequest) (
	*walletdkrpc.UnlockResponse, error) {

	if s.deps == nil || s.deps.RPCServer == nil {
		return nil, status.Error(
			codes.Unavailable, ErrSwapBackendUnavailable.Error(),
		)
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
		return nil, status.Error(
			codes.Unavailable, ErrSwapBackendUnavailable.Error(),
		)
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

	entry := unilateralExitEntryStub(req.GetOutpoint())
	s.runtime.trackPendingEntryWithoutTimeout(entry)
	s.runtime.emit(entry)

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

// exitStatus proxies daemonrpc.GetUnrollStatus and projects the daemon's
// UnrollJobStatus onto the wallet-facing ExitJobStatus.
func (s *Service) exitStatus(ctx context.Context,
	req *walletdkrpc.ExitStatusRequest) (*walletdkrpc.ExitStatusResponse,
	error) {

	if s.deps == nil || s.deps.RPCServer == nil {
		return nil, status.Error(
			codes.Unavailable, ErrSwapBackendUnavailable.Error(),
		)
	}

	if req.GetOutpoint() == "" {
		return nil, status.Error(
			codes.InvalidArgument, "outpoint is required",
		)
	}

	resp, err := s.deps.RPCServer.GetUnrollStatus(
		ctx, &daemonrpc.GetUnrollStatusRequest{
			Outpoint: req.GetOutpoint(),
		},
	)
	if err != nil {
		return nil, status.Errorf(status.Code(err), "exit status: %v",
			err)
	}

	return &walletdkrpc.ExitStatusResponse{
		Found:     resp.GetFound(),
		Status:    exitStatusFromDaemon(resp.GetStatus()),
		SweepTxid: resp.GetSweepTxid(),
		LastError: resp.GetLastError(),
	}, nil
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
