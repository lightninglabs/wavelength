package batchsweeper

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo/batchwatcher"
	"pgregory.net/rapid"
)

// TestSelectSweepCandidatesInvariants verifies that candidate selection
// enforces the core filtering invariants across random inputs.
func TestSelectSweepCandidatesInvariants(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(t *rapid.T) {
		bestHeight := rapid.Uint32().Draw(t, "bestHeight")
		sweepDelay := rapid.Uint32().Draw(t, "sweepDelay")

		numOutputs := rapid.IntRange(0, 50).Draw(t, "numOutputs")
		outputs := make(
			map[wire.OutPoint]*batchwatcher.Output, numOutputs,
		)

		for i := 0; i < numOutputs; i++ {
			var txid chainhash.Hash
			txidBytes := rapid.SliceOfN(
				rapid.Byte(), 32, 32,
			).Draw(t, "txid")
			copy(txid[:], txidBytes)

			outpoint := wire.OutPoint{
				Hash:  txid,
				Index: rapid.Uint32().Draw(t, "idx"),
			}

			isVTXO := rapid.Bool().Draw(t, "isVTXO")
			confirmedHeight := rapid.Uint32().Draw(
				t, "confirmedHeight",
			)

			// Make TreeNode present only for some outputs.
			// The selection logic should ignore outputs without a
			// node.
			var node *tree.Node
			if rapid.Bool().Draw(t, "hasNode") {
				node = &tree.Node{}
			}

			outputs[outpoint] = &batchwatcher.Output{
				Outpoint:        outpoint,
				TxOut:           wire.NewTxOut(1, []byte{0x51}),
				ConfirmedHeight: confirmedHeight,
				IsVTXO:          isVTXO,
				TreeNode:        node,
			}
		}

		state := &batchwatcher.BatchTreeState{
			ExistingOutputs: outputs,
		}

		candidates := selectSweepCandidates(
			state, bestHeight, sweepDelay,
		)

		// INVARIANT: all candidates are from the input set.
		for _, c := range candidates {
			if c == nil {
				t.Fatalf("nil candidate")
			}

			if outputs[c.Outpoint] != c {
				t.Fatalf("candidate not from input set")
			}
		}

		// INVARIANT: candidates are operator-controlled and mature.
		for _, c := range candidates {
			if c.IsVTXO {
				t.Fatalf("candidate is VTXO")
			}
			if c.TreeNode == nil {
				t.Fatalf("candidate missing tree node")
			}

			maturityHeight, overflow := addUint32(
				c.ConfirmedHeight, sweepDelay,
			)
			if overflow {
				t.Fatalf("candidate maturity overflowed")
			}

			if maturityHeight > bestHeight {
				t.Fatalf("candidate is not mature")
			}
		}

		// INVARIANT: candidates are deterministically sorted.
		for i := 1; i < len(candidates); i++ {
			if outpointLess(
				candidates[i].Outpoint,
				candidates[i-1].Outpoint,
			) {

				t.Fatalf("candidates not sorted")
			}
		}
	})
}
