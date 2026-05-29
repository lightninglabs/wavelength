//go:build walletdkrpc && swapruntime

package swapwallet

import (
	"errors"
	"testing"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/rpc/walletdkrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// newAdminFixture wires a Service handle with a fake RPC server suitable
// for exercising the Create/Unlock/Exit/ExitStatus proxies. The runtime
// is created but not started; admin handlers MUST work pre-runtime.
func newAdminFixture(t *testing.T) (*Service, *fakeRPCServer) {
	t.Helper()

	rpc := &fakeRPCServer{}
	deps := &Deps{
		SwapService: &fakeSwapService{},
		RPCServer:   rpc,
	}
	runtime := newRuntime(t.Context(), deps)
	t.Cleanup(runtime.stop)

	return newService(deps, runtime), rpc
}

// TestCreateGeneratesSeedWhenMnemonicEmpty exercises the fresh-wallet
// path: GenSeed is called, the returned mnemonic is plumbed into
// InitWallet, and the response echoes the new mnemonic so the caller can
// record it.
func TestCreateGeneratesSeedWhenMnemonicEmpty(t *testing.T) {
	t.Parallel()

	svc, rpc := newAdminFixture(t)
	rpc.genSeedResp = &daemonrpc.GenSeedResponse{
		Mnemonic: []string{
			"word1",
			"word2",
			"word3",
		},
	}
	rpc.initWalletResp = &daemonrpc.InitWalletResponse{
		IdentityPubkey: "deadbeef",
	}

	resp, err := svc.Create(t.Context(), &walletdkrpc.CreateRequest{
		WalletPassword: []byte("hunter2hunter2"),
	})
	require.NoError(t, err)
	require.Equal(
		t, []string{"word1", "word2", "word3"}, resp.GetMnemonic(),
	)
	require.Equal(t, "deadbeef", resp.GetIdentityPubkey())
	require.Equal(t, 1, rpc.genSeedCalls)
	require.Equal(t, 1, rpc.initWalletCalls)
	require.Equal(
		t, []string{"word1", "word2", "word3"},
		rpc.initWalletLast.GetMnemonic(),
	)
}

// TestCreateRecoveryEchoesProvidedMnemonic confirms supplying a mnemonic
// skips GenSeed and echoes the same mnemonic in the response.
func TestCreateRecoveryEchoesProvidedMnemonic(t *testing.T) {
	t.Parallel()

	svc, rpc := newAdminFixture(t)
	rpc.initWalletResp = &daemonrpc.InitWalletResponse{
		IdentityPubkey: "cafe",
	}

	recovery := []string{"alpha", "beta", "gamma"}
	resp, err := svc.Create(t.Context(), &walletdkrpc.CreateRequest{
		WalletPassword: []byte("hunter2hunter2"),
		Mnemonic:       recovery,
	})
	require.NoError(t, err)
	require.Equal(t, recovery, resp.GetMnemonic())
	require.Equal(t, "cafe", resp.GetIdentityPubkey())
	require.Equal(t, 0, rpc.genSeedCalls, "recovery path must skip GenSeed")
	require.Equal(t, recovery, rpc.initWalletLast.GetMnemonic())
}

// TestCreateRejectsEmptyPassword confirms the handler rejects missing
// passwords with InvalidArgument before touching the daemon.
func TestCreateRejectsEmptyPassword(t *testing.T) {
	t.Parallel()

	svc, rpc := newAdminFixture(t)
	_, err := svc.Create(t.Context(), &walletdkrpc.CreateRequest{})
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Equal(t, 0, rpc.genSeedCalls)
	require.Equal(t, 0, rpc.initWalletCalls)
}

// TestCreateRequiresRPCServer confirms an unconfigured backend surfaces
// Unavailable rather than panicking.
func TestCreateRequiresRPCServer(t *testing.T) {
	t.Parallel()

	deps := &Deps{}
	runtime := newRuntime(t.Context(), deps)
	t.Cleanup(runtime.stop)
	svc := newService(deps, runtime)

	_, err := svc.Create(t.Context(), &walletdkrpc.CreateRequest{
		WalletPassword: []byte("password"),
	})
	require.Error(t, err)
	require.Equal(t, codes.Unavailable, status.Code(err))
}

// TestUnlockProxiesDaemon confirms Unlock plumbs the caller's password
// through and returns the daemon's identity pubkey.
func TestUnlockProxiesDaemon(t *testing.T) {
	t.Parallel()

	svc, rpc := newAdminFixture(t)
	rpc.unlockWalletResp = &daemonrpc.UnlockWalletResponse{
		IdentityPubkey: "ffff",
	}

	resp, err := svc.Unlock(t.Context(), &walletdkrpc.UnlockRequest{
		WalletPassword: []byte("hunter2hunter2"),
	})
	require.NoError(t, err)
	require.Equal(t, "ffff", resp.GetIdentityPubkey())
	require.Equal(t, 1, rpc.unlockWalletCalls)
	require.Equal(
		t, []byte("hunter2hunter2"),
		rpc.unlockWalletLast.GetWalletPassword(),
	)
}

// TestUnlockRejectsEmptyPassword confirms Unlock rejects missing
// passwords with InvalidArgument.
func TestUnlockRejectsEmptyPassword(t *testing.T) {
	t.Parallel()

	svc, _ := newAdminFixture(t)
	_, err := svc.Unlock(t.Context(), &walletdkrpc.UnlockRequest{})
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

// TestExitProxiesUnroll confirms Exit plumbs the outpoint and surfaces
// the created flag plus actor id.
func TestExitProxiesUnroll(t *testing.T) {
	t.Parallel()

	svc, rpc := newAdminFixture(t)
	rpc.unrollResp = &daemonrpc.UnrollResponse{
		Created: true,
		ActorId: "exit-job-42",
	}

	resp, err := svc.Exit(t.Context(), &walletdkrpc.ExitRequest{
		Outpoint: "abc:0",
	})
	require.NoError(t, err)
	require.True(t, resp.GetCreated())
	require.Equal(t, "exit-job-42", resp.GetActorId())
	require.Equal(t, "abc:0", rpc.unrollLast.GetOutpoint())

	entries := svc.runtime.pendingSnapshot()
	require.Len(t, entries, 1)
	require.Equal(t, "abc:0", entries[0].GetId())
	require.Equal(
		t, walletdkrpc.EntryKind_ENTRY_KIND_EXIT, entries[0].GetKind(),
	)
	require.Equal(
		t, walletdkrpc.EntryStatus_ENTRY_STATUS_PENDING,
		entries[0].GetStatus(),
	)
}

// TestExitRejectsEmptyOutpoint confirms a missing outpoint is rejected
// before any daemon call.
func TestExitRejectsEmptyOutpoint(t *testing.T) {
	t.Parallel()

	svc, rpc := newAdminFixture(t)
	_, err := svc.Exit(t.Context(), &walletdkrpc.ExitRequest{})
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Equal(t, 0, rpc.unrollCalls)
}

// TestExitStatusMapsAllPhases sanity-checks that every daemon
// UnrollJobStatus value maps to a wallet ExitJobStatus value (1:1).
func TestExitStatusMapsAllPhases(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   daemonrpc.UnrollJobStatus
		want walletdkrpc.ExitJobStatus
	}{
		{
			daemonrpc.UnrollJobStatus_UNROLL_JOB_STATUS_PENDING,
			walletdkrpc.ExitJobStatus_EXIT_JOB_STATUS_PENDING,
		},
		{
			daemonrpc.
				UnrollJobStatus_UNROLL_JOB_STATUS_MATERIALIZING,
			walletdkrpc.ExitJobStatus_EXIT_JOB_STATUS_MATERIALIZING,
		},
		{
			daemonrpc.UnrollJobStatus_UNROLL_JOB_STATUS_CSV_PENDING,
			walletdkrpc.ExitJobStatus_EXIT_JOB_STATUS_CSV_PENDING,
		},
		{
			daemonrpc.UnrollJobStatus_UNROLL_JOB_STATUS_SWEEPING,
			walletdkrpc.ExitJobStatus_EXIT_JOB_STATUS_SWEEPING,
		},
		{
			daemonrpc.UnrollJobStatus_UNROLL_JOB_STATUS_COMPLETED,
			walletdkrpc.ExitJobStatus_EXIT_JOB_STATUS_COMPLETED,
		},
		{
			daemonrpc.UnrollJobStatus_UNROLL_JOB_STATUS_FAILED,
			walletdkrpc.ExitJobStatus_EXIT_JOB_STATUS_FAILED,
		},
		{
			daemonrpc.UnrollJobStatus_UNROLL_JOB_STATUS_UNSPECIFIED,
			walletdkrpc.ExitJobStatus_EXIT_JOB_STATUS_UNSPECIFIED,
		},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, exitStatusFromDaemon(tc.in))
	}
}

