// Package fraud detects recipient-side OOR ancestry materialization.
package fraud

import (
	"bytes"
	"fmt"
	"sort"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
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

	// SweepLeafHash is the tap hash of the operator's unilateral-CSV
	// timeout leaf committed in this output's taproot. A spend that
	// reveals exactly this leaf is the operator's legitimate batch sweep
	// rather than a sender materializing ancestry, so it is what
	// distinguishes the two. Empty when the tree carries no sweep script
	// (connector trees), in which case no spend can be attributed to the
	// operator and every spend escalates.
	SweepLeafHash []byte
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
			heightHint, treePath.SweepTapscriptRoot,
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
		Watches:        watches,
	}, nil
}

// collectTreeWatches records every on-path tree input and leaf output. Tree
// inputs detect tree materialization; leaf-output watches detect the first OOR
// checkpoint spending the materialized source VTXO.
func collectTreeWatches(node *tree.Node, inputPkScript []byte,
	heightHint uint32, sweepLeafHash []byte,
	watches map[wire.OutPoint]WatchPoint) error {

	if node == nil {
		return fmt.Errorf("%w: nil tree node", ErrWatchInvalid)
	}
	tx, err := node.ToTx()
	if err != nil {
		return fmt.Errorf("%w: tree node tx: %w", ErrWatchInvalid, err)
	}

	watches[node.Input] = WatchPoint{
		Outpoint:      node.Input,
		PkScript:      append([]byte(nil), inputPkScript...),
		HeightHint:    heightHint,
		SweepLeafHash: append([]byte(nil), sweepLeafHash...),
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

		// A VTXO leaf output commits no operator sweep leaf: its
		// taproot carries only the collaborative and owner-timeout
		// paths. Leave SweepLeafHash empty so no spend of it can ever
		// be attributed to the operator.
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
			child, node.Outputs[outputIndex].PkScript, heightHint,
			sweepLeafHash, watches,
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

// revealedTapLeafHash returns the tap hash of the script revealed by a
// tapscript spend of the given input, and whether the input was a tapscript
// spend at all.
//
// A taproot script-path witness ends with the control block and carries the
// revealed script immediately before it. A key-path spend (a single signature)
// reveals no script and returns false, as does any witness too short to be a
// script path.
func revealedTapLeafHash(tx *wire.MsgTx,
	inputIndex uint32) (chainhash.Hash, bool) {

	if tx == nil || inputIndex >= uint32(len(tx.TxIn)) {
		return chainhash.Hash{}, false
	}

	witness := tx.TxIn[inputIndex].Witness

	// script + control block is the minimum for a script-path spend.
	const minScriptPathWitnessItems = 2
	if len(witness) < minScriptPathWitnessItems {
		return chainhash.Hash{}, false
	}

	script := witness[len(witness)-2]
	if len(script) == 0 {
		return chainhash.Hash{}, false
	}

	return txscript.NewBaseTapLeaf(script).TapHash(), true
}

// IsOperatorSweepSpend reports whether the observed spend of this watch point
// took the operator's committed unilateral-CSV timeout leaf, i.e. whether it
// is the operator's legitimate batch sweep.
//
// Provenance, not timing, is the test. Chain height only proves the timeout
// path has matured; it says nothing about which path a given transaction
// actually used, so a hostile expansion confirmed after expiry would pass a
// height check while being exactly the thing fraud response exists to answer.
// Matching the revealed tapleaf against the one committed in the watched
// output settles it.
func (p WatchPoint) IsOperatorSweepSpend(tx *wire.MsgTx,
	inputIndex uint32) bool {

	if len(p.SweepLeafHash) == 0 {
		return false
	}

	revealed, ok := revealedTapLeafHash(tx, inputIndex)
	if !ok {
		return false
	}

	return bytes.Equal(revealed[:], p.SweepLeafHash)
}
