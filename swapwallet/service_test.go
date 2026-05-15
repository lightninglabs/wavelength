//go:build walletrpc && swapruntime

package swapwallet

import (
	"testing"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/rpc/swapclientrpc"
	"github.com/lightninglabs/darepo-client/rpc/walletrpc"
	"github.com/stretchr/testify/require"
)

// newServiceFixture builds a Service with fake deps so each gRPC handler
// can be exercised without a real daemon.
func newServiceFixture(t *testing.T) (*Service, *fakeSwapService,
	*fakeRPCServer) {

	t.Helper()

	swap := &fakeSwapService{}
	rpc := &fakeRPCServer{}
	deps := &Deps{
		SwapService: swap,
		RPCServer:   rpc,
	}
	runtime := newRuntime(t.Context(), deps)
	t.Cleanup(runtime.stop)

	return newService(deps, runtime), swap, rpc
}

// TestServiceDepositRegistersIntent confirms Deposit calls NewAddress and
// registers the canonical-id intent with the returned address.
func TestServiceDepositRegistersIntent(t *testing.T) {
	t.Parallel()

	svc, _, rpc := newServiceFixture(t)
	rpc.newAddressResp = &daemonrpc.NewAddressResponse{
		Address: "bcrt1qboardingaddr",
	}

	resp, err := svc.Deposit(
		t.Context(), &walletrpc.DepositRequest{AmtSatHint: 50_000},
	)
	require.NoError(t, err)
	require.Equal(t, "bcrt1qboardingaddr", resp.GetOnchainAddress())
	require.Equal(t,
		walletrpc.EntryKind_ENTRY_KIND_DEPOSIT,
		resp.GetEntry().GetKind(),
	)

	// Intent must be registered under the canonical id.
	canonical := svc.runtime.resolveCanonicalID(
		"synthetic", "", "", nil, "bcrt1qboardingaddr",
	)
	require.Equal(t, resp.GetEntry().GetId(), canonical,
		"the deposit intent must be registered by boarding address")
}

// TestServiceBalanceProjectsDaemonGetBalance confirms Balance pulls from
// the daemon's GetBalance and projects the unified flat shape.
func TestServiceBalanceProjectsDaemonGetBalance(t *testing.T) {
	t.Parallel()

	svc, _, rpc := newServiceFixture(t)
	rpc.getBalanceResp = &daemonrpc.GetBalanceResponse{
		TotalConfirmedSat:       100_000,
		BoardingUnconfirmedSat:  20_000,
		BoardingPendingSweepSat: 5_000,
	}

	resp, err := svc.Balance(
		t.Context(), &walletrpc.BalanceRequest{},
	)
	require.NoError(t, err)
	require.Equal(t, int64(100_000), resp.GetConfirmedSat())
	require.Equal(t, int64(20_000), resp.GetPendingInSat())
	require.Equal(t, int64(5_000), resp.GetPendingOutSat())
}

// TestServiceStatusComposesInfoBalanceAndPending confirms Status reads
// GetInfo, GetBalance, and the pending count via the history merger.
func TestServiceStatusComposesInfoBalanceAndPending(t *testing.T) {
	t.Parallel()

	svc, swap, rpc := newServiceFixture(t)
	rpc.getInfoResp = &daemonrpc.GetInfoResponse{
		WalletReady: true,
		Network:     "regtest",
	}
	rpc.getBalanceResp = &daemonrpc.GetBalanceResponse{
		TotalConfirmedSat: 1_000,
	}
	swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{
		Swaps: []*swapclientrpc.SwapSummary{
			{
				PaymentHash: "still-pending",
				Direction: swapclientrpc.
					SwapDirection_SWAP_DIRECTION_PAY,
				Pending: true,
			},
		},
	}
	rpc.listTxResp = &daemonrpc.ListTransactionsResponse{}

	resp, err := svc.Status(
		t.Context(), &walletrpc.StatusRequest{},
	)
	require.NoError(t, err)
	require.True(t, resp.GetReady())
	require.True(t, resp.GetUnlocked())
	require.Equal(t, "regtest", resp.GetNetwork())
	require.Equal(t, int64(1_000), resp.GetBalance().GetConfirmedSat())
	require.Equal(t, uint32(1), resp.GetPendingCount())
}
