//go:build walletdkrpc && swapruntime

package swapwallet

import (
	"context"
	"testing"

	"github.com/lightninglabs/darepo-client/daemonrpc"
	"github.com/lightninglabs/darepo-client/rpc/swapclientrpc"
	"github.com/lightninglabs/darepo-client/rpc/walletdkrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
// returns a DEPOSIT-kind WalletEntry with the boarding address, keyed by the
// address-scoped canonical id.
func TestServiceDepositReturnsAddress(t *testing.T) {
	t.Parallel()

	svc, _, rpc := newServiceFixture(t)
	rpc.newAddressResp = &daemonrpc.NewAddressResponse{
		Address: "bcrt1qboardingaddr",
	}

	resp, err := svc.Deposit(
		t.Context(), &walletdkrpc.DepositRequest{
			AmtSatHint: 50_000,
		},
	)
	require.NoError(t, err)
	require.Equal(t, "bcrt1qboardingaddr", resp.GetOnchainAddress())
	require.Equal(
		t, walletdkrpc.EntryKind_ENTRY_KIND_DEPOSIT,
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
	require.Equal(
		t, "deposit-bcrt1qboardingaddr", resp.GetEntry().GetId(),
	)
}

// TestServiceDepositDoesNotProjectAtAllocation confirms Deposit does NOT
// persist a row when an address is merely generated — allocating an address is
// not a pending deposit. The returned entry still carries the address-scoped
// id the confirmed deposit will later use, so a caller can correlate.
func TestServiceDepositDoesNotProjectAtAllocation(t *testing.T) {
	t.Parallel()

	swap := &fakeSwapService{}
	rpc := &fakeRPCServer{
		newAddressResp: &daemonrpc.NewAddressResponse{
			Address: "bcrt1qboardingaddr",
		},
	}
	store := &fakeActivityProjector{}
	deps := &Deps{SwapService: swap, RPCServer: rpc, ActivityStore: store}
	runtime := newRuntime(t.Context(), deps)
	t.Cleanup(runtime.stop)
	svc := newService(deps, runtime)

	resp, err := svc.Deposit(
		t.Context(), &walletdkrpc.DepositRequest{
			AmtSatHint: 50_000,
		},
	)
	require.NoError(t, err)
	require.Equal(
		t, "deposit-bcrt1qboardingaddr", resp.GetEntry().GetId(),
	)
	require.Equal(
		t, 0, store.count(),
		"generating an address must not persist a pending row",
	)
}

// TestServiceWalletVerbsRejectLockedWalletBeforeWork confirms wallet verbs
// fail with an actionable wallet-readiness status before reaching swap or
// address-generation work that requires unlocked key material.
func TestServiceWalletVerbsRejectLockedWalletBeforeWork(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		call func(context.Context, *Service) error
	}{
		{
			name: "prepare send",
			call: func(ctx context.Context, svc *Service) error {
				_, err := svc.PrepareSend(
					ctx, &walletdkrpc.PrepareSendRequest{},
				)

				return err
			},
		},
		{
			name: "send",
			call: func(ctx context.Context, svc *Service) error {
				_, err := svc.Send(
					ctx, &walletdkrpc.SendRequest{},
				)

				return err
			},
		},
		{
			name: "recv",
			call: func(ctx context.Context, svc *Service) error {
				_, err := svc.Recv(
					ctx, &walletdkrpc.RecvRequest{
						AmtSat: 50_000,
					},
				)

				return err
			},
		},
		{
			name: "deposit",
			call: func(ctx context.Context, svc *Service) error {
				_, err := svc.Deposit(
					ctx, &walletdkrpc.DepositRequest{},
				)

				return err
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			svc, swap, rpc := newServiceFixture(t)
			rpc.getInfoResp = &daemonrpc.GetInfoResponse{
				WalletState: daemonrpc.WalletState_WALLET_STATE_LOCKED,
			}

			err := tc.call(t.Context(), svc)
			require.Error(t, err)
			require.True(t, daemonrpc.IsWalletNotReadyError(err))
			require.Equal(
				t, codes.FailedPrecondition, status.Code(err),
			)

			state, ok := daemonrpc.WalletNotReadyState(err)
			require.True(t, ok)
			require.Equal(
				t, daemonrpc.WalletNotReadyStateLocked, state,
			)
			require.Equal(t, 0, swap.startReceiveCalls)
		})
	}
}

// TestServiceRecvWithoutRPCServerReturnsUnavailable confirms the shared
// readiness gate preserves a stable wire code for missing backend plumbing.
func TestServiceRecvWithoutRPCServerReturnsUnavailable(t *testing.T) {
	t.Parallel()

	swap := &fakeSwapService{}
	deps := &Deps{
		SwapService: swap,
	}
	runtime := newRuntime(t.Context(), deps)
	t.Cleanup(runtime.stop)

	svc := newService(deps, runtime)
	_, err := svc.Recv(
		t.Context(), &walletdkrpc.RecvRequest{
			AmtSat: 50_000,
		},
	)
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.Unavailable, st.Code())

	// The readiness gate's pre-formed status carries the machine-readable
	// reason so the SDK can reconstruct it like an interceptor-mapped
	// sentinel.
	require.Equal(
		t, walletdkrpc.ReasonSwapBackendUnavailable,
		errorInfoReason(t, st),
	)
	require.Equal(t, 0, swap.startReceiveCalls)
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
		t.Context(), &walletdkrpc.BalanceRequest{},
	)
	require.NoError(t, err)
	require.Equal(t, int64(75_000), resp.GetConfirmedSat())
	require.Equal(t, int64(135_000), resp.GetPendingInSat())
	require.Equal(t, int64(5_000), resp.GetPendingOutSat())
}

func TestServiceBalanceIncludesCredits(t *testing.T) {
	t.Parallel()

	svc, swap, rpc := newServiceFixture(t)
	rpc.getBalanceResp = &daemonrpc.GetBalanceResponse{
		VtxoBalanceSat: 75_000,
	}
	swap.listCreditsResp = &swapclientrpc.ListCreditsResponse{
		AvailableSat: 12_345,
		ReservedSat:  6_789,
	}

	resp, err := svc.Balance(
		t.Context(), &walletdkrpc.BalanceRequest{},
	)
	require.NoError(t, err)
	require.Equal(t, int64(75_000), resp.GetConfirmedSat())
	require.Equal(t, uint64(12_345), resp.GetCreditAvailableSat())
	require.Equal(t, uint64(6_789), resp.GetCreditReservedSat())
	require.Equal(t, 1, swap.listCreditsCalls)
	require.Equal(t, uint32(1), swap.listCreditsLast.GetLimit())
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
		t.Context(), &walletdkrpc.BalanceRequest{},
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
		t.Context(), &walletdkrpc.BalanceRequest{},
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
		t.Context(), &walletdkrpc.StatusRequest{},
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
		t.Context(), &walletdkrpc.StatusRequest{},
	)
	require.NoError(t, err)
	require.False(t, resp.GetReady())
	require.True(t, resp.GetUnlocked())
}
