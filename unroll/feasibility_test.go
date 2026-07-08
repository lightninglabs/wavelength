package unroll

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/lib/recovery"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/lightninglabs/darepo-client/txconfirm"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/stretchr/testify/require"
)

// p2trTestOut returns a wire output with a P2TR-sized (34-byte) pkScript, for
// sizing recovery-tx estimates in tests.
func p2trTestOut() *wire.TxOut {
	return wire.NewTxOut(1000, make([]byte, 34))
}

// anchorTestOut returns a wire output with a P2A-sized (4-byte) pkScript.
func anchorTestOut() *wire.TxOut {
	return wire.NewTxOut(0, make([]byte, 4))
}

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

// TestRecoveryTxVBytes verifies the parent-weight estimate that the exit
// funding model now folds into the CPFP budget: the fallback per-tx constant
// for pruned fragments and OOR checkpoints, and the exact per-node sum for a
// real extracted tree path (including its radix sensitivity).
func TestRecoveryTxVBytes(t *testing.T) {
	t.Parallel()

	// A nil descriptor costs nothing.
	require.Zero(t, RecoveryTxVBytes(nil))

	// A pruned fragment (depth persisted, no path) falls back to the
	// per-tx constant, once per level.
	pruned := &vtxo.Descriptor{
		Ancestry: []types.Ancestry{
			{
				TreeDepth: 3,
			},
		},
	}
	require.Equal(
		t, 3*defaultRecoveryTxVBytes, RecoveryTxVBytes(pruned),
	)

	// A fragment with neither path nor depth still floors at one tx, so
	// a malformed fragment never silently costs nothing.
	bare := &vtxo.Descriptor{
		Ancestry: []types.Ancestry{
			{
				CommitmentTxID: chainhash.Hash{
					1,
				},
			},
		},
	}
	require.Equal(t, defaultRecoveryTxVBytes, RecoveryTxVBytes(bare))

	// Each OOR checkpoint hop is one more fallback-sized recovery tx.
	chain := &vtxo.Descriptor{ChainDepth: 2}
	require.Equal(
		t, 2*defaultRecoveryTxVBytes, RecoveryTxVBytes(chain),
	)

	// A real extracted path sums the actual per-node tx sizes: a root
	// branch funding three children plus a leaf, each carrying an anchor.
	leaf := &tree.Node{
		Outputs: []*wire.TxOut{
			p2trTestOut(),
			anchorTestOut(),
		},
	}
	root := &tree.Node{
		Outputs: []*wire.TxOut{
			p2trTestOut(), p2trTestOut(), p2trTestOut(),
			anchorTestOut(),
		},
		Children: map[uint32]*tree.Node{
			0: leaf,
		},
	}
	realPath := &vtxo.Descriptor{
		Ancestry: []types.Ancestry{
			{
				TreePath: &tree.Tree{
					Root: root,
				},
			},
		},
	}
	want := nodeTxVBytes(root) + nodeTxVBytes(leaf)
	require.Equal(t, want, RecoveryTxVBytes(realPath))
	require.Greater(t, want, int64(0))

	// Radix sensitivity: a wider branch (more child outputs) is a larger
	// recovery tx, so it must cost strictly more than a narrow one.
	narrow := &tree.Node{
		Outputs: []*wire.TxOut{
			p2trTestOut(), p2trTestOut(), anchorTestOut(),
		},
	}
	wide := &tree.Node{Outputs: []*wire.TxOut{anchorTestOut()}}
	for i := 0; i < 8; i++ {
		wide.Outputs = append(wide.Outputs, p2trTestOut())
	}
	require.Greater(t, nodeTxVBytes(wide), nodeTxVBytes(narrow))
}

// testRecoveryTx builds a finalized-shape recovery transaction with the given
// number of P2TR outputs plus an anchor, for sizing OOR extra-node estimates.
func testRecoveryTx(numOut int) *wire.MsgTx {
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{})
	for i := 0; i < numOut; i++ {
		tx.AddTxOut(p2trTestOut())
	}
	tx.AddTxOut(anchorTestOut())

	return tx
}