// TestExitStatusProxiesAndProjects confirms ExitStatus returns the
// found/sweep/last_error fields and the projected status enum.
func TestExitStatusProxiesAndProjects(t *testing.T) {
	t.Parallel()

	svc, rpc := newAdminFixture(t)
	rpc.unrollStatusResp = &daemonrpc.GetUnrollStatusResponse{
		Found:     true,
		Status:    daemonrpc.UnrollJobStatus_UNROLL_JOB_STATUS_SWEEPING,
		SweepTxid: "sweep-txid",
	}

	resp, err := svc.ExitStatus(
		t.Context(), &walletdkrpc.ExitStatusRequest{
			Outpoint: "abc:0",
		},
	)
	require.NoError(t, err)
	require.True(t, resp.GetFound())
	require.Equal(
		t, walletdkrpc.ExitJobStatus_EXIT_JOB_STATUS_SWEEPING,
		resp.GetStatus(),
	)
	require.Equal(t, "sweep-txid", resp.GetSweepTxid())
}

// TestAdminHandlersSurfaceDaemonErrors confirms that downstream daemon
// errors are wrapped (not swallowed) and surface through the proxy.
func TestAdminHandlersSurfaceDaemonErrors(t *testing.T) {
	t.Parallel()

	svc, rpc := newAdminFixture(t)
	sentinel := errors.New("daemon-down")
	rpc.genSeedErr = sentinel

	_, err := svc.Create(t.Context(), &walletdkrpc.CreateRequest{
		WalletPassword: []byte("hunter2hunter2"),
	})
	require.ErrorContains(t, err, sentinel.Error())

	rpc.unlockWalletErr = sentinel
	_, err = svc.Unlock(t.Context(), &walletdkrpc.UnlockRequest{
		WalletPassword: []byte("hunter2hunter2"),
	})
	require.ErrorContains(t, err, sentinel.Error())

	rpc.unrollErr = sentinel
	_, err = svc.Exit(t.Context(), &walletdkrpc.ExitRequest{
		Outpoint: "abc:0",
	})
	require.ErrorContains(t, err, sentinel.Error())

	rpc.unrollStatusErr = sentinel
	_, err = svc.ExitStatus(t.Context(), &walletdkrpc.ExitStatusRequest{
		Outpoint: "abc:0",
	})
	require.ErrorContains(t, err, sentinel.Error())
}

