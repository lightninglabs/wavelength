// Package fraud detects recipient-side OOR ancestry materialization.
package fraud

import (
	"bytes"
	"fmt"
	"sort"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/vtxo"
)

var (
	// ErrWatchUnavailable indicates local state is insufficient to arm
	// recipient fraud watches.
	ErrWatchUnavailable = fmt.Errorf("recipient fraud watch unavailable")

	// ErrWatchInvalid indicates persisted ancestry state is internally
	// inconsistent.
	ErrWatchInvalid = fmt.Errorf("recipient fraud watch invalid")
)

// WatchPlan is the passive ancestry watch set for one locally-owned OOR VTXO.
type WatchPlan struct {
	// TargetOutpoint is the VTXO to unroll if any watched ancestor spends.
	TargetOutpoint wire.OutPoint

	// BatchExpiry is the absolute height at which the target stops being
	// live and becomes eligible for operator-sweep reissue.
	BatchExpiry int32

	// Watches are the ancestor outpoints that indicate materialization has
	// started.
	Watches []WatchPoint
}

// WatchPoint identifies one outpoint the fraud watcher should monitor.
type WatchPoint struct {
	// Outpoint is the ancestor output to watch for a spend.
	Outpoint wire.OutPoint

	// PkScript is the script committed to Outpoint.
	PkScript []byte

	// HeightHint is the earliest plausible spend height.
	HeightHint uint32

	// SweepTapscriptRoot is the committed operator timeout leaf for a
	// VTXO-tree input. It is empty for the leaf-output watch, whose spend
	// is always ancestry materialization rather than a batch-root sweep.
	SweepTapscriptRoot []byte
}

// BuildWatchPlan builds the passive fraud watch set for desc.
func BuildWatchPlan(desc *vtxo.Descriptor) (*WatchPlan, error) {
	if desc == nil {
		return nil, fmt.Errorf("%w: descriptor is nil",
			ErrWatchUnavailable)
	}
	if len(desc.Ancestry) == 0 {
		return nil, fmt.Errorf("%w: descriptor missing ancestry",
			ErrWatchUnavailable)
	}

	heightHint := heightHintFromCreatedHeight(desc.CreatedHeight)
	watchesByOutpoint := make(map[wire.OutPoint]WatchPoint)
	for i := range desc.Ancestry {
		treePath := desc.Ancestry[i].TreePath
		if treePath == nil || treePath.Root == nil {
			return nil, fmt.Errorf("%w: ancestry %d missing "+
				"tree path", ErrWatchUnavailable, i)
		}
		if treePath.BatchOutput == nil {
			return nil, fmt.Errorf("%w: ancestry %d missing "+
				"batch output", ErrWatchUnavailable, i)
		}

		err := collectTreeWatches(
			treePath.Root, treePath.BatchOutput.PkScript,
			treePath.SweepTapscriptRoot, heightHint,
			watchesByOutpoint,
		)
		if err != nil {
			return nil, fmt.Errorf("ancestry %d: %w", i, err)
		}
	}

	watches := make([]WatchPoint, 0, len(watchesByOutpoint))
	for _, watch := range watchesByOutpoint {
		watches = append(watches, watch)
	}
	sortWatchPoints(watches)

	return &WatchPlan{
		TargetOutpoint: desc.Outpoint,
		BatchExpiry:    desc.BatchExpiry,
		Watches:        watches,
	}, nil
}

// collectTreeWatches records every on-path tree input and leaf output. Tree
// inputs detect tree materialization; leaf-output watches detect the first OOR
// checkpoint spending the materialized source VTXO.
func collectTreeWatches(node *tree.Node, inputPkScript []byte,
	sweepTapscriptRoot []byte, heightHint uint32,
	watches map[wire.OutPoint]WatchPoint) error {

	if node == nil {
		return fmt.Errorf("%w: nil tree node", ErrWatchInvalid)
	}
	tx, err := node.ToTx()
	if err != nil {
		return fmt.Errorf("%w: tree node tx: %w", ErrWatchInvalid, err)
	}

	watches[node.Input] = WatchPoint{
		Outpoint:   node.Input,
		PkScript:   append([]byte(nil), inputPkScript...),
		HeightHint: heightHint,
		SweepTapscriptRoot: append(
			[]byte(nil), sweepTapscriptRoot...,
		),
	}

	if node.IsLeaf() {
		leafOutpoint, err := node.GetNonAnchorOutpoint()
		if err != nil {
			return fmt.Errorf("%w: leaf output: %w",
				ErrWatchInvalid, err)
		}
		if int(leafOutpoint.Index) >= len(node.Outputs) {
			return fmt.Errorf("%w: leaf output %s out of range",
				ErrWatchInvalid, *leafOutpoint)
		}

		watches[*leafOutpoint] = WatchPoint{
			Outpoint: *leafOutpoint,
			PkScript: append(
				[]byte(nil),
				node.Outputs[leafOutpoint.Index].PkScript...,
			),
			HeightHint: heightHint,
		}

		return nil
	}

	for outputIndex, child := range node.Children {
		if int(outputIndex) >= len(node.Outputs) {
			return fmt.Errorf("%w: child output index %d out "+
				"of range", ErrWatchInvalid, outputIndex)
		}
		childInput := wire.OutPoint{
			Hash:  tx.TxHash(),
			Index: outputIndex,
		}
		if child.Input != childInput {
			return fmt.Errorf("%w: child input %s does not match "+
				"parent output %s", ErrWatchInvalid,
				child.Input, childInput)
		}

		err := collectTreeWatches(
			child, node.Outputs[outputIndex].PkScript,
			sweepTapscriptRoot, heightHint, watches,
		)
		if err != nil {
			return err
		}
	}

	return nil
}

// heightHintFromCreatedHeight returns a backend-compatible chain height hint.
func heightHintFromCreatedHeight(createdHeight int32) uint32 {
	if createdHeight <= 0 {
		return 1
	}

	return uint32(createdHeight)
}

// sortWatchPoints sorts watch points by outpoint for deterministic actor IO.
func sortWatchPoints(watches []WatchPoint) {
	sort.Slice(watches, func(i, j int) bool {
		return outpointLess(watches[i].Outpoint, watches[j].Outpoint)
	})
}

// outpointLess returns the canonical ordering for watch outpoints.
// Compares the raw 32-byte hash with bytes.Compare to avoid the
// per-call hex/reverse allocation of chainhash.Hash.String(). The
// resulting order is only used for deterministic watch registration
// and test assertions; the comparison direction (display-order vs
// raw-bytes) does not matter as long as it is stable.
func outpointLess(a, b wire.OutPoint) bool {
	if cmp := bytes.Compare(a.Hash[:], b.Hash[:]); cmp != 0 {
		return cmp < 0
	}

	return a.Index < b.Index
}
