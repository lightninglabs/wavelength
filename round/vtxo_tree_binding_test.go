package round

import (
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/lib/types"
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

// TestConfirmationWatchScriptUsesBatchOutput builds a multi-output round whose
// batch output is NOT at index 0 and confirms two things: the tree still binds
// to the commitment tx, and the confirmation watch script tracks the validated
// batch output rather than blindly watching output 0.
func TestConfirmationWatchScriptUsesBatchOutput(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	intent := h.newTestBoardingIntent()
	vtxoReq := h.newTestVTXORequestForIntent(intent)
	vtxtTree := h.newTestVTXOTreeForIntents([]types.VTXORequest{vtxoReq})

	// Canonical batch output: the taproot script of the tree's cosigner
	// set tweaked by the sweep root, valued at the summed leaf amount.
	finalKey, err := tree.ComputeFinalKey(
		vtxtTree.Root.CoSigners, vtxtTree.SweepTapscriptRoot,
	)
	require.NoError(t, err)
	batchScript, err := txscript.PayToTaprootScript(finalKey)
	require.NoError(t, err)

	// A distinct, non-batch output to sit at index 0 (e.g. a leave
	// output), so the batch output lands at index 1.
	fillerScript, err := txscript.PayToTaprootScript(h.operatorPubKey)
	require.NoError(t, err)
	require.NotEqual(t, fillerScript, batchScript)

	const batchIdx = 1
	tx := wire.NewMsgTx(3)
	tx.AddTxIn(&wire.TxIn{PreviousOutPoint: intent.Outpoint})
	tx.AddTxOut(&wire.TxOut{Value: 12345, PkScript: fillerScript})
	tx.AddTxOut(&wire.TxOut{
		Value:    vtxtTree.BatchOutput.Value,
		PkScript: batchScript,
	})
	tx.AddTxOut(&wire.TxOut{
		Value:    0,
		PkScript: []byte{txscript.OP_TRUE, txscript.OP_RETURN},
	})

	// Rebuild the tree rooted at the real batch output (index 1).
	txid := tx.TxHash()
	rebuilt, err := tree.NewTree(
		wire.OutPoint{
			Hash:  txid,
			Index: batchIdx,
		},
		tx.TxOut[batchIdx],
		h.leafDescriptorsFromTree(vtxtTree),
		h.operatorPubKey,
		vtxtTree.SweepTapscriptRoot,
		2,
	)
	require.NoError(t, err)

	trees := map[int]*tree.Tree{batchIdx: rebuilt}

	// The non-index-0 tree must still bind.
	require.NoError(t, validateVTXOTreeBinding(tx, trees))

	// The watch script must be the batch output, not output 0.
	watch := confirmationWatchScript(tx, trees)
	require.Equal(t, batchScript, watch)
	require.NotEqual(t, fillerScript, watch)
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
