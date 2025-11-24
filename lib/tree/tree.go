package tree

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

// Tree wraps a Node with additional context needed for operations.
type Tree struct {
	// Root is the root transaction that spends the batch output.
	Root *Node

	// BatchOutpoint is the outpoint in the commitment transaction
	// that the root transaction spends.
	BatchOutpoint wire.OutPoint

	// BatchOutput is the actual output at BatchOutpoint.
	// Used for verification and signing.
	BatchOutput *wire.TxOut

	// SweepTapscriptRoot is the tapscript root hash used for tweaking
	// branch outputs. This is the script from UnilateralCSVTimeoutTapLeaf.
	// For VTXO trees, this is the operator's sweep script.
	// For connector trees, this is nil (no sweep script).
	SweepTapscriptRoot []byte
}

// NewTree constructs a transaction tree from the given leaves using BFS.
// The radix parameter controls the branching factor - each branch node will
// have up to 'radix' children. A radix of 2 creates a binary tree, radix of 4
// creates a quad-tree, etc. Higher radix values reduce tree depth but increase
// transaction size.
func NewTree(rootOutpoint wire.OutPoint, rootOutput *wire.TxOut,
	leaves []LeafDescriptor, operatorKey *btcec.PublicKey,
	sweepTapscriptRoot []byte, radix int) (*Tree, error) {

	// Validate inputs.
	if rootOutput == nil {
		return nil, fmt.Errorf("root output cannot be nil")
	}

	if operatorKey == nil {
		return nil, fmt.Errorf("operator key cannot be nil")
	}

	if len(leaves) == 0 {
		return nil, fmt.Errorf("at least one leaf required")
	}

	if radix < 2 {
		return nil, fmt.Errorf("radix must be at least 2, got %d",
			radix)
	}

	// Build the tree using BFS.
	root, err := buildTreeBFS(
		rootOutpoint, leaves, operatorKey,
		sweepTapscriptRoot, radix,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to build tree: %w", err)
	}

	return &Tree{
		Root:               root,
		BatchOutpoint:      rootOutpoint,
		BatchOutput:        rootOutput,
		SweepTapscriptRoot: sweepTapscriptRoot,
	}, nil
}

// ExtractPathForCoSigners extracts the path relevant for one or more cosigners
// and returns a new Tree containing only the nodes where any of the provided
// cosigner keys are present. This is useful when a client has multiple VTXOs
// with different keys in the same tree.
func (t *Tree) ExtractPathForCoSigners(targetKeys ...*btcec.PublicKey) (*Tree,
	error) {

	if len(targetKeys) == 0 {
		return nil, fmt.Errorf("at least one target key required")
	}

	for _, key := range targetKeys {
		if key == nil {
			return nil, fmt.Errorf("target key cannot be nil")
		}
	}

	extractedRoot, ok := t.Root.ExtractPathForCoSigners(targetKeys...)
	if !ok {
		return nil, nil
	}

	return &Tree{
		Root:               extractedRoot,
		BatchOutpoint:      t.BatchOutpoint,
		BatchOutput:        t.BatchOutput,
		SweepTapscriptRoot: t.SweepTapscriptRoot,
	}, nil
}

// ExtractPathForIndices extracts a minimal subtree containing all the paths to
// the specified leaf indices. Accepts one or more indices as variadic
// parameters. The returned tree will contain only the nodes necessary to reach
// all target leaves.
//
// IMPORTANT: Indices are relative to the current tree structure. After
// extraction, leaves in the returned subtree will be renumbered starting from
// 0. This means ExtractPathForIndices is NOT idempotent - calling it twice
// with the same indices will fail on the second call since the leaf positions
// have changed. If you need stable identifiers across extractions, use
// ExtractPathForCoSigners instead.
func (t *Tree) ExtractPathForIndices(leafIndices ...int) (*Tree, error) {
	if len(leafIndices) == 0 {
		return nil, nil
	}

	extractedRoot, err := t.Root.ExtractPathForIndices(leafIndices...)
	if err != nil {
		return nil, err
	}
	if extractedRoot == nil {
		return nil, fmt.Errorf("no extracted root found")
	}

	return &Tree{
		Root:               extractedRoot,
		BatchOutpoint:      t.BatchOutpoint,
		BatchOutput:        t.BatchOutput,
		SweepTapscriptRoot: t.SweepTapscriptRoot,
	}, nil
}

// Verify recursively verifies that the tree structure is consistent.
// It checks that each child's input correctly references the parent's
// transaction at the expected output index.
func (t *Tree) Verify() error {
	// Verify root spends batch outpoint.
	if t.Root.Input != t.BatchOutpoint {
		return fmt.Errorf("root input does not match batch outpoint")
	}

	// Verify tree structure.
	return t.Root.Verify()
}

