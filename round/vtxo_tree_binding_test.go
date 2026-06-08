package round

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/lib/types"
	"github.com/stretchr/testify/require"
)

// newBoundVTXOFixture builds a single-output commitment tx and a VTXO tree
// rooted in it, exactly as the honest operator would. The returned tx and tree
// satisfy validateVTXOTreeBinding; tests corrupt one aspect at a time to prove
// the binding rejects it. The harness is returned so cases can mint extra
// valid scripts/keys.
func newBoundVTXOFixture(t *testing.T) (*wire.MsgTx, *tree.Tree,
	*boardingTestHarness) {

	t.Helper()

	h := newTestHarness(t)
	intent := h.newTestBoardingIntent()
	vtxoReq := h.newTestVTXORequestForIntent(intent)
	vtxtTree := h.newTestVTXOTreeForIntents([]types.VTXORequest{vtxoReq})

	// bindTreeToCommitment rebuilds vtxtTree in place so its root spends
	// output 0 of the commitment tx it builds.
	packet := h.bindTreeToCommitment([]BoardingIntent{intent}, vtxtTree)

	return packet.UnsignedTx, vtxtTree, h
}

// TestValidateVTXOTreeBindingAccepts confirms an honest single-output round
// (tree rooted in the commitment tx) passes the binding validation.
func TestValidateVTXOTreeBindingAccepts(t *testing.T) {
	t.Parallel()

	commitmentTx, vtxtTree, _ := newBoundVTXOFixture(t)

	err := validateVTXOTreeBinding(
		commitmentTx, map[int]*tree.Tree{
			0: vtxtTree,
		},
	)
	require.NoError(t, err)
}

// TestValidateVTXOTreeBindingRejects walks each way an operator-supplied tree
// can fail to bind to the commitment tx the client is about to sign into. Each
// case starts from an honest fixture and corrupts exactly one aspect, proving
// the corruption is what the binding catches.
func TestValidateVTXOTreeBindingRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string

		// corrupt mutates an otherwise-valid (commitmentTx, trees)
		// fixture to introduce exactly one binding defect, returning
		// the tree map to validate.
		corrupt func(*testing.T, *wire.MsgTx, *tree.Tree,
			*boardingTestHarness) map[int]*tree.Tree
	}{{
		// The root claims to spend some other transaction's output.
		name: "batch outpoint hash mismatch",
		corrupt: func(t *testing.T, _ *wire.MsgTx, vtxtTree *tree.Tree,
			_ *boardingTestHarness) map[int]*tree.Tree {

			vtxtTree.BatchOutpoint.Hash = chainhash.Hash{
				0xff,
			}

			return map[int]*tree.Tree{
				0: vtxtTree,
			}
		},
	}, {
		// The map key (the commitment output index this tree is filed
		// under) disagrees with the tree's own BatchOutpoint.Index.
		name: "map key disagrees with outpoint index",
		corrupt: func(t *testing.T, _ *wire.MsgTx, vtxtTree *tree.Tree,
			_ *boardingTestHarness) map[int]*tree.Tree {

			// Tree's BatchOutpoint.Index is 0; file it under 1.
			return map[int]*tree.Tree{
				1: vtxtTree,
			}
		},
	}, {
		// The claimed batch output index is past the end of the
		// commitment tx's outputs.
		name: "batch outpoint index out of range",
		corrupt: func(t *testing.T, commitmentTx *wire.MsgTx,
			vtxtTree *tree.Tree,
			_ *boardingTestHarness) map[int]*tree.Tree {

			idx := uint32(len(commitmentTx.TxOut) + 5)
			vtxtTree.BatchOutpoint.Index = idx

			// Keep the map key in step so the earlier key-vs-index
			// check passes and we exercise the range check.
			return map[int]*tree.Tree{
				int(idx): vtxtTree,
			}
		},
	}, {
		// The trusted BatchOutput value (the MuSig2 prevout amount)
		// does not match the real committed output.
		name: "batch output value mismatch",
		corrupt: func(t *testing.T, _ *wire.MsgTx, vtxtTree *tree.Tree,
			_ *boardingTestHarness) map[int]*tree.Tree {

			vtxtTree.BatchOutput.Value++

			return map[int]*tree.Tree{
				0: vtxtTree,
			}
		},
	}, {
		// The trusted BatchOutput script does not match the real
		// committed output script.
		name: "batch output script mismatch",
		corrupt: func(t *testing.T, _ *wire.MsgTx, vtxtTree *tree.Tree,
			h *boardingTestHarness) map[int]*tree.Tree {

			other, err := txscript.PayToTaprootScript(
				h.operatorPubKey,
			)
			require.NoError(t, err)
			vtxtTree.BatchOutput.PkScript = other

			return map[int]*tree.Tree{
				0: vtxtTree,
			}
		},
	}, {
		// The committed output and the tree's BatchOutput agree on a
		// script byte-for-byte, but that script is NOT the taproot
		// output of the tree's declared cosigner set + sweep root. The
		// value/script-equality checks pass; only the recomputation
		// check catches this substituted-but-self-consistent script.
		name: "committed script not the recomputed tree root",
		corrupt: func(t *testing.T, commitmentTx *wire.MsgTx,
			vtxtTree *tree.Tree,
			h *boardingTestHarness) map[int]*tree.Tree {

			substituted, err := txscript.PayToTaprootScript(
				h.operatorPubKey,
			)
			require.NoError(t, err)

			idx := vtxtTree.BatchOutpoint.Index
			commitmentTx.TxOut[idx].PkScript = substituted
			vtxtTree.BatchOutput.PkScript = substituted

			return map[int]*tree.Tree{
				int(idx): vtxtTree,
			}
		},
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			commitmentTx, vtxtTree, h := newBoundVTXOFixture(t)

			// Sanity check: the fixture must bind before
			// corruption, so a failure below is attributable to the
			// corruption.
			require.NoError(
				t,
				validateVTXOTreeBinding(
					commitmentTx, map[int]*tree.Tree{
						0: vtxtTree,
					},
				),
			)

			trees := tc.corrupt(t, commitmentTx, vtxtTree, h)
			require.Error(
				t, validateVTXOTreeBinding(
					commitmentTx, trees,
				),
			)
		})
	}
}

