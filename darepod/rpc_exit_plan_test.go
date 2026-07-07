package darepod

import (
	"testing"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/lightninglabs/darepo-client/unroll"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func newReadyExitPlanRPCServer() *RPCServer {
	walletReady := make(chan struct{})
	close(walletReady)

	return &RPCServer{
		server: &Server{
			walletReady:  walletReady,
			chainParams:  &chaincfg.RegressionNetParams,
			chainBackend: nil,
		},
	}
}

func TestGetExitPlanRejectsEmptyOutpoints(t *testing.T) {
	t.Parallel()

	r := newReadyExitPlanRPCServer()
	_, err := r.GetExitPlan(t.Context(), &ExitPlanRequest{})
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestGetExitPlanRejectsUninitializedStore(t *testing.T) {
	t.Parallel()

	// The VTXO store check is request-wide: a nil store fails the whole
	// call rather than producing per-outpoint errors.
	r := newReadyExitPlanRPCServer()
	_, err := r.GetExitPlan(t.Context(), &ExitPlanRequest{
		Outpoints: []string{"not-an-outpoint"},
	})
	require.Error(t, err)
	require.Equal(t, codes.Unavailable, status.Code(err))
}

func TestSweepWalletRejectsMissingDestination(t *testing.T) {
	t.Parallel()

	r := newReadyExitPlanRPCServer()
	_, err := r.SweepWallet(t.Context(), &SweepWalletRequest{})
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestSweepWalletRejectsNegativeFeeRate(t *testing.T) {
	t.Parallel()

	addr, err := btcaddr.NewAddressWitnessPubKeyHash(
		make([]byte, 20), &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)

	r := newReadyExitPlanRPCServer()
	_, err = r.SweepWallet(t.Context(), &SweepWalletRequest{
		DestinationAddress: addr.String(),
		FeeRateSatPerVByte: -1,
	})
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

// Note: the preview math, dust boundary, fee-cap, and network-validation
// coverage for the wallet sweep moved into the wallet package alongside the
// handler that now owns that logic (wallet/wallet_sweep_actor_test.go). The
// remaining tests here exercise the RPC shim's request-surface validation.

// TestClaimExitFunding verifies that a feasible exit's reserved inputs and
// fee budget are subtracted from the running wallet snapshot, clamping at
// zero.
func TestClaimExitFunding(t *testing.T) {
	t.Parallel()

	t.Run("decrements inputs and balance", func(t *testing.T) {
		t.Parallel()

		got := claimExitFunding(
			unroll.ExitFundingSnapshot{
				WalletConfirmedSat: 30_000,
				WalletUsableInputs: 3,
			},
			unroll.ExitFeasibility{
				RequiredWalletInputs: 1,
				CPFPFeeTotalSat:      6_000,
			},
		)
		require.Equal(t, 2, got.WalletUsableInputs)
		require.Equal(
			t, btcutil.Amount(24_000), got.WalletConfirmedSat,
		)
	})

	t.Run("clamps at zero", func(t *testing.T) {
		t.Parallel()

		got := claimExitFunding(
			unroll.ExitFundingSnapshot{
				WalletConfirmedSat: 1_000,
				WalletUsableInputs: 1,
			},
			unroll.ExitFeasibility{
				RequiredWalletInputs: 2,
				CPFPFeeTotalSat:      5_000,
			},
		)
		require.Equal(t, 0, got.WalletUsableInputs)
		require.Equal(t, btcutil.Amount(0), got.WalletConfirmedSat)
	})
}

// TestExitPlanBatchSharedSupplyDecrements locks the multi-outpoint accounting
// fix: two VTXOs that each need one fee input, against a wallet holding
// exactly one usable input, must NOT both report ready. The first exit claims
// the only input, so the second -- assessed against the decremented wallet --
// is left short. Without the running allocation both would independently see
// the full wallet and falsely report can_start with zero shortfall.
func TestExitPlanBatchSharedSupplyDecrements(t *testing.T) {
	t.Parallel()

	const feeRate = btcutil.Amount(10)
	snapshot := unroll.ExitFundingSnapshot{
		WalletConfirmedSat: 1_000_000,
		WalletUsableInputs: 1,
	}
	feasInput := func(
		s unroll.ExitFundingSnapshot) unroll.ExitFeasibilityInput {

		return unroll.ExitFeasibilityInput{
			NumRecoveryTxs:     1,
			NumAncestryPaths:   1,
			VTXOAmountSat:      1_000_000,
			FeeRateSatPerVByte: feeRate,
			WalletConfirmedSat: s.WalletConfirmedSat,
			WalletUsableInputs: s.WalletUsableInputs,
		}
	}

	// First exit sees the single usable input and is feasible.
	first := unroll.AssessExitFeasibility(feasInput(snapshot))
	require.True(t, first.Feasible)
	require.Equal(t, 1, first.RequiredWalletInputs)

	// It claims that input, leaving the running wallet with none.
	remaining := claimExitFunding(snapshot, first)
	require.Equal(t, 0, remaining.WalletUsableInputs)

	// The second exit, assessed against the shrunken wallet, cannot fund a
	// distinct CPFP input and is infeasible.
	second := unroll.AssessExitFeasibility(feasInput(remaining))
	require.False(t, second.Feasible)
	require.Equal(t, unroll.ExitWalletTooFewInputs, second.Reason)
}