// VerifyVTXOPath verifies that the tree contains a valid path to a VTXO for
// the given cosigner and that the leaf VTXO script matches the expected script.
// This is used by clients to verify they received the correct VTXO path.
func (t *Tree) VerifyVTXOPath(coSignerKey *btcec.PublicKey,
	expectedVTXOScript []byte) error {

	if coSignerKey == nil {
		return fmt.Errorf("cosigner key cannot be nil")
	}

	if len(expectedVTXOScript) == 0 {
		return fmt.Errorf("expected VTXO script cannot be empty")
	}

	// Extract path for cosigner.
	extracted, err := t.ExtractPathForCoSigners(coSignerKey)
	if err != nil {
		return fmt.Errorf("failed to extract path: %w", err)
	}

	if extracted == nil {
		return fmt.Errorf("no path found for cosigner")
	}

	// Verify structure.
	if err := extracted.Verify(); err != nil {
		return fmt.Errorf("extracted tree structure invalid: %w", err)
	}

	// Verify extracted tree has exactly one leaf.
	leaves := extracted.Root.GetLeafNodes()
	if len(leaves) != 1 {
		return fmt.Errorf("expected exactly 1 leaf, got %d",
			len(leaves))
	}

	leaf := leaves[0]

	// Verify leaf has 2 outputs: [VTXO, anchor].
	if len(leaf.Outputs) != 2 {
		return fmt.Errorf("leaf should have 2 outputs, got %d",
			len(leaf.Outputs))
	}

	// Verify VTXO output script matches.
	if !bytes.Equal(leaf.Outputs[0].PkScript, expectedVTXOScript) {
		return fmt.Errorf("VTXO script mismatch")
	}

	// Verify anchor output (last output, value 0).
	if leaf.Outputs[1].Value != 0 {
		return fmt.Errorf("anchor output should have value 0, got %d",
			leaf.Outputs[1].Value)
	}

	// Verify cosigner is in all nodes on path.
	for node := range extracted.Root.NodesIter() {
		if !ContainsCosigner(node.CoSigners, coSignerKey) {
			return fmt.Errorf("cosigner not found in node")
		}
	}

	return nil
}

// VerifySigned verifies that all nodes in the tree have valid signatures.
func (t *Tree) VerifySigned() error {
	// Create prev output fetcher.
	prevOutFetcher, err := t.Root.PrevOutputFetcher(t.BatchOutput)
	if err != nil {
		return fmt.Errorf("failed to create prev output fetcher: %w",
			err)
	}

	return t.Root.VerifySigned(prevOutFetcher)
}

// SubmitTreeSigs stores signatures in the tree nodes. This method does NOT
// validate the signatures cryptographically - it only stores them. Use
// VerifySigned after calling this method to validate signatures.
func (t *Tree) SubmitTreeSigs(sigs map[string]*schnorr.Signature) error {
	if sigs == nil {
		return fmt.Errorf("signatures map cannot be nil")
	}

	return t.Root.ForEach(func(node *Node) error {
		txid, err := node.TXID()
		if err != nil {
			return fmt.Errorf("failed to get TXID: %w", err)
		}

		sig, exists := sigs[txid.String()]
		if !exists {
			return fmt.Errorf("signature not found for "+
				"transaction %s", txid.String())
		}

		node.Signature = sig

		return nil
	})
}

// NumLeaves returns the total number of leaf transactions in the tree.
func (t *Tree) NumLeaves() int {
	count := 0
	for range t.Root.LeavesIter() {
		count++
	}

	return count
}

// Depth returns the maximum depth of the tree.
func (t *Tree) Depth() int {
	return t.Root.Depth()
}

// NumTx returns the total number of transactions in the tree.
func (t *Tree) NumTx() int {
	return t.Root.NumTx()
}

// PrettyPrint returns a human-readable string representation of the full tree.
func (t *Tree) PrettyPrint() string {
	return t.Root.PrettyPrint()
}

// NewTreeSignerSession creates a TreeSignerSession for this tree.
// This is a convenience wrapper that sets up the session with the tree's
// context.
func (t *Tree) NewTreeSignerSession(wallet input.MuSig2Signer,
	signerKey *keychain.KeyDescriptor) (*SignerSession, error) {

	// Create prev output fetcher.
	prevOutFetcher, err := t.Root.PrevOutputFetcher(t.BatchOutput)
	if err != nil {
		return nil, fmt.Errorf("failed to create prev output "+
			"fetcher: %w", err)
	}

	return NewSignerSession(
		wallet, signerKey, t.SweepTapscriptRoot, prevOutFetcher,
		t.Root,
	)
}
