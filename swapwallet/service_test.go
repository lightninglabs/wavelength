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

// TestServiceDepositReturnsAddress confirms Deposit calls NewAddress and
// returns a DEPOSIT-kind WalletEntry with the boarding address. v1 does
// NOT register a canonical-id intent for deposits because the daemon
// has no notification hook from boarding-address to the eventual
// boarding txid; correlation between this entry and the later boarding
// ledger row is a v2 task (see swapwallet/doc.go).
func TestServiceDepositReturnsAddress(t *testing.T) {
	t.Parallel()

	svc, _, rpc := newServiceFixture(t)
	rpc.newAddressResp = &daemonrpc.NewAddressResponse{
		Address: "bcrt1qboardingaddr",
	}

	resp, err := svc.Deposit(
		t.Context(), &walletrpc.DepositRequest{
			AmtSatHint: 50_000,
		},
	)
	require.NoError(t, err)
	require.Equal(t, "bcrt1qboardingaddr", resp.GetOnchainAddress())
	require.Equal(
		t, walletrpc.EntryKind_ENTRY_KIND_DEPOSIT,
		resp.GetEntry().GetKind(),
	)
	require.Equal(
		t, int64(50_000), resp.GetEntry().GetAmountSat(),
		"deposit hint must surface on the initial entry",
	)
	require.Equal(
		t, "bcrt1qboardingaddr",
		resp.GetEntry().GetRequest().GetOnchainAddress().GetAddress(),
	)
	require.Equal(
		t, "address_issued",
		resp.GetEntry().GetProgress().GetPhaseLabel(),
	)
}

// TestServiceBalanceProjectsDaemonGetBalance confirms Balance pulls
// from the daemon's GetBalance and projects the unified flat shape.
// confirmed_sat must be the spendable VTXO balance only; boarding
// outputs awaiting round registration go into pending_in_sat
// alongside boarding-unconfirmed so the user does not see boarding
// funds reported as immediately spendable.
func TestServiceBalanceProjectsDaemonGetBalance(t *testing.T) {
	t.Parallel()

	svc, _, rpc := newServiceFixture(t)
	rpc.getBalanceResp = &daemonrpc.GetBalanceResponse{
		VtxoBalanceSat:          75_000,
		BoardingConfirmedSat:    100_000,
		BoardingUnconfirmedSat:  20_000,
		BoardingAdoptedSat:      15_000,
		TotalConfirmedSat:       175_000, // ignored by the mapping
		BoardingPendingSweepSat: 5_000,
	}

	resp, err := svc.Balance(
		t.Context(), &walletrpc.BalanceRequest{},
	)
	require.NoError(t, err)
	require.Equal(t, int64(75_000), resp.GetConfirmedSat())
	require.Equal(t, int64(135_000), resp.GetPendingInSat())
	require.Equal(t, int64(5_000), resp.GetPendingOutSat())
}

// TestServiceBalanceKeepsAdoptedBoardingPending pins issue #542: after a
// boarding UTXO is adopted into a round, the underlying on-chain UTXO is
// spent before the resulting VTXO is live. The balance must keep that value
// pending inbound rather than dropping to zero during commitment confirmation.
func TestServiceBalanceKeepsAdoptedBoardingPending(t *testing.T) {
	t.Parallel()

	svc, _, rpc := newServiceFixture(t)
	rpc.getBalanceResp = &daemonrpc.GetBalanceResponse{
		BoardingAdoptedSat: 100_000,
	}

	resp, err := svc.Balance(
		t.Context(), &walletrpc.BalanceRequest{},
	)
	require.NoError(t, err)
	require.Equal(t, int64(0), resp.GetConfirmedSat())
	require.Equal(t, int64(100_000), resp.GetPendingInSat())
	require.Equal(t, int64(0), resp.GetPendingOutSat())
}

// TestServiceBalanceConfirmedExcludesBoardingUTXOs pins the boarding
// reproduction from issue #502: a wallet that holds one live VTXO and
// one confirmed-but-not-yet-boarded UTXO must report only the VTXO
// under confirmed_sat. The boarding UTXO is not VTXO-spendable until a
// round adopts it, so its value belongs in pending_in_sat per the
// proto contract on BalanceResponse.confirmed_sat
// ("total spendable VTXO amount").
func TestServiceBalanceConfirmedExcludesBoardingUTXOs(t *testing.T) {
	t.Parallel()

	const (
		vtxoSat            = int64(99_745)
		boardingConfirmed  = int64(100_000)
		expectedConfirmed  = vtxoSat
		expectedPendingIn  = boardingConfirmed
		expectedPendingOut = int64(0)
	)

	svc, _, rpc := newServiceFixture(t)
	rpc.getBalanceResp = &daemonrpc.GetBalanceResponse{
		VtxoBalanceSat:       vtxoSat,
		BoardingConfirmedSat: boardingConfirmed,
		TotalConfirmedSat:    vtxoSat + boardingConfirmed,
	}

	resp, err := svc.Balance(
		t.Context(), &walletrpc.BalanceRequest{},
	)
	require.NoError(t, err)
	require.Equal(t, expectedConfirmed, resp.GetConfirmedSat())
	require.Equal(t, expectedPendingIn, resp.GetPendingInSat())
	require.Equal(t, expectedPendingOut, resp.GetPendingOutSat())
}

// TestServiceStatusComposesInfoBalanceAndPending confirms Status reads
// GetInfo, GetBalance, and the pending count via the history merger.
func TestServiceStatusComposesInfoBalanceAndPending(t *testing.T) {
	t.Parallel()

	svc, swap, rpc := newServiceFixture(t)
	rpc.getInfoResp = &daemonrpc.GetInfoResponse{
		WalletState: daemonrpc.WalletState_WALLET_STATE_READY,
		Network:     "regtest",
	}
	rpc.getBalanceResp = &daemonrpc.GetBalanceResponse{
		VtxoBalanceSat:    1_000,
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

// TestServiceStatusReportsSyncingWalletUnlocked confirms the wallet
// facade reports an unlocked wallet during daemon sync without
// promoting it to ready.
func TestServiceStatusReportsSyncingWalletUnlocked(t *testing.T) {
	t.Parallel()

	svc, swap, rpc := newServiceFixture(t)
	rpc.getInfoResp = &daemonrpc.GetInfoResponse{
		WalletState: daemonrpc.WalletState_WALLET_STATE_SYNCING,
		Network:     "regtest",
	}
	rpc.getBalanceResp = &daemonrpc.GetBalanceResponse{}
	rpc.listTxResp = &daemonrpc.ListTransactionsResponse{}
	swap.listSwapsResp = &swapclientrpc.ListSwapsResponse{}

	resp, err := svc.Status(
		t.Context(), &walletrpc.StatusRequest{},
	)
	require.NoError(t, err)
	require.False(t, resp.GetReady())
	require.True(t, resp.GetUnlocked())
}
