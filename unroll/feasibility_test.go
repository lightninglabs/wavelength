package unroll

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/stretchr/testify/require"
)

// TestAssessExitFeasibility exercises the pure economic model across the
// feasible path and every infeasibility reason, asserting both the
// verdict and the cost breakdown it reports.
func TestAssessExitFeasibility(t *testing.T) {
	t.Parallel()

	// A healthy baseline: a 1M-sat VTXO, one shallow ancestry path, a
	// well-funded wallet, 1 sat/vB. Individual cases mutate one axis.
	baseline := func() ExitFeasibilityInput {
		return ExitFeasibilityInput{
			NumRecoveryTxs:     2,
			NumAncestryPaths:   1,
			VTXOAmountSat:      1_000_000,
			FeeRateSatPerVByte: 1,
			WalletConfirmedSat: 1_000_000,
			WalletUsableInputs: 4,
		}
	}

	tests := []struct {
		name       string
		mutate     func(*ExitFeasibilityInput)
		wantReason ExitInfeasibilityReason
		// assert lets a case make extra breakdown assertions.
		assert func(t *testing.T, f ExitFeasibility)
	}{
		{
			name:       "healthy exit is feasible",
			mutate:     func(*ExitFeasibilityInput) {},
			wantReason: ExitFeasible,
			assert: func(t *testing.T, f ExitFeasibility) {
				require.True(t, f.Feasible)

				// 2 recovery txs * 155 vB * 1 sat/vB.
				require.EqualValues(
					t, 310, f.CPFPFeeTotalSat,
				)
				// 200 vB * 1 sat/vB.
				require.EqualValues(t, 200, f.SweepFeeSat)
				require.EqualValues(
					t, 510, f.TotalRecoveryCostSat,
				)
				require.EqualValues(
					t, 999_800, f.NetRecoveredSat,
				)
			},
		},
		{
			name: "sub-dust VTXO cannot be swept",
			mutate: func(in *ExitFeasibilityInput) {
				// The #608 case: a 1-sat VTXO. After any sweep
				// fee the net is negative, well below dust.
				in.VTXOAmountSat = 1
			},
			wantReason: ExitSweepBelowDust,
			assert: func(t *testing.T, f ExitFeasibility) {
				require.False(t, f.Feasible)
				require.True(t, f.Reason.Impossible())
			},
		},
		{
			name: "net one sat below dust is infeasible",
			mutate: func(in *ExitFeasibilityInput) {
				// sweepFee=200; net = dust-1 must fail.
				// Keep recovery trivial so it's the only
				// failure.
				in.NumRecoveryTxs = 0
				in.NumAncestryPaths = 0
				in.FeeRateSatPerVByte = 1
				in.VTXOAmountSat = 200 +
					defaultSweepOutputDustSat - 1
			},
			wantReason: ExitSweepBelowDust,
		},
		{
			name: "net exactly at dust clears the floor",
			mutate: func(in *ExitFeasibilityInput) {
				// net = vtxo-200 = dust. An output at the
				// dust limit is relayable, so feasible.
				// Keep recovery trivial.
				in.NumRecoveryTxs = 0
				in.NumAncestryPaths = 0
				in.VTXOAmountSat = 200 +
					defaultSweepOutputDustSat
			},
			wantReason: ExitFeasible,
		},
		{
			name: "loss-making exit is uneconomical",
			mutate: func(in *ExitFeasibilityInput) {
				// Deep lineage on a small (but above-dust-net)
				// VTXO: CPFP fees dwarf the coin's value at a
				// high fee rate.
				in.NumRecoveryTxs = 50
				in.FeeRateSatPerVByte = 50
				in.VTXOAmountSat = 20_000
			},
			wantReason: ExitUneconomical,
			assert: func(t *testing.T, f ExitFeasibility) {
				require.False(t, f.Reason.Impossible())
				require.GreaterOrEqual(
					t, int64(f.TotalRecoveryCostSat),
					int64(f.VTXOAmountSat),
				)
			},
		},
		{
			name: "wallet cannot fund CPFP fees",
			mutate: func(in *ExitFeasibilityInput) {
				// Plenty of value, but the on-chain wallet is
				// nearly empty so the CPFP children can't be
				// funded.
				in.NumRecoveryTxs = 4
				in.FeeRateSatPerVByte = 10
				in.WalletConfirmedSat = 100
			},
			wantReason: ExitWalletUnderfunded,
		},
		{
			name: "wallet has balance but too few inputs",
			mutate: func(in *ExitFeasibilityInput) {
				// Two independent ancestry paths need two
				// distinct fee inputs; the wallet has balance
				// in only one usable UTXO.
				in.NumAncestryPaths = 2
				in.WalletUsableInputs = 1
			},
			wantReason: ExitWalletTooFewInputs,
		},
		{
			name: "dust check precedes wallet checks",
			mutate: func(in *ExitFeasibilityInput) {
				// Both sub-dust and underfunded: the impossible
				// reason must win so the user isn't told to
				// fund a wallet for an exit that can't work.
				in.VTXOAmountSat = 1
				in.WalletConfirmedSat = 0
			},
			wantReason: ExitSweepBelowDust,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			in := baseline()
			tc.mutate(&in)

			f := AssessExitFeasibility(in)

			require.Equal(
				t, tc.wantReason, f.Reason, "reason %s",
				f.Reason,
			)
			require.Equal(
				t, tc.wantReason == ExitFeasible, f.Feasible,
			)

			if tc.assert != nil {
				tc.assert(t, f)
			}
		})
	}
}

