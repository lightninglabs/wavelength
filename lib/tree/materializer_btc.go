package tree

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
)

// ScriptLookup returns the output script (pkscript) for a leaf node.
// Returns nil if the node is not a leaf or has no script.
type ScriptLookup func(node *Node) []byte

// ScriptLookupFromMap creates a ScriptLookup from a map of node
// pointers to pkscripts. This is the standard way to provide leaf scripts
// for BTC tree materialization while keeping the shared Node type
// asset-agnostic and branch nodes untouched.
func ScriptLookupFromMap(leafScripts map[*Node][]byte) ScriptLookup {
	return func(node *Node) []byte {
		if leafScripts == nil {
			return nil
		}

		return leafScripts[node]
	}
}

// BTCMaterializer builds simple BTC tree transactions. It implements the
// Materializer interface for BTC-only trees.
type BTCMaterializer struct {
	// OperatorKey is the operator's public key.
	OperatorKey *btcec.PublicKey

	// SweepTapscriptRoot is the tapscript root for the sweep script.
	SweepTapscriptRoot []byte

	// LeafScriptFn returns the output script for leaf nodes. This is
	// populated during structure building and used here during
	// materialization.
	LeafScriptFn ScriptLookup
}

// NewBTCMaterializer creates a new BTC materializer. The leafScriptFn
// parameter provides leaf output scripts populated during structure building.
func NewBTCMaterializer(operatorKey *btcec.PublicKey, sweepTapscriptRoot []byte,
	leafScriptFn ScriptLookup) *BTCMaterializer {

	return &BTCMaterializer{
		OperatorKey:        operatorKey,
		SweepTapscriptRoot: sweepTapscriptRoot,
		LeafScriptFn:       leafScriptFn,
	}
}

// MaterializeNode fills in transaction data for a single node. For BTC trees,
// this involves computing the final key and building outputs.
//
// NOTE: BTC trees use Node.Amount (set during structure building) for output
// values.
func (m *BTCMaterializer) MaterializeNode(_ context.Context, node *Node,
	params MaterializeParams) (map[uint32]MaterializeParams, error) {

	// Set the input outpoint.
	node.Input = params.Input

	if node.IsLeaf() {
		return m.materializeLeaf(node)
	}

	return m.materializeBranch(node)
}

// materializeLeaf builds outputs for a leaf node.
func (m *BTCMaterializer) materializeLeaf(node *Node) (
	map[uint32]MaterializeParams, error) {

	// Guard against miswired assemblers: LeafScriptFn must be provided.
	if m.LeafScriptFn == nil {
		return nil, fmt.Errorf("leaf script lookup function not set")
	}

	// Compute the final key for this node's input.
	finalKey, err := ComputeFinalKey(node.CoSigners, m.SweepTapscriptRoot)
	if err != nil {
		return nil, fmt.Errorf("compute final key: %w", err)
	}

	node.FinalKey = finalKey

	// Get leaf PkScript using the lookup function (populated during
	// structure building).
	leafPkScript := m.LeafScriptFn(node)
	if len(leafPkScript) == 0 {
		return nil, fmt.Errorf("leaf node missing pkscript")
	}

	// Build outputs: [leaf output, anchor]. Use node.Amount which was set
	// during structure building from the leaf descriptor. We construct
	// the leaf output here (pass 2) because the BTC path defers script
	// attachment to materialization, keeping the shared structure free
	// of BTC-only fields.
	node.Outputs = []*wire.TxOut{
		wire.NewTxOut(int64(node.Amount), leafPkScript),
		arkscript.AnchorOutput(),
	}

	// Leaves have no children.
	return nil, nil
}

// materializeBranch builds outputs for a branch node.
func (m *BTCMaterializer) materializeBranch(node *Node) (
	map[uint32]MaterializeParams, error) {

	// Compute the final key for this node's input.
	finalKey, err := ComputeFinalKey(node.CoSigners, m.SweepTapscriptRoot)
	if err != nil {
		return nil, fmt.Errorf("compute final key: %w", err)
	}

	node.FinalKey = finalKey

	// Build outputs for each child plus anchor.
	indices := sortedChildIndices(node.Children)
	outputs := make([]*wire.TxOut, 0, len(indices)+1)

	for _, idx := range indices {
		child := node.Children[idx]

		// Compute child's output key using their cosigners.
		childKey, err := ComputeFinalKey(
			child.CoSigners, m.SweepTapscriptRoot,
		)
		if err != nil {
			return nil, fmt.Errorf("compute child key for index "+
				"%d: %w", idx, err)
		}

		pkScript, err := txscript.PayToTaprootScript(childKey)
		if err != nil {
			return nil, fmt.Errorf("create pkscript for index "+
				"%d: %w", idx, err)
		}

		// Use child.Amount which is the sum of all leaf amounts in
		// that child's subtree (set during structure building).
		outputs = append(
			outputs,
			wire.NewTxOut(
				int64(child.Amount), pkScript,
			),
		)
	}

	// Add anchor output.
	outputs = append(outputs, arkscript.AnchorOutput())

	node.Outputs = outputs

	// Compute TXID for child inputs.
	txHash, err := node.TXID()
	if err != nil {
		return nil, fmt.Errorf("get parent txid: %w", err)
	}

	// Build child params.
	childParams := make(map[uint32]MaterializeParams)
	for i, idx := range indices {
		childParams[idx] = MaterializeParams{
			Input: wire.OutPoint{
				Hash:  txHash,
				Index: uint32(i),
			},
		}
	}

	return childParams, nil
}
