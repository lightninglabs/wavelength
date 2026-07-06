//go:build walletdkrpc && swapruntime

package swapwallet

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/lightninglabs/darepo-client/darepod"
	"github.com/lightninglabs/darepo-client/rpc/swapclientrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// fakeResumeOnlyBackend implements darepod.SwapBackend but deliberately
// does NOT implement swapclientrpc.SwapClientServiceServer so it forces
// the type-assertion error path inside Register. Used to assert the
// SuppressResume-failover hook fires.
type fakeResumeOnlyBackend struct {
	resumeCalls atomic.Int32
}

func (f *fakeResumeOnlyBackend) ResumePending(_ context.Context) {
	f.resumeCalls.Add(1)
}

// fakeWalletBackend satisfies both the in-Go resume handle and the gRPC-shaped
// swap service required by the wallet registrar.
type fakeWalletBackend struct {
	swapclientrpc.UnimplementedSwapClientServiceServer

	resumeCalls atomic.Int32
}

func (f *fakeWalletBackend) ResumePending(_ context.Context) {
	f.resumeCalls.Add(1)
}

// TestChainParamsForWalletNetworkAcceptsTestNet4 verifies the walletdkrpc
// subserver accepts every network string the main daemon config advertises.
func TestChainParamsForWalletNetworkAcceptsTestNet4(t *testing.T) {
	t.Parallel()

	params, err := chainParamsForWalletNetwork("testnet4")
	require.NoError(t, err)
	require.Same(t, &chaincfg.TestNet4Params, params)
}

// TestRegisterDefersResumeUntilWalletReadyHook confirms the wallet subserver
// registers its RPC surface immediately but waits for the daemon's wallet-ready
// phase before starting persisted swap workers.
func TestRegisterDefersResumeUntilWalletReadyHook(t *testing.T) {
	t.Parallel()

	backend := &fakeWalletBackend{}
	cfg := &darepod.Config{
		Network: "regtest",
		Swap: &darepod.SwapConfig{
			Backend:        backend,
			SuppressResume: true,
		},
	}

	cleanup, err := Register(
		t.Context(), grpc.NewServer(), nil, cfg,
	)
	require.NoError(t, err)
	require.NotNil(t, cleanup)
	t.Cleanup(cleanup)

	require.Equal(
		t, int32(0), backend.resumeCalls.Load(),
		"registering walletdkrpc while locked must not resume swaps",
	)
	require.Len(t, cfg.WalletReadyHooks, 1)

	require.NoError(t, cfg.WalletReadyHooks[0](t.Context()))
	require.Equal(t, int32(1), backend.resumeCalls.Load())

	require.NoError(t, cfg.WalletReadyHooks[0](t.Context()))
	require.Equal(
		t, int32(1), backend.resumeCalls.Load(),
		"wallet-ready hook must be idempotent",
	)
}

// TestRegisterRecoversResumeOnTypeAssertionFailure asserts that when
// Register bails out because the swap backend does not satisfy
// SwapClientServiceServer, it compensates for the SuppressResume
// handshake by calling Backend.ResumePending. Without this, the swap
// subserver would have skipped its own resume sweep (the walletdkrpc
// build sets cfg.Swap.SuppressResume = true unconditionally before
// this registrar runs) and no actor would ever drive pending workers.
func TestRegisterRecoversResumeOnTypeAssertionFailure(t *testing.T) {
	t.Parallel()

	backend := &fakeResumeOnlyBackend{}
	cfg := &darepod.Config{
		Swap: &darepod.SwapConfig{
			Backend:        backend,
			SuppressResume: true,
		},
	}

	cleanup, err := Register(t.Context(), nil, nil, cfg)
	require.Error(
		t, err, "a backend that does not implement "+
			"SwapClientServiceServer must surface a "+
			"registration error",
	)
	require.Nil(
		t, cleanup,
		"a failed Register must NOT return a cleanup function",
	)
	require.Equal(
		t, int32(1), backend.resumeCalls.Load(),
		"failover must invoke ResumePending exactly once so the "+
			"daemon does not leak SuppressResume into a "+
			"never-resumed swap subsystem",
	)
}

// TestRegisterRejectsNilBackend asserts the missing-handle error path is
// stable and does not panic. No failover is expected here because there
// is nothing to resume: the swap subserver itself never published its
// handle.
func TestRegisterRejectsNilBackend(t *testing.T) {
	t.Parallel()

	cfg := &darepod.Config{
		Swap: &darepod.SwapConfig{
			Backend: nil,
		},
	}
	cleanup, err := Register(t.Context(), nil, nil, cfg)
	require.ErrorIs(t, err, ErrSwapBackendUnavailable)
	require.Nil(t, cleanup)
}
