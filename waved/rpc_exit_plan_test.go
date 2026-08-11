package waved

import (
	"testing"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/lightninglabs/wavelength/db"
	"github.com/lightninglabs/wavelength/unroll"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// exitFundingShortfallForTest recomputes the funding shortfall from the
// exported feasibility verdict fields, mirroring the unexported
// unroll.exitFundingShortfall so the waved entry mapping can be asserted
// without a live wallet/DB. For a wallet that covers both the required
// distinct fee inputs and the CPFP balance the result is zero.
func exitFundingShortfallForTest(v unroll.ExitFeasibility) int64 {
	recommended := unroll.RecommendedExitFeeInputAmount(v)

	missingInputs := 0
	if v.WalletUsableInputs < v.RequiredWalletInputs {
		missingInputs = v.RequiredWalletInputs - v.WalletUsableInputs
	}
	inputShortfall := int64(recommended) * int64(missingInputs)

	var balanceShortfall int64
	if v.WalletConfirmedSat < v.CPFPFeeTotalSat {
		balanceShortfall = int64(
			v.CPFPFeeTotalSat - v.WalletConfirmedSat,
		)
	}

	return max(inputShortfall, balanceShortfall)
}

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

// TestExitPlanEntrySurfacesStructuralInfeasibility locks the #894 fix: when a
// VTXO fails the dust or uneconomical gate against a well-funded wallet, the
// entry must report can_start=false with a ZERO funding shortfall (no amount
// of wallet funding fixes it) AND carry the structural reason on
// InfeasibilityReason, rather than leaving the caller with a silent
// can_start=false and an empty error. It mirrors the exact verdict->entry
// mapping exitPlanEntry performs.
func TestExitPlanEntrySurfacesStructuralInfeasibility(t *testing.T) {
	t.Parallel()

	// A well-funded wallet so the block can never be a funding gap: any
	// infeasibility must be structural (dust or uneconomical).
	const (
		feeRate         = btcutil.Amount(50)
		walletConfirmed = btcutil.Amount(10_000_000)
		walletInputs    = 10
	)

	tests := []struct {
		name       string
		in         unroll.ExitFeasibilityInput
		wantReason unroll.ExitInfeasibilityReason
	}{
		{
			// A 1-sat VTXO: after any sweep fee the swept output is
			// far below dust, so the sweep can never relay.
			name: "sub-dust vtxo",
			in: unroll.ExitFeasibilityInput{
				NumRecoveryTxs:     1,
				NumAncestryPaths:   1,
				VTXOAmountSat:      1,
				FeeRateSatPerVByte: feeRate,
				WalletConfirmedSat: walletConfirmed,
				WalletUsableInputs: walletInputs,
			},
			wantReason: unroll.ExitSweepBelowDust,
		},
		{
			// A deep lineage on a small (but above-dust-net) VTXO
			// at a high fee rate: CPFP fees dwarf the coin's value.
			name: "uneconomical vtxo",
			in: unroll.ExitFeasibilityInput{
				NumRecoveryTxs:     50,
				NumAncestryPaths:   1,
				VTXOAmountSat:      20_000,
				FeeRateSatPerVByte: feeRate,
				WalletConfirmedSat: walletConfirmed,
				WalletUsableInputs: walletInputs,
			},
			wantReason: unroll.ExitUneconomical,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			verdict := unroll.AssessExitFeasibility(tc.in)

			// The well-funded wallet covers both the required
			// distinct fee inputs and the CPFP balance, so the
			// funding shortfall is zero: the block is purely
			// structural (dust/uneconomical), not a funding gap.
			require.GreaterOrEqual(
				t, verdict.WalletUsableInputs,
				verdict.RequiredWalletInputs,
			)
			require.GreaterOrEqual(
				t, int64(verdict.WalletConfirmedSat),
				int64(verdict.CPFPFeeTotalSat),
			)
			shortfall := exitFundingShortfallForTest(verdict)

			// Mirror the exact verdict->entry mapping exitPlanEntry
			// performs.
			entry := ExitPlanEntry{
				CanStart:            verdict.Feasible,
				InfeasibilityReason: verdict.Reason,
				FundingShortfallSat: shortfall,
			}

			require.False(t, entry.CanStart)
			require.Zero(t, entry.FundingShortfallSat)
			require.Equal(
				t, tc.wantReason, entry.InfeasibilityReason,
			)
		})
	}
}

// exitPlanCommittedFixture persists a VTXO in the given lifecycle status with
// a one-fragment ancestry, which is the minimum that makes the funding
// estimate price the exit at all. Without it recoveryEstimate returns zero
// required inputs and exitPlanEntry short-circuits on "no unilateral-exit
// ancestry" before the commitment advisory is ever reached.
func exitPlanCommittedFixture(t *testing.T, store *db.VTXOPersistenceStore,
	hashByte byte, vtxoStatus vtxo.VTXOStatus) *vtxo.Descriptor {

	t.Helper()

	// A single-fragment ancestry: enough for the estimate to require
	// real CPFP funding, shallow enough to stay economical against a
	// 50k VTXO. The tree path must be non-nil because the ancestry row
	// stores it NOT NULL.
	desc := newRefreshEstimateVTXO(t, hashByte, 50_000, 900)
	desc.Ancestry = []vtxo.Ancestry{
		recoveryTestFragment(desc.CommitmentTxID, 0),
	}
	require.NoError(t, store.SaveVTXO(t.Context(), desc))

	// SaveVTXO inserts at a fixed status and ignores desc.Status, so the
	// lifecycle state under test has to be applied as an update.
	desc.Status = vtxoStatus
	require.NoError(
		t,
		store.UpdateVTXOStatus(
			t.Context(), desc.Outpoint, vtxoStatus,
		),
	)

	return desc
}