// TestAdminHandlersPreserveGRPCCode confirms the admin handlers
// preserve gRPC status codes from downstream daemon errors instead of
// flattening them to codes.Unknown. This is the contract M8 enforces:
// a daemon returning codes.AlreadyExists must surface to the caller
// with the same code so the CLI can branch on it.
func TestAdminHandlersPreserveGRPCCode(t *testing.T) {
	t.Parallel()

	svc, rpc := newAdminFixture(t)
	rpc.genSeedErr = status.Error(
		codes.AlreadyExists, "wallet exists",
	)

	_, err := svc.Create(t.Context(), &walletdkrpc.CreateRequest{
		WalletPassword: []byte("hunter2hunter2"),
	})
	require.Equal(t, codes.AlreadyExists, status.Code(err))

	rpc.unlockWalletErr = status.Error(
		codes.PermissionDenied, "bad password",
	)
	_, err = svc.Unlock(t.Context(), &walletdkrpc.UnlockRequest{
		WalletPassword: []byte("hunter2hunter2"),
	})
	require.Equal(t, codes.PermissionDenied, status.Code(err))

	rpc.unrollErr = status.Error(
		codes.FailedPrecondition, "not unlocked",
	)
	_, err = svc.Exit(t.Context(), &walletdkrpc.ExitRequest{
		Outpoint: "abc:0",
	})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
}