// TestRecoveryEstimateMaterialSizesOOR verifies that when resolved lineage
// material is supplied, the OOR chain is counted and sized from the actual
// checkpoint/ark transactions rather than the ChainDepth scalar — so a single
// hop that carries multiple checkpoint PSBTs is no longer undercounted.
func TestRecoveryEstimateMaterialSizesOOR(t *testing.T) {
	t.Parallel()

	// A two-hop OOR VTXO whose descriptor only records ChainDepth == 2.
	desc := &vtxo.Descriptor{
		Outpoint: wire.OutPoint{
			Index: 1,
		},
		ChainDepth: 2,
	}

	// Descriptor-only: two hops at the fallback size, and two recovery txs.
	numTxs, _, vbytes := recoveryEstimate(desc, nil)
	require.Equal(t, 2, numTxs)
	require.Equal(t, 2*defaultRecoveryTxVBytes, vbytes)

	// The real hops: the first hop spent a multi-input source, so it
	// carries two checkpoint PSBTs plus its ark tx; the second hop a single
	// checkpoint plus its ark tx. That is four checkpoints/arks, not two.
	mat := &LineageMaterial{
		TargetOutpoint: desc.Outpoint,
		ExtraNodes: []*recovery.Node{
			{
				Kind: recovery.NodeKindCheckpoint,
				Tx:   testRecoveryTx(1),
			},
			{
				Kind: recovery.NodeKindCheckpoint,
				Tx:   testRecoveryTx(1),
			},
			{
				Kind: recovery.NodeKindArk,
				Tx:   testRecoveryTx(1),
			},
			{
				Kind: recovery.NodeKindArk,
				Tx:   testRecoveryTx(2),
			},
		},
	}

	var wantVBytes int64
	for _, n := range mat.ExtraNodes {
		wantVBytes += extraNodeVBytes(n)
	}

	matTxs, _, matVBytes := recoveryEstimate(desc, mat)

	// The material path counts every checkpoint/ark tx and sizes each
	// exactly, so it exceeds the ChainDepth approximation on both axes.
	require.Equal(t, len(mat.ExtraNodes), matTxs)
	require.Greater(t, matTxs, numTxs)
	require.Equal(t, wantVBytes, matVBytes)
	require.Greater(t, matVBytes, vbytes)

	// extraNodeVBytes measures a real tx, and a nil node or tx falls back
	// to the conservative default rather than costing nothing.
	require.Greater(t, extraNodeVBytes(mat.ExtraNodes[0]), int64(0))
	require.Equal(t, defaultRecoveryTxVBytes, extraNodeVBytes(nil))
	require.Equal(
		t, defaultRecoveryTxVBytes,
		extraNodeVBytes(
			&recovery.Node{},
		),
	)
}

// TestExtraNodeVBytesMatchesBroadcaster locks the OOR parent-sizing to the
// exact primitive the txconfirm broadcaster uses to compute the ancestor
// package fee. computePackageFee sizes a parent as (EstimateWeight(tx)+3)/4;
// if extraNodeVBytes ever drifts from that, the up-front funding
// recommendation would stop matching what the broadcaster actually pays, so
// this test fails the moment either side changes.
func TestExtraNodeVBytesMatchesBroadcaster(t *testing.T) {
	t.Parallel()

	for _, numOut := range []int{1, 2, 4, 8} {
		tx := testRecoveryTx(numOut)
		node := &recovery.Node{
			Kind: recovery.NodeKindCheckpoint,
			Tx:   tx,
		}

		want := (txconfirm.EstimateWeight(tx) + 3) / 4
		require.Equal(t, want, extraNodeVBytes(node))
	}
}

// TestRecoveryEstimateNilRootFallsThrough verifies that a descriptor whose
// TreePath is non-nil but carries a nil Root degrades to the malformed-tx
// floor instead of panicking in Tree.NumTx, preserving the "estimate always
// degrades, never blocks the exit" contract.
func TestRecoveryEstimateNilRootFallsThrough(t *testing.T) {
	t.Parallel()

	desc := &vtxo.Descriptor{
		Ancestry: []types.Ancestry{
			{
				TreePath: &tree.Tree{
					Root: nil,
				},
			},
		},
	}

	var (
		numTxs   int
		numPaths int
		vbytes   int64
	)
	require.NotPanics(t, func() {
		numTxs, numPaths, vbytes = recoveryEstimate(desc, nil)
	})

	// The fragment still counts as one recovery tx and one path, sized at
	// the fallback — count and vBytes stay in lockstep.
	require.Equal(t, 1, numTxs)
	require.Equal(t, 1, numPaths)
	require.Equal(t, defaultRecoveryTxVBytes, vbytes)
}

// TestRecoveryEstimateMixedTreeAndOOR covers the realistic OOR-on-round case:
// a descriptor with a real commitment-tree ancestry path plus resolved OOR
// checkpoint/ark material. Both contributions must be summed, on both the
// count and the vBytes axes.
func TestRecoveryEstimateMixedTreeAndOOR(t *testing.T) {
	t.Parallel()

	leaf := &tree.Node{
		Outputs: []*wire.TxOut{
			p2trTestOut(),
			anchorTestOut(),
		},
	}
	root := &tree.Node{
		Outputs: []*wire.TxOut{
			p2trTestOut(), p2trTestOut(), anchorTestOut(),
		},
		Children: map[uint32]*tree.Node{
			0: leaf,
		},
	}
	treePath := &tree.Tree{Root: root}

	desc := &vtxo.Descriptor{
		Outpoint: wire.OutPoint{
			Index: 1,
		},
		ChainDepth: 1,
		Ancestry: []types.Ancestry{
			{
				TreePath: treePath,
			},
		},
	}
	mat := &LineageMaterial{
		TargetOutpoint: desc.Outpoint,
		TreePaths: []*tree.Tree{
			treePath,
		},
		ExtraNodes: []*recovery.Node{
			{
				Kind: recovery.NodeKindCheckpoint,
				Tx:   testRecoveryTx(1),
			},
			{
				Kind: recovery.NodeKindArk,
				Tx:   testRecoveryTx(1),
			},
		},
	}

	numTxs, numPaths, vbytes := recoveryEstimate(desc, mat)

	// The tree path contributes its node txs; the OOR material adds its
	// two extra nodes. numPaths counts only the ancestry fragment.
	wantTreeVBytes := treePathVBytes(treePath)
	var wantOORVBytes int64
	for _, n := range mat.ExtraNodes {
		wantOORVBytes += extraNodeVBytes(n)
	}

	require.Equal(t, treePath.NumTx()+len(mat.ExtraNodes), numTxs)
	require.Equal(t, 1, numPaths)
	require.Equal(t, wantTreeVBytes+wantOORVBytes, vbytes)
}