// TestExitPlanEntryWarnsCommittedVTXO asserts the exit preview reports a VTXO
// committed to a cooperative round as an advisory alongside a full pricing,
// rather than failing the entry.
//
// Previewing it as unconditionally ready is what escalated wavelength#577
// into a near miss: a user whose cooperative leave was already in flight was
// told can_start=true, shortfall=0, and reached for --force-unroll-ack. But
// refusing the entry outright is wrong in the other direction, because Unroll
// has no commitment check and performs that exit — and it is the only lever
// that recovers the coin when the operator is unreachable and the commitment
// never confirms. An error would tell exactly that user their recovery is
// impossible while withholding the funding figures it needs.
func TestExitPlanEntryWarnsCommittedVTXO(t *testing.T) {
	t.Parallel()

	for _, committed := range []vtxo.VTXOStatus{
		vtxo.VTXOStatusPendingForfeit, vtxo.VTXOStatusForfeiting,
	} {
		t.Run(committed.String(), func(t *testing.T) {
			t.Parallel()

			r, store := newLeaveAdmissionServer(t)
			desc := exitPlanCommittedFixture(
				t, store, 0x61, committed,
			)

			// A wallet that comfortably covers the CPFP funding,
			// so the only thing lowering the verdict is the round
			// commitment itself.
			entry, verdict := r.exitPlanEntry(
				t.Context(), desc.Outpoint.String(), 1,
				unroll.ExitFundingSnapshot{
					WalletConfirmedSat: 1_000_000,
					WalletUsableInputs: 4,
				},
			)

			// The entry is answered, not failed: Err means the
			// preview could not price the coin at all.
			require.NoError(t, entry.Err)

			// The warning survives, and names what holds the coin.
			require.Error(t, entry.RoundCommitment)
			require.ErrorIs(
				t, entry.RoundCommitment,
				vtxo.ErrForfeitInFlight,
			)
			require.Contains(
				t, entry.RoundCommitment.Error(),
				desc.Outpoint.String(),
			)

			// The verdict is lowered so no caller reads the coin
			// as ready, and the reason says why.
			require.False(t, entry.CanStart)
			require.Equal(
				t, unroll.ExitRoundCommitted,
				entry.InfeasibilityReason,
			)

			// The funding figures the recovery path needs are
			// still there. The underlying funding assessment is
			// untouched, which is what keeps the preview
			// consistent with what Unroll will do.
			require.True(t, verdict.Feasible)
			require.Positive(t, entry.RequiredFeeUTXOCount)
			require.Positive(t, entry.RecommendedTotalFundingSat)
			require.Positive(t, entry.RecommendedUTXOAmountSat)
		})
	}
}

// TestExitPlanEntryUncommittedKeepsFeasibleVerdict is the control for the
// test above: the same fixture in a live state must still preview as ready,
// so the lowered verdict is attributable to the commitment and nothing else.
func TestExitPlanEntryUncommittedKeepsFeasibleVerdict(t *testing.T) {
	t.Parallel()

	r, store := newLeaveAdmissionServer(t)
	desc := exitPlanCommittedFixture(
		t, store, 0x62, vtxo.VTXOStatusLive,
	)

	entry, _ := r.exitPlanEntry(
		t.Context(), desc.Outpoint.String(), 1,
		unroll.ExitFundingSnapshot{
			WalletConfirmedSat: 1_000_000,
			WalletUsableInputs: 4,
		},
	)

	require.NoError(t, entry.Err)
	require.NoError(t, entry.RoundCommitment)
	require.True(t, entry.CanStart)
	require.Equal(t, unroll.ExitFeasible, entry.InfeasibilityReason)
}

// TestExitPlanRoundCommitmentScope asserts the preview only blocks on a round
// commitment. A VTXO already exiting must still get a real preview, since its
// exit job status is the answer the caller wants.
func TestExitPlanRoundCommitmentScope(t *testing.T) {
	t.Parallel()

	tests := []struct {
		status  vtxo.VTXOStatus
		blocked bool
	}{
		{
			vtxo.VTXOStatusLive,
			false,
		},
		{
			vtxo.VTXOStatusExpired,
			false,
		},
		{
			vtxo.VTXOStatusPendingForfeit,
			true,
		},
		{
			vtxo.VTXOStatusForfeiting,
			true,
		},
		{
			vtxo.VTXOStatusUnilateralExit,
			false,
		},
		{
			vtxo.VTXOStatusSpending,
			false,
		},
		{
			vtxo.VTXOStatusForfeited,
			false,
		},
	}

	for _, test := range tests {
		t.Run(test.status.String(), func(t *testing.T) {
			t.Parallel()

			err := exitPlanRoundCommitment(&vtxo.Descriptor{
				Status: test.status,
			})

			if !test.blocked {
				require.NoError(t, err)

				return
			}

			require.ErrorIs(t, err, vtxo.ErrForfeitInFlight)
		})
	}
}
