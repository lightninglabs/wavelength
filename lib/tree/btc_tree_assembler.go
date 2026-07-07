package tree

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
)

const (
	// DefaultRadix is the default branching factor for tree construction.
	DefaultRadix = 2
)

// TreeConfig carries the parameters needed to build a BTC-only tree.
type TreeConfig struct {
	// OperatorKey is the operator's public key (included in all cosigner
	// sets).
	OperatorKey *btcec.PublicKey

	// SweepTapscriptRoot is the tapscript root used for the sweep script.
	// This provides the unilateral exit path for tree outputs.
	SweepTapscriptRoot []byte

	// Radix is the maximum number of children per branch node. If zero,
	// defaults to 2.
	Radix int

	// WeightFn determines how leaves are weighted for partitioning.
	// If nil, defaults to WeightByBtcAmount().
	WeightFn PartitionWeightFunc
}

// TreeAssembler builds a BTC-only tree using the two-pass approach:
// structure building (pass 1) followed by materialization (pass 2).
type TreeAssembler struct {
	cfg TreeConfig
}

// NewTreeAssembler constructs an assembler with the given configuration.
func NewTreeAssembler(cfg TreeConfig) *TreeAssembler {
	return &TreeAssembler{cfg: cfg}
}

// BuildTree assembles a BTC-only tree using a two-pass approach:
//
// Pass 1 (bottom-up): Build tree structure from leaf descriptors, computing
// cosigners at each level. No transactions are built yet.
//
// Pass 2 (top-down): Materialize transactions starting from the root, filling
// in Input, Outputs, and FinalKey as we traverse down the tree.
//
// Parameters:
//   - rootInput: The outpoint being spent by the root transaction
//   - rootOutput: The output being spent (provides BTC value)
//   - leaves: Leaf descriptors defining the tree leaves
//
// Returns the assembled tree.
func (a *TreeAssembler) BuildTree(rootInput wire.OutPoint,
	rootOutput *wire.TxOut, leaves []LeafDescriptor) (*Tree, error) {

	if len(leaves) == 0 {
		return nil, fmt.Errorf("no leaves supplied")
	}
	if rootOutput == nil {
		return nil, fmt.Errorf("root output cannot be nil")
	}
	if a.cfg.OperatorKey == nil {
		return nil, fmt.Errorf("operator key cannot be nil")
	}
	if a.cfg.Radix < DefaultRadix && a.cfg.Radix != 0 {
		return nil, fmt.Errorf("radix must be at least %d, got %d",
			DefaultRadix, a.cfg.Radix)
	}

	// Note: SweepTapscriptRoot can be nil for connector trees which have
	// no sweep script (they're signed only by the operator).

	// Apply defaults.
	radix := a.cfg.Radix
	if radix == 0 {
		radix = DefaultRadix
	}

	weightFn := a.cfg.WeightFn
	if weightFn == nil {
		weightFn = WeightByBtcAmount()
	}

	// Pass 1: Build tree structure bottom-up. This determines the tree
	// shape, partitions leaves into groups, and computes cosigners at
	// each level.
	structCfg := StructureConfig{
		OperatorKey: a.cfg.OperatorKey,
		Radix:       radix,
		WeightFn:    weightFn,
	}

	result, err := BuildStructure(leaves, structCfg)
	if err != nil {
		return nil, fmt.Errorf("build tree structure: %w", err)
	}
	root := result.Root

	// Sanity check: the sum of all leaf BTC amounts (stored in
	// root.Amount) must be equal to the root output value.
	rootValue := btcutil.Amount(rootOutput.Value)
	if root.Amount != rootValue {
		return nil, fmt.Errorf("leaf amounts sum (%v) must equal the "+
			"output value (%v)", root.Amount, rootValue)
	}

	// Pass 2: Materialize transactions top-down. The BTCMaterializer
	// uses the leaf scripts map from structure building.
	leafScriptFn := ScriptLookupFromMap(result.LeafScriptMap)
	mat := NewBTCMaterializer(
		a.cfg.OperatorKey, a.cfg.SweepTapscriptRoot, leafScriptFn,
	)

	// BTC values are now carried by each node's Amount field (set during
	// structure building), so we only need to provide the root input.
	matParams := MaterializeParams{
		Input: rootInput,
	}

	// Bitcoin-only tree materialization doesn't use the context so we can
	// just pass background.
	if err := Materialize(
		context.Background(), root, matParams, mat,
	); err != nil {
		return nil, fmt.Errorf("materialize tree: %w", err)
	}

	return &Tree{
		Root:               root,
		BatchOutpoint:      rootInput,
		BatchOutput:        rootOutput,
		SweepTapscriptRoot: a.cfg.SweepTapscriptRoot,
	}, nil
}
