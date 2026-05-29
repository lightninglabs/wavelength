//go:build walletdkrpc && swapruntime

package swapwallet

import (
	"context"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/rpc/walletdkrpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

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
		},
	)
	if err != nil {
		return nil, status.Errorf(status.Code(err), "init wallet: %v",
			err)
	}

	return &walletdkrpc.CreateResponse{
		Mnemonic:       mnemonic,
		IdentityPubkey: initResp.GetIdentityPubkey(),
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

// exit proxies daemonrpc.Unroll. The daemon's unroll registry remains the
// authoritative store; the wallet layer tracks only the friendly pending row
// so activity can show the user's exit attempt immediately.
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
	}, nil
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
