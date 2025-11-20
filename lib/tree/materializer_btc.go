package tree

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/scripts"
)

// BTCMaterializer builds simple BTC transactions without asset semantics.
// It implements the Materializer interface for BTC-only trees.
type BTCMaterializer struct {
	// OperatorKey is the operator's public key.
	OperatorKey *btcec.PublicKey

	// SweepTapscriptRoot is the tapscript root for the sweep script.
	SweepTapscriptRoot []byte
}

// NewBTCMaterializer creates a new BTC materializer.
func NewBTCMaterializer(operatorKey *btcec.PublicKey,
	sweepTapscriptRoot []byte) *BTCMaterializer {

	return &BTCMaterializer{
		OperatorKey:        operatorKey,
		SweepTapscriptRoot: sweepTapscriptRoot,
	}
}

// MaterializeNode fills in transaction data for a single node. For BTC trees,
// this involves computing the final key and building outputs.
func (m *BTCMaterializer) MaterializeNode(_ context.Context, node *Node,
	params MaterializeParams) (map[uint32]MaterializeParams, error) {

	// Set the input outpoint.
	node.Input = params.Input

	if node.IsLeaf() {
		return m.materializeLeaf(node, params)
	}

	return m.materializeBranch(node, params)
}

// materializeLeaf builds outputs for a leaf node.
func (m *BTCMaterializer) materializeLeaf(node *Node,
	params MaterializeParams) (map[uint32]MaterializeParams, error) {

	// Compute the final key for this node's input.
	finalKey, err := ComputeFinalKey(node.CoSigners, m.SweepTapscriptRoot)
	if err != nil {
		return nil, fmt.Errorf("compute final key: %w", err)
	}

	node.FinalKey = finalKey
	node.TaprootTweak = m.SweepTapscriptRoot

	// Get leaf PkScript from metadata.
	if node.Metadata == nil || len(node.Metadata.LeafPkScript) == 0 {
		return nil, fmt.Errorf("leaf node missing pkscript in metadata")
	}

	// Build outputs: [leaf output, anchor].
	node.Outputs = []*wire.TxOut{
		wire.NewTxOut(params.InputBtcValue, node.Metadata.LeafPkScript),
		scripts.AnchorOutput(),
	}

	// Leaves have no children.
	return nil, nil
}

// materializeBranch builds outputs for a branch node.
func (m *BTCMaterializer) materializeBranch(node *Node,
	params MaterializeParams) (map[uint32]MaterializeParams, error) {

	// Compute the final key for this node's input.
	finalKey, err := ComputeFinalKey(node.CoSigners, m.SweepTapscriptRoot)
	if err != nil {
		return nil, fmt.Errorf("compute final key: %w", err)
	}

	node.FinalKey = finalKey
	node.TaprootTweak = m.SweepTapscriptRoot

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
			return nil, fmt.Errorf("compute child key for "+
				"index %d: %w", idx, err)
		}

		pkScript, err := txscript.PayToTaprootScript(childKey)
		if err != nil {
			return nil, fmt.Errorf("create pkscript for "+
				"index %d: %w", idx, err)
		}

		outputs = append(outputs, wire.NewTxOut(
			params.ChildBtcValue, pkScript,
		))
	}

	// Add anchor output.
	outputs = append(outputs, scripts.AnchorOutput())

	node.Outputs = outputs

	// Compute TXID for child inputs.
	txHash, err := node.TXID()
	if err != nil {
		return nil, fmt.Errorf("get parent txid: %w", err)
	}

	// Build child params.
	childParams := make(map[uint32]MaterializeParams)
	for i, idx := range indices {
		child := node.Children[idx]

		childParams[idx] = MaterializeParams{
			Input: wire.OutPoint{
				Hash:  txHash,
				Index: uint32(i),
			},
			InputBtcValue: params.ChildBtcValue,
			ChildBtcValue: computeChildBtcValue(child,
				params.ChildBtcValue),
		}
	}

	return childParams, nil
}