// TestExitFeasibilityConfigDefaults verifies that the zero-value config
// resolves to the documented defaults and that explicit overrides win.
func TestExitFeasibilityConfigDefaults(t *testing.T) {
	t.Parallel()

	got := ExitFeasibilityConfig{}.withDefaults()
	require.Equal(t, defaultCPFPChildVBytes, got.CPFPChildVBytes)
	require.EqualValues(t, estimatedSweepVBytes, got.SweepVBytes)
	require.Equal(t, defaultSweepOutputDustSat, got.SweepOutputDustSat)
	require.Equal(
		t, defaultMaxRecoveryCostFractionBP,
		got.MaxRecoveryCostFractionBP,
	)

	override := ExitFeasibilityConfig{
		CPFPChildVBytes:           200,
		SweepVBytes:               300,
		SweepOutputDustSat:        546,
		MaxRecoveryCostFractionBP: 5_000,
	}.withDefaults()
	require.Equal(t, int64(200), override.CPFPChildVBytes)
	require.Equal(t, int64(300), override.SweepVBytes)
	require.Equal(t, btcutil.Amount(546), override.SweepOutputDustSat)
	require.Equal(t, int64(5_000), override.MaxRecoveryCostFractionBP)
}

// TestExitFeasibilityStricterFraction shows a lower cost-fraction policy
// blocks an exit the default 100% threshold would allow.
func TestExitFeasibilityStricterFraction(t *testing.T) {
	t.Parallel()

	in := ExitFeasibilityInput{
		NumRecoveryTxs:     10,
		NumAncestryPaths:   1,
		VTXOAmountSat:      100_000,
		FeeRateSatPerVByte: 10,
		WalletConfirmedSat: 1_000_000,
		WalletUsableInputs: 4,
	}

	// Total cost = 10*155*10 + 200*10 = 15_500 + 2_000 = 17_500 sat,
	// which is 17.5% of the 100k VTXO: feasible under the 100% default.
	require.True(t, AssessExitFeasibility(in).Feasible)

	// Under a 10% (1_000 bp) policy the same exit is uneconomical.
	in.Config.MaxRecoveryCostFractionBP = 1_000
	f := AssessExitFeasibility(in)
	require.False(t, f.Feasible)
	require.Equal(t, ExitUneconomical, f.Reason)
}

// TestRecoveryTxCount checks the descriptor-derived recovery-transaction
// and ancestry-path counts, including the OOR ChainDepth contribution and
// the pruned-tree fallback.
func TestRecoveryTxCount(t *testing.T) {
	t.Parallel()

	t.Run("nil descriptor", func(t *testing.T) {
		t.Parallel()

		numTxs, numPaths := RecoveryTxCount(nil)
		require.Zero(t, numTxs)
		require.Zero(t, numPaths)
	})

	t.Run("pruned tree uses depth fallback plus chain depth", func(
		t *testing.T) {

		t.Parallel()

		desc := &vtxo.Descriptor{
			ChainDepth: 2,
			Ancestry: []types.Ancestry{
				{
					CommitmentTxID: chainhash.Hash{
						0x01,
					},
					TreePath:  nil,
					TreeDepth: 3,
				},
				{
					CommitmentTxID: chainhash.Hash{
						0x02,
					},
					TreePath:  nil,
					TreeDepth: 1,
				},
			},
		}

		numTxs, numPaths := RecoveryTxCount(desc)
		require.Equal(t, 2, numPaths)
		// 3 + 1 (depth fallback) + 2 (chain depth).
		require.Equal(t, 6, numTxs)
	})

	t.Run("tree path contributes its NumTx", func(t *testing.T) {
		t.Parallel()

		// A root with two leaf children is three transactions.
		treePath := &tree.Tree{
			Root: &tree.Node{
				Children: map[uint32]*tree.Node{
					0: {},
					1: {},
				},
			},
		}
		require.Equal(t, 3, treePath.NumTx())

		desc := &vtxo.Descriptor{
			Ancestry: []types.Ancestry{
				{
					CommitmentTxID: chainhash.Hash{
						0x01,
					},
					TreePath: treePath,
				},
			},
		}

		numTxs, numPaths := RecoveryTxCount(desc)
		require.Equal(t, 1, numPaths)
		require.Equal(t, 3, numTxs)
	})

	t.Run("malformed path floors at one tx", func(t *testing.T) {
		t.Parallel()

		// An ancestry entry with neither a tree path nor a depth
		// must still contribute at least one transaction so the
		// CPFP-fee estimate is never zeroed out for a real path.
		desc := &vtxo.Descriptor{
			Ancestry: []types.Ancestry{
				{
					CommitmentTxID: chainhash.Hash{
						0x01,
					},
					TreePath:  nil,
					TreeDepth: 0,
				},
			},
		}

		numTxs, numPaths := RecoveryTxCount(desc)
		require.Equal(t, 1, numPaths)
		require.Equal(t, 1, numTxs)
	})
}