// TestCommitmentTxReceivedRejectsUnboundTree confirms the binding is wired into
// the round FSM: a commitment-tx event carrying a tree that is not rooted in
// the commitment tx fails the round into ClientFailedState before any nonce is
// generated, rather than being co-signed.
func TestCommitmentTxReceivedRejectsUnboundTree(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)

	intent := h.newTestBoardingIntent()
	vtxoReq := h.newTestVTXORequestForIntent(intent)
	intents := []BoardingIntent{intent}
	vtxos := []types.VTXORequest{vtxoReq}
	vtxtTree := h.newTestVTXOTreeForIntents(vtxos)
	commitmentTx := h.bindTreeToCommitment(intents, vtxtTree)

	// Re-root the tree at an unrelated transaction after binding: the FSM
	// must reject it rather than sign into it.
	vtxtTree.BatchOutpoint.Hash = chainhash.Hash{0xab}

	roundID := testRoundIDTr("round-unbound")
	state := &CommitmentTxReceivedState{
		RoundID:      roundID,
		CommitmentTx: commitmentTx,
		TxID:         commitmentTx.UnsignedTx.TxHash(),
		SweepDelay:   1008,
		VTXOTreePaths: map[int]*tree.Tree{
			0: vtxtTree,
		},
		Intents: Intents{
			Boarding: intents,
			VTXOs:    vtxos,
		},
		ClientTrees: make(map[SignerKey]*tree.Tree),
	}
	h.withState(state)

	event := &CommitmentTxBuilt{
		RoundID: roundID,
		Tx:      commitmentTx,
		VTXOTreePaths: map[int]*tree.Tree{
			0: vtxtTree,
		},
	}

	transition, err := h.sendEvent(event)
	require.NoError(t, err)
	require.NotNil(t, transition)

	failedState := assertStateType[*ClientFailedState](h)
	require.Contains(t, failedState.Reason, "binding")

	// The round must NOT have advanced toward signing.
	require.NotContains(t, failedState.Reason, "validation failed")
}
