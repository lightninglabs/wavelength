package batchwatcher

import (
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/tree"
)

// Output represents an on-chain output that exists as part of a batch tree.
type Output struct {
	// Outpoint identifies the output on-chain.
	Outpoint wire.OutPoint

	// TxOut contains the output's value and pkScript.
	TxOut *wire.TxOut

	// IsVTXO indicates whether this output is a VTXO leaf output. When
	// true, this output represents a spendable VTXO that should trigger
	// fraud detection notifications.
	IsVTXO bool

	// TreeNode is a reference to the tree node that created this output.
	// This is used for progressive watching - when this output is spent,
	// we can look up the child nodes to watch next.
	TreeNode *tree.Node

	// OutputIndex is the index of this output within the TreeNode's
	// outputs. This is needed to look up the child node when this output
	// is spent.
	OutputIndex uint32
}

// BatchTreeState tracks the on-chain state of a single batch's VTXO tree.
type BatchTreeState struct {
	// BatchID uniquely identifies this batch.
	BatchID BatchID

	// Tree is the full pre-signed transaction tree for this batch.
	Tree *tree.Tree

	// ExpiryHeight is the block height at which this batch expires and
	// becomes sweepable by the operator.
	ExpiryHeight uint32

	// SpentNodes tracks which tree transactions have been confirmed
	// on-chain. The key is the txid of the spent transaction.
	SpentNodes map[chainhash.Hash]struct{}

	// ExistingOutputs tracks outputs that currently exist on-chain
	// (unspent). These are outputs from tree transactions that have
	// been confirmed but not yet spent.
	ExistingOutputs map[wire.OutPoint]*Output

	// VTXOsOnChain tracks VTXO leaf outputs that are now spendable
	// on-chain. These are a subset of ExistingOutputs.
	VTXOsOnChain map[wire.OutPoint]*Output

	// WatchedOutpoints tracks which outpoints we have registered spend
	// watches for. This prevents duplicate registrations.
	WatchedOutpoints map[wire.OutPoint]struct{}
}

// NewBatchTreeState creates a new BatchTreeState for the given batch.
func NewBatchTreeState(batchID BatchID, t *tree.Tree,
	expiryHeight uint32) *BatchTreeState {

	return &BatchTreeState{
		BatchID:          batchID,
		Tree:             t,
		ExpiryHeight:     expiryHeight,
		SpentNodes:       make(map[chainhash.Hash]struct{}),
		ExistingOutputs:  make(map[wire.OutPoint]*Output),
		VTXOsOnChain:     make(map[wire.OutPoint]*Output),
		WatchedOutpoints: make(map[wire.OutPoint]struct{}),
	}
}

// IsNodeSpent returns true if the node with the given txid has been spent.
func (b *BatchTreeState) IsNodeSpent(txid chainhash.Hash) bool {
	_, exists := b.SpentNodes[txid]
	return exists
}

// MarkNodeSpent marks the node with the given txid as spent.
func (b *BatchTreeState) MarkNodeSpent(txid chainhash.Hash) {
	b.SpentNodes[txid] = struct{}{}
}

// AddExistingOutput adds an output to the set of existing on-chain outputs.
func (b *BatchTreeState) AddExistingOutput(output *Output) {
	b.ExistingOutputs[output.Outpoint] = output

	// If this is a VTXO output, also track it in VTXOsOnChain.
	if output.IsVTXO {
		b.VTXOsOnChain[output.Outpoint] = output
	}
}

// RemoveExistingOutput removes an output from the set of existing outputs.
// This is called when an output is spent.
func (b *BatchTreeState) RemoveExistingOutput(outpoint wire.OutPoint) *Output {
	output, exists := b.ExistingOutputs[outpoint]
	if !exists {
		return nil
	}

	delete(b.ExistingOutputs, outpoint)
	delete(b.VTXOsOnChain, outpoint)

	return output
}

// GetExistingOutput returns the output at the given outpoint if it exists.
func (b *BatchTreeState) GetExistingOutput(outpoint wire.OutPoint) *Output {
	return b.ExistingOutputs[outpoint]
}

// IsWatched returns true if the outpoint is already being watched for spends.
func (b *BatchTreeState) IsWatched(outpoint wire.OutPoint) bool {
	_, exists := b.WatchedOutpoints[outpoint]
	return exists
}

// MarkWatched marks the outpoint as being watched for spends.
func (b *BatchTreeState) MarkWatched(outpoint wire.OutPoint) {
	b.WatchedOutpoints[outpoint] = struct{}{}
}

// GetUnspentOutputs returns all currently unspent outputs for this batch.
func (b *BatchTreeState) GetUnspentOutputs() []*Output {
	outputs := make([]*Output, 0, len(b.ExistingOutputs))
	for _, output := range b.ExistingOutputs {
		outputs = append(outputs, output)
	}

	return outputs
}

// GetVTXOsOnChain returns all VTXO outputs currently on-chain for this batch.
func (b *BatchTreeState) GetVTXOsOnChain() []*Output {
	outputs := make([]*Output, 0, len(b.VTXOsOnChain))
	for _, output := range b.VTXOsOnChain {
		outputs = append(outputs, output)
	}

	return outputs
}

// Clone creates a copy of the BatchTreeState suitable for returning in query
// responses without exposing internal mutable state. All internal maps and
// bookkeeping fields are deep-copied, but the following are shallow-copied:
//
//   - Tree: The batch tree is immutable after registration, so sharing it
//     across clones is safe and avoids unnecessary copying.
//   - Output pointers: The Output structs in ExistingOutputs and VTXOsOnChain
//     are shared between original and clone. These are treated as immutable
//     once created.
func (b *BatchTreeState) Clone() *BatchTreeState {
	clone := &BatchTreeState{
		BatchID: b.BatchID,

		// Tree is shallow-copied; it is immutable after registration.
		Tree:         b.Tree,
		ExpiryHeight: b.ExpiryHeight,
		SpentNodes: make(
			map[chainhash.Hash]struct{}, len(b.SpentNodes),
		),
		ExistingOutputs: make(
			map[wire.OutPoint]*Output, len(b.ExistingOutputs),
		),
		VTXOsOnChain: make(
			map[wire.OutPoint]*Output, len(b.VTXOsOnChain),
		),
		WatchedOutpoints: make(
			map[wire.OutPoint]struct{}, len(b.WatchedOutpoints),
		),
	}

	for k, v := range b.SpentNodes {
		clone.SpentNodes[k] = v
	}

	// Output pointers are shallow-copied. The Output structs are immutable
	// once created, so sharing them between original and clone is safe.
	for k, v := range b.ExistingOutputs {
		clone.ExistingOutputs[k] = v
	}

	for k, v := range b.VTXOsOnChain {
		clone.VTXOsOnChain[k] = v
	}

	for k, v := range b.WatchedOutpoints {
		clone.WatchedOutpoints[k] = v
	}

	return clone
}
