package tree

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

// Tree wraps a Node with additional context needed for operations.
//
// **Cache-aliasing invariant.** A *Tree value is treated as
// effectively immutable once it has been published from a builder or
// resolver. Multiple downstream consumers may share the same *Tree
// pointer through caches and ancestry-fragment slices (see
// `indexer.lineageResolver.treeByKey` and
// `indexer.cloneLineage`), and silently mutating a shared tree's
// nodes or roots would corrupt every aliasing reader. Callers that
// need to transform a tree must clone it first; in-place mutation of
// a published *Tree is a bug.
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

	return NewTreeWithConfig(
		rootOutpoint, rootOutput, leaves, operatorKey,
		sweepTapscriptRoot, TreeBuildConfig{
			Radix: radix,
		},
	)
}

// TreeBuildConfig configures tree construction.
type TreeBuildConfig struct {
	// Radix is the branching factor of the tree.
	Radix int

	// WeightFunc optionally overrides how leaves are balanced into groups.
	// When nil, leaves are distributed using WeightByBtcAmount().
	WeightFunc PartitionWeightFunc
}

// NewTreeWithConfig constructs a transaction tree from the given leaves using
// the two-pass approach with the provided configuration.
func NewTreeWithConfig(rootOutpoint wire.OutPoint, rootOutput *wire.TxOut,
	leaves []LeafDescriptor, operatorKey *btcec.PublicKey,
	sweepTapscriptRoot []byte, cfg TreeBuildConfig) (*Tree, error) {

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

	if cfg.Radix < 2 {
		return nil, fmt.Errorf("radix must be at least 2, got %d",
			cfg.Radix)
	}

	// Use BTCTreeAssembler with the two-pass approach.
	assembler := NewTreeAssembler(TreeConfig{
		OperatorKey:        operatorKey,
		SweepTapscriptRoot: sweepTapscriptRoot,
		Radix:              cfg.Radix,
		WeightFn:           cfg.WeightFunc,
	})

	return assembler.BuildTree(rootOutpoint, rootOutput, leaves)
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
func (t *Tree) SubmitTreeSigs(sigs map[TxID]*schnorr.Signature) error {
	if sigs == nil {
		return fmt.Errorf("signatures map cannot be nil")
	}

	return t.Root.ForEach(func(node *Node) error {
		txid, err := node.TXID()
		if err != nil {
			return fmt.Errorf("failed to get TXID: %w", err)
		}

		sig, exists := sigs[txid]
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
		wallet, signerKey, t.SweepTapscriptRoot, prevOutFetcher, t.Root,
	)
}

// ValidatePath validates the tree path from the batch output to the client's
// VTXO for a given signing key. Each signing key maps to exactly one VTXO,
// providing a unique identifier for the VTXO and keeping MuSig2 signing simple.
//
// The client MUST validate the complete path before signing the boarding UTXO.
// This ensures they can recover their funds even if the operator disappears.
//
// Returns the extracted client sub-tree on success.
func (t *Tree) ValidatePath(signingKey *btcec.PublicKey,
	expectedLeaf LeafDescriptor, operatorKey *btcec.PublicKey) (*Tree,
	error) {

	if t == nil {
		return nil, fmt.Errorf("tree is nil")
	}

	// Use VerifyVTXOPath for core validation: script match, exactly 1 leaf
	// per signing key, anchor output, and cosigner presence in all nodes.
	if err := t.VerifyVTXOPath(
		signingKey, expectedLeaf.PkScript,
	); err != nil {
		return nil, fmt.Errorf("VTXO path verification failed: %w", err)
	}

	// Extract the client's sub-tree for additional validation and to
	// return to caller.
	clientTree, err := t.ExtractPathForCoSigners(signingKey)
	if err != nil {
		return nil, fmt.Errorf("failed to extract client path: %w", err)
	}

	// Get the single leaf node (VerifyVTXOPath already enforced exactly 1).
	leaves := clientTree.Root.GetLeafNodes()
	leaf := leaves[0]
	vtxoOutput := leaf.Outputs[0]

	// Ensure the VTXO amount matches the request to prevent value
	// extraction by the operator.
	if vtxoOutput.Value != int64(expectedLeaf.Amount) {
		return nil, fmt.Errorf("VTXO output value %d != expected %d",
			vtxoOutput.Value, expectedLeaf.Amount)
	}

	// Confirm operator key is present as co-signer to enable collaborative
	// spending path.
	if !ContainsCosigner(leaf.CoSigners, operatorKey) {
		return nil, fmt.Errorf("leaf does not include operator key " +
			"in co-signers")
	}

	return clientTree, nil
}

// ValidateAndSubmitSignatures validates and submits the complete VTXT
// signatures to the tree. This must be called BEFORE the client signs the
// boarding UTXO input.
//
// The signatures are provided as a map from transaction ID to raw signature
// bytes. Each entry corresponds to a transaction in the VTXT.
//
// The client MUST NOT sign the boarding UTXO until the VTXT is fully signed
// and validated. Otherwise, the operator could include the boarding UTXO in a
// commitment tx without providing valid VTXOs.
func (t *Tree) ValidateAndSubmitSignatures(
	signatures map[chainhash.Hash][]byte) error {

	if t == nil {
		return fmt.Errorf("tree is nil")
	}
	if len(signatures) == 0 {
		return fmt.Errorf("no signatures provided")
	}

	// Parse each raw signature into a Schnorr signature structure.
	sigMap := make(map[TxID]*schnorr.Signature, len(signatures))
	for txid, sigBytes := range signatures {
		sig, err := schnorr.ParseSignature(sigBytes)
		if err != nil {
			return fmt.Errorf("failed to parse signature for tx "+
				"%s: %w", txid, err)
		}
		sigMap[txid] = sig
	}

	// Submit and validate all signatures atomically to ensure the entire
	// tree is properly signed.
	if err := t.SubmitTreeSigs(sigMap); err != nil {
		return fmt.Errorf("failed to submit tree signatures: %w", err)
	}

	// Perform cryptographic verification of all signatures to guarantee
	// the operator has properly co-signed the VTXT.
	if err := t.VerifySigned(); err != nil {
		return fmt.Errorf("tree signature verification failed: %w", err)
	}

	return nil
}

// ValidateAnchors validates that all transactions in the tree have valid
// ephemeral anchor outputs for CPFP fee bumping (BIP 431).
//
// Without valid anchors, the client cannot broadcast the VTXT chain for
// unilateral exit, resulting in fund loss.
func (t *Tree) ValidateAnchors() error {
	if t == nil {
		return fmt.Errorf("tree is nil")
	}

	// Validate anchors in all tree nodes recursively.
	return t.Root.ForEach(func(node *Node) error {
		if node == nil {
			return fmt.Errorf("tree node is nil")
		}

		// Convert node to transaction to check version and outputs.
		tx, err := node.ToTx()
		if err != nil {
			return fmt.Errorf("failed to convert node to tx: %w",
				err)
		}

		// Verify transaction version is 3 for BIP 431 ephemeral
		// anchors.
		if tx.Version != 3 {
			return fmt.Errorf("transaction version is %d, "+
				"expected 3 for BIP 431 ephemeral anchors",
				tx.Version)
		}

		// All virtual transactions must have at least one output
		// (anchor).
		if len(node.Outputs) == 0 {
			return fmt.Errorf("transaction has no outputs")
		}

		// The last output must be the ephemeral anchor.
		anchorIdx := len(node.Outputs) - 1
		anchorOutput := node.Outputs[anchorIdx]

		// Anchor must have zero value.
		if anchorOutput.Value != 0 {
			return fmt.Errorf("anchor output at index %d has "+
				"value %d, expected 0", anchorIdx,
				anchorOutput.Value)
		}

		// Anchor script must match the standard ephemeral anchor
		// script.
		matchesAnchorScript := bytes.Equal(
			anchorOutput.PkScript, arkscript.AnchorPkScript,
		)
		if !matchesAnchorScript {
			return fmt.Errorf("anchor output at index %d has "+
				"invalid script", anchorIdx)
		}

		return nil
	})
}

// TxidEntry holds information about a single transaction in a tree, including
// its txid, level in the tree, and which parent output it spends.
type TxidEntry struct {
	// Txid is the transaction hash for this tree node.
	Txid chainhash.Hash

	// TreeLevel is the depth of this node (0 = root, increasing toward
	// leaves).
	TreeLevel int

	// OutputIndex is which output of the parent transaction this node
	// spends.
	OutputIndex uint32
}

// ExtractTxids walks the tree using BFS and returns all transaction IDs with
// their tree level and output index. This is useful for building indexes that
// map txids to trees for efficient lookup when chain events occur.
func (t *Tree) ExtractTxids() ([]TxidEntry, error) {
	if t == nil || t.Root == nil {
		return nil, nil
	}

	var entries []TxidEntry

	// Use BFS to traverse the tree level by level. We track each node with
	// its level.
	type nodeWithLevel struct {
		node  *Node
		level int
	}

	queue := NewQueue[nodeWithLevel]()
	queue.Enqueue(nodeWithLevel{node: t.Root, level: 0})

	for !queue.IsEmpty() {
		item, _ := queue.Dequeue()
		node, level := item.node, item.level

		// Get this node's txid.
		txid, err := node.TXID()
		if err != nil {
			return nil, fmt.Errorf("get txid at level %d: %w",
				level, err)
		}

		entries = append(entries, TxidEntry{
			Txid:        txid,
			TreeLevel:   level,
			OutputIndex: node.Input.Index,
		})

		// Enqueue children for next level.
		for _, child := range node.Children {
			queue.Enqueue(nodeWithLevel{
				node:  child,
				level: level + 1,
			})
		}
	}

	return entries, nil
}
