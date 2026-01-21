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

	// ConfirmedHeight is the block height at which this output was
	// confirmed on-chain. This is used for CSV maturity calculations for
	// operator sweeps.
	ConfirmedHeight uint32

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

// Clone creates a deep copy of this output suitable for returning in query
// responses. This prevents callers from mutating internal state via shared
// pointers or slices.
//
// NOTE: The TreeNode reference is intentionally shared because the batch tree
// is immutable after registration.
func (o *Output) Clone() *Output {
	if o == nil {
		return nil
	}

	var txOutCopy *wire.TxOut
	if o.TxOut != nil {
		txOutCopy = &wire.TxOut{
			Value:    o.TxOut.Value,
			PkScript: append([]byte(nil), o.TxOut.PkScript...),
		}
	}

	return &Output{
		Outpoint: o.Outpoint,
		TxOut:    txOutCopy,

		ConfirmedHeight: o.ConfirmedHeight,
		IsVTXO:          o.IsVTXO,

		TreeNode: o.TreeNode,

		OutputIndex: o.OutputIndex,
	}
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
// responses without exposing internal mutable state. All internal maps,
// bookkeeping fields, and outputs are deep-copied.
//
// NOTE: The underlying batch Tree is shallow-copied because it is immutable
// after registration, so sharing it across clones is safe and avoids
// unnecessary copying.
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

	for k, v := range b.ExistingOutputs {
		clone.ExistingOutputs[k] = v.Clone()
	}

	for k, v := range b.VTXOsOnChain {
		// Preserve pointer identity for outputs tracked in both maps.
		if existing, ok := clone.ExistingOutputs[k]; ok {
			clone.VTXOsOnChain[k] = existing
			continue
		}

		clone.VTXOsOnChain[k] = v.Clone()
	}

	for k, v := range b.WatchedOutpoints {
		clone.WatchedOutpoints[k] = v
	}

	return clone
}
