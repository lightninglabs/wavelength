package batchsweeper

import (
	"bytes"
	"fmt"
	"sort"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo/batchwatcher"
)

// selectSweepCandidates filters the current BatchWatcher tree state into the
// set of operator-controlled outputs that are CSV-mature at the provided best
// height. The returned slice is sorted deterministically by outpoint.
func selectSweepCandidates(state *batchwatcher.BatchTreeState, bestHeight,
	sweepDelay uint32) []*batchwatcher.Output {

	if state == nil {
		return nil
	}

	candidates := make(
		[]*batchwatcher.Output, 0, len(state.ExistingOutputs),
	)
	for _, output := range state.ExistingOutputs {
		if output == nil {
			continue
		}

		// VTXO leaves are client-owned and must not be swept by the
		// operator.
		if output.IsVTXO {
			continue
		}

		// We require the tree node to be present to derive the internal
		// key for the operator sweep control block.
		if output.TreeNode == nil {
			continue
		}

		maturityHeight, overflow := addUint32(
			output.ConfirmedHeight, sweepDelay,
		)
		if overflow {
			continue
		}

		if maturityHeight > bestHeight {
			continue
		}

		candidates = append(candidates, output)
	}

	sort.Slice(candidates, func(i, j int) bool {
		return outpointLess(
			candidates[i].Outpoint, candidates[j].Outpoint,
		)
	})

	return candidates
}

// addUint32 adds two uint32 values and reports whether the sum overflowed.
func addUint32(a, b uint32) (uint32, bool) {
	sum := a + b
	if sum < a {
		return 0, true
	}

	return sum, false
}

// outpointLess provides a deterministic ordering for outpoints based on txid
// bytes, then output index.
func outpointLess(a, b wire.OutPoint) bool {
	hashCmp := bytes.Compare(a.Hash[:], b.Hash[:])
	if hashCmp != 0 {
		return hashCmp < 0
	}

	return a.Index < b.Index
}

// nextSweepMaturityHeight returns the smallest maturity height among
// operator-controlled unspent outputs that are not yet mature at bestHeight.
// The second return value is false if no such outputs exist.
func nextSweepMaturityHeight(state *batchwatcher.BatchTreeState, bestHeight,
	sweepDelay uint32) (uint32, bool, error) {

	if state == nil {
		return 0, false, nil
	}

	var (
		found     bool
		minMature uint32
	)

	for _, output := range state.ExistingOutputs {
		if output == nil {
			continue
		}

		if output.IsVTXO {
			continue
		}

		if output.TreeNode == nil {
			return 0, false, fmt.Errorf(
				"missing tree node for outpoint %v",
				output.Outpoint,
			)
		}

		maturityHeight, overflow := addUint32(
			output.ConfirmedHeight, sweepDelay,
		)
		if overflow {
			continue
		}

		if maturityHeight <= bestHeight {
			continue
		}

		if !found || maturityHeight < minMature {
			found = true
			minMature = maturityHeight
		}
	}

	return minMature, found, nil
}
