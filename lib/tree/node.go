package tree

import (
	"bytes"
	"fmt"
	"iter"
	"math"
	"sort"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/scripts"
)

// Node represents a single transaction in a virtual transaction tree.
type Node struct {
	// Input is the single outpoint this transaction spends.
	Input wire.OutPoint

	// Outputs are all the outputs created by this transaction.
	// - For VTXO tree leaves: [VTXO output, anchor output]
	// - For connector tree leaves: [dust connector output, anchor output]
	// - For branches: [child1 output, child2 output, ..., anchor output]
	Outputs []*wire.TxOut

	// CoSigners is the set of public keys that must participate in
	// MuSig2 signing for this transaction's input (keyspend path).
	CoSigners []*btcec.PublicKey

	// Children maps output index to child Node.
	// Empty for leaf nodes.
	Children map[uint32]*Node

	// Signature is the final aggregated MuSig2 signature for the input.
	// This is populated after signing is complete.
	Signature *schnorr.Signature

	// FinalKey is the final aggregated public key (after taproot tweak)
	// that must sign this node's input. This is cached to avoid repeated
	// MuSig2 aggregations during signature verification.
	FinalKey *btcec.PublicKey

	// TaprootTweak optionally records the tweak applied when deriving the
	// FinalKey for this node. When nil, callers fall back to the global
	// sweep tapscript root used in legacy VTXO trees.
	TaprootTweak []byte

	// Metadata carries optional bookkeeping for this node/subtree. It is
	// ignored by signing and verification logic.
	Metadata *NodeMetadata
}

// NewLeafNode creates a leaf node (transaction with leaf output).
func NewLeafNode(input wire.OutPoint, leaf LeafDescriptor,
	operatorKey *btcec.PublicKey, sweepTapscriptRoot []byte) (*Node,
	error) {

	// The cosigners for a leaf are the leaf owner and operator.
	cosigners := []*btcec.PublicKey{
		operatorKey,
		leaf.CoSignerKey,
	}

	outputs := []*wire.TxOut{
		// The leaf output (VTXO or connector).
		wire.NewTxOut(int64(leaf.Amount), leaf.PkScript),

		// The zero value ephemeral anchor output.
		scripts.AnchorOutput(),
	}

	// Compute the final key for this node's input at construction time.
	finalKey, err := ComputeFinalKey(cosigners, sweepTapscriptRoot)
	if err != nil {
		return nil, fmt.Errorf("failed to compute final key: %w", err)
	}

	return &Node{
		Input:        input,
		Outputs:      outputs,
		CoSigners:    cosigners,
		TaprootTweak: sweepTapscriptRoot,
		Children:     make(map[uint32]*Node),
		Signature:    nil,
		FinalKey:     finalKey,
		Metadata: &NodeMetadata{
			AssetProof: proofBytes(leaf.Asset),
			Leaf:       leaf.Asset,
		},
	}, nil
}

// NewBranchNode creates a branch node with outputs for each group of leaves.
func NewBranchNode(input wire.OutPoint, groups [][]LeafDescriptor,
	operatorKey *btcec.PublicKey, sweepTapscriptRoot []byte) (*Node,
	error) {

	// Validate inputs.
	if operatorKey == nil {
		return nil, fmt.Errorf("operator key cannot be nil")
	}

	if len(groups) == 0 {
		return nil, fmt.Errorf("at least one group required")
	}

	// Validate all leaf cosigner keys.
	for i, group := range groups {
		if len(group) == 0 {
			return nil, fmt.Errorf("group %d is empty", i)
		}

		for j, leaf := range group {
			if leaf.CoSignerKey == nil {
				return nil, fmt.Errorf("leaf cosigner key "+
					"cannot be nil at groups[%d][%d]", i, j)
			}
		}
	}

	outputs := make([]*wire.TxOut, 0, len(groups)+1)
	allCosigners := []*btcec.PublicKey{operatorKey}

	// Each group will become an output.
	for groupIdx, group := range groups {
		// Calculate total amount and collect cosigners for this group.
		var (
			amount         = int64(0)
			groupCosigners = []*btcec.PublicKey{operatorKey}
		)

		for _, leaf := range group {
			if leaf.Amount < 0 {
				return nil, fmt.Errorf("negative amount "+
					"in group %d: %d", groupIdx,
					leaf.Amount)
			}

			leafAmt := int64(leaf.Amount)

			// Check for overflow when accumulating amounts.
			if amount > 0 && leafAmt > 0 &&
				amount > math.MaxInt64-leafAmt {

				return nil, fmt.Errorf("amount overflow in "+
					"group %d", groupIdx)
			}

			amount += leafAmt
			groupCosigners = append(
				groupCosigners, leaf.CoSignerKey,
			)
			allCosigners = append(allCosigners, leaf.CoSignerKey)
		}

		// Deduplicate cosigners for this group.
		groupCosigners = UniqueCosigners(groupCosigners)

		// Sanity check: should always have at least operator key.
		if len(groupCosigners) == 0 {
			return nil, fmt.Errorf("no cosigners for group %d "+
				"after deduplication", groupIdx)
		}

		// Compute the aggregated key for this group's output.
		// This uses the same logic as computing the input signing key.
		tapKey, err := ComputeFinalKey(
			groupCosigners, sweepTapscriptRoot,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to compute key for "+
				"group %d: %w", groupIdx, err)
		}

		inputScript, err := txscript.PayToTaprootScript(tapKey)
		if err != nil {
			return nil, fmt.Errorf("failed to create script "+
				"pubkey for group %d: %w", groupIdx, err)
		}

		outputs = append(outputs, &wire.TxOut{
			Value:    amount,
			PkScript: inputScript,
		})
	}

	// Add anchor output.
	outputs = append(outputs, scripts.AnchorOutput())

	// Use all unique cosigners for the branch transaction.
	allCosigners = UniqueCosigners(allCosigners)

	// Compute the final key for this node's input at construction time.
	finalKey, err := ComputeFinalKey(allCosigners, sweepTapscriptRoot)
	if err != nil {
		return nil, fmt.Errorf("failed to compute final key: %w", err)
	}

	return &Node{
		Input:        input,
		Outputs:      outputs,
		CoSigners:    allCosigners,
		TaprootTweak: sweepTapscriptRoot,
		Children:     make(map[uint32]*Node),
		Signature:    nil,
		FinalKey:     finalKey,
		Metadata:     aggregateBranchMetadata(groups),
	}, nil
}

// ComputeFinalKey computes the final aggregated public key for signing.
// This is a helper function that aggregates the cosigners and applies the
// taproot tweak with the given sweep tapscript root. It handles both
// single-key and multi-key cases.
// ComputeInternalKey computes the MuSig2 aggregate key without any taproot
// tweak. This is the "internal key" that should be passed to tapd, which will
// then apply its own taproot tweak (including asset commitment).
func ComputeInternalKey(cosigners []*btcec.PublicKey) (*btcec.PublicKey, error) {
	if len(cosigners) == 0 {
		return nil, fmt.Errorf("no cosigners provided")
	}

	// Single cosigner: just return the key.
	if len(cosigners) == 1 {
		return cosigners[0], nil
	}

	// Multi-key case: MuSig2 aggregation without tweak.
	aggKey, _, _, err := musig2.AggregateKeys(cosigners, true)
	if err != nil {
		return nil, fmt.Errorf("failed to aggregate keys: %w", err)
	}

	return aggKey.PreTweakedKey, nil
}

// ComputeFinalKey computes the final taproot output key from cosigners and a
// taproot tweak. The tweak should be the combined taproot root (including
// asset commitment for Taproot Asset outputs).
func ComputeFinalKey(cosigners []*btcec.PublicKey,
	sweepTapscriptRoot []byte) (*btcec.PublicKey, error) {

	if len(cosigners) == 0 {
		return nil, fmt.Errorf("no cosigners provided")
	}

	// Handle single cosigner case.
	if len(cosigners) == 1 {
		// For a single key, apply the taproot output key computation.
		return txscript.ComputeTaprootOutputKey(
			cosigners[0], sweepTapscriptRoot,
		), nil
	}

	// Multi-key case: use MuSig2 aggregation with taproot tweak.
	aggKey, _, _, err := musig2.AggregateKeys(
		cosigners, true,
		musig2.WithTaprootKeyTweak(sweepTapscriptRoot),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to aggregate keys: %w", err)
	}

	return aggKey.FinalKey, nil
}

// fundingAmount returns the BTC funding amount from asset metadata if present.
func fundingAmount(meta *AssetMetadata) btcutil.Amount {
	if meta == nil {
		return 0
	}

	return meta.Funding.Amount
}

// proofBytes returns the input proof blob from asset metadata if present.
func proofBytes(meta *AssetMetadata) []byte {
	if meta == nil || len(meta.InputProof) == 0 {
		return nil
	}

	proof := make([]byte, len(meta.InputProof))
	copy(proof, meta.InputProof)

	return proof
}

// aggregateBranchMetadata creates empty metadata for branch nodes.
// AssetProof remains nil for branches.
func aggregateBranchMetadata(groups [][]LeafDescriptor) *NodeMetadata {
	if len(groups) == 0 {
		return nil
	}

	return &NodeMetadata{}
}

// AddSignature sets the signature for this node's transaction.
func (n *Node) AddSignature(sig *schnorr.Signature) {
	n.Signature = sig
}

// SetChildren sets the children map for this node, replacing any existing
// children. This is a convenience method for constructing trees.
func (n *Node) SetChildren(children map[uint32]*Node) {
	n.Children = children
}

// ToTx converts the Node into its unsigned wire.MsgTx representation.
func (n *Node) ToTx() (*wire.MsgTx, error) {
	// Virtual transactions are V3 since they use ephemeral anchors.
	tx := wire.NewMsgTx(3)

	// Add the single, unsigned input.
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: n.Input,
		Sequence:         wire.MaxTxInSequenceNum - 2,
	})

	// Add all outputs.
	for _, output := range n.Outputs {
		tx.AddTxOut(output)
	}

	return tx, nil
}

// ToSignedTx converts the Node into its signed wire.MsgTx representation.
// This requires that the node has a signature set.
func (n *Node) ToSignedTx() (*wire.MsgTx, error) {
	if n.Signature == nil {
		return nil, fmt.Errorf("cannot create signed " +
			"transaction: no signature present")
	}

	// Start with the unsigned transaction.
	tx, err := n.ToTx()
	if err != nil {
		return nil, fmt.Errorf("failed to create base "+
			"transaction: %w", err)
	}

	// Add the signature as witness data for the single input.
	// For taproot keyspend, the witness consists of just the signature.
	if len(tx.TxIn) != 1 {
		return nil, fmt.Errorf("expected exactly 1 input, got %d",
			len(tx.TxIn))
	}

	tx.TxIn[0].Witness = wire.TxWitness{
		n.Signature.Serialize(),
	}

	return tx, nil
}

// TXID computes the transaction ID of the transaction represented by this
// Node.
func (n *Node) TXID() (chainhash.Hash, error) {
	tx, err := n.ToTx()
	if err != nil {
		return chainhash.Hash{}, fmt.Errorf(
			"failed to create transaction: %w", err,
		)
	}

	return tx.TxHash(), nil
}

// IsLeaf returns true if this node is a leaf (has no children).
func (n *Node) IsLeaf() bool {
	return len(n.Children) == 0
}

// NodesIter returns an iterator over all nodes in the tree in depth-first
// pre-order (current node, then children).
func (n *Node) NodesIter() iter.Seq[*Node] {
	return func(yield func(*Node) bool) {
		n.forEach(yield)
	}
}

// GetLeafNodes returns all leaf nodes from this tree.
func (n *Node) GetLeafNodes() []*Node {
	var leaves []*Node

	for leaf := range n.LeavesIter() {
		leaves = append(leaves, leaf)
	}

	return leaves
}

// forEach is the internal implementation of iteration.
func (n *Node) forEach(yield func(*Node) bool) bool {
	// Yield current node.
	if !yield(n) {
		return false
	}

	// Recursively yield children in sorted order for determinism.
	indices := sortedChildIndices(n.Children)

	for _, idx := range indices {
		if !n.Children[idx].forEach(yield) {
			return false
		}
	}

	return true
}

// ForEach traverses the tree and applies the given callback function to each
// node. The traversal is done in depth-first order (pre-order: current node,
// then children). If the callback returns an error, traversal stops and the
// error is returned.
func (n *Node) ForEach(callback func(*Node) error) error {
	// Apply callback to current node.
	if err := callback(n); err != nil {
		return err
	}

	// Recursively apply to children in sorted order for determinism.
	indices := sortedChildIndices(n.Children)

	for _, idx := range indices {
		if err := n.Children[idx].ForEach(callback); err != nil {
			return err
		}
	}

	return nil
}

// LeavesIter returns an iterator over all leaf nodes in the tree.
func (n *Node) LeavesIter() iter.Seq[*Node] {
	return func(yield func(*Node) bool) {
		n.forEachLeaf(yield)
	}
}

// forEachLeaf is the internal implementation of leaf iteration.
func (n *Node) forEachLeaf(yield func(*Node) bool) bool {
	// If this is a leaf node, yield it.
	if n.IsLeaf() {
		return yield(n)
	}

	// Recursively yield children's leaves in sorted order for determinism.
	indices := sortedChildIndices(n.Children)

	for _, idx := range indices {
		if !n.Children[idx].forEachLeaf(yield) {
			return false
		}
	}

	return true
}

// ForEachLeaf traverses the tree and applies the given callback function to
// each leaf node only. If the callback returns an error, traversal stops and
// the error is returned.
func (n *Node) ForEachLeaf(callback func(*Node) error) error {
	// If this is a leaf node, apply callback.
	if n.IsLeaf() {
		return callback(n)
	}

	// Recursively apply to children in sorted order for determinism.
	indices := sortedChildIndices(n.Children)

	for _, idx := range indices {
		if err := n.Children[idx].ForEachLeaf(callback); err != nil {
			return err
		}
	}

	return nil
}

// sortedChildIndices returns child indexes sorted ascending to guarantee
// deterministic traversal for callers.
func sortedChildIndices(children map[uint32]*Node) []uint32 {
	indices := make([]uint32, 0, len(children))
	for idx := range children {
		indices = append(indices, idx)
	}

	sort.Slice(indices, func(i, j int) bool {
		return indices[i] < indices[j]
	})

	return indices
}

// Depth returns the maximum depth of the tree. A single node (leaf) has depth
// 1, a node with children has depth 1 + max depth of children.
func (n *Node) Depth() int {
	if n.IsLeaf() {
		return 1
	}

	maxChildDepth := 0
	for _, child := range n.Children {
		childDepth := child.Depth()
		if childDepth > maxChildDepth {
			maxChildDepth = childDepth
		}
	}

	return 1 + maxChildDepth
}

// NumTx returns the total number of transactions in the tree. Each Node
// represents one transaction, so this counts all nodes in the tree.
func (n *Node) NumTx() int {
	// Count this node.
	count := 1

	// Add counts from children.
	for _, child := range n.Children {
		count += child.NumTx()
	}

	return count
}

// ExtractPathForCoSigner takes a Node and extracts the path that is relevant
// for a given cosigner. It returns a new Node that contains only the nodes and
// children where the cosigner's key is present in the CoSigners list from root
// to leaf.
func (n *Node) ExtractPathForCoSigner(targetKey *btcec.PublicKey) *Node {
	// Check if the target key is in this node's cosigners.
	if !ContainsCosigner(n.CoSigners, targetKey) {
		return nil
	}

	// Create a new node with the same basic info.
	extracted := &Node{
		Input:        n.Input,
		CoSigners:    n.CoSigners,
		Outputs:      n.Outputs,
		Signature:    n.Signature,
		FinalKey:     n.FinalKey,
		TaprootTweak: n.TaprootTweak,
		Children:     make(map[uint32]*Node),
		Metadata:     n.Metadata,
	}

	// Recursively extract relevant children.
	for outputIndex, child := range n.Children {
		extractedChild := child.ExtractPathForCoSigner(targetKey)
		if extractedChild != nil {
			extracted.Children[outputIndex] = extractedChild
		}
	}

	return extracted
}

// ExtractPathForIndex extracts the path to a specific leaf by its index.
// Returns nil if the index is out of bounds.
func (n *Node) ExtractPathForIndex(targetIndex int) (*Node, error) {
	if targetIndex < 0 {
		return nil, fmt.Errorf("leaf index must be non-negative, "+
			"got %d", targetIndex)
	}

	// First, count total leaves to validate index.
	totalLeaves := n.countLeaves()
	if targetIndex >= totalLeaves {
		return nil, nil // Index out of bounds, return nil (no error).
	}

	// Find the path to the target leaf.
	extracted, _ := n.extractPathForIndexRecursive(targetIndex, 0)

	return extracted, nil
}

// countLeaves returns the total number of leaf nodes in this subtree.
func (n *Node) countLeaves() int {
	if n.IsLeaf() {
		return 1
	}

	count := 0
	for _, child := range n.Children {
		count += child.countLeaves()
	}

	return count
}

// extractPathForIndexRecursive recursively extracts the path to the
// target leaf index. Returns the extracted node and the number of leaves
// consumed in this subtree.
func (n *Node) extractPathForIndexRecursive(
	targetIndex, currentIndex int) (*Node, int) {
	// If this is a leaf node.
	if n.IsLeaf() {
		if currentIndex == targetIndex {
			// Found our target leaf.
			return &Node{
				Input:        n.Input,
				CoSigners:    n.CoSigners,
				Outputs:      n.Outputs,
				Signature:    n.Signature,
				FinalKey:     n.FinalKey,
				TaprootTweak: n.TaprootTweak,
				Children:     make(map[uint32]*Node),
			}, currentIndex + 1
		}

		return nil, currentIndex + 1
	}

	// This is a branch node, check children.
	extracted := &Node{
		Input:        n.Input,
		CoSigners:    n.CoSigners,
		Outputs:      n.Outputs,
		Signature:    n.Signature,
		FinalKey:     n.FinalKey,
		TaprootTweak: n.TaprootTweak,
		Children:     make(map[uint32]*Node),
	}

	leafIndex := currentIndex
	foundTarget := false

	// Process children in sorted order for consistent indexing.
	for i := uint32(0); i < uint32(len(n.Outputs)-1); i++ {
		child, exists := n.Children[i]
		if !exists {
			continue
		}

		childExtracted, newLeafIndex :=
			child.extractPathForIndexRecursive(
				targetIndex, leafIndex,
			)

		if childExtracted != nil {
			extracted.Children[i] = childExtracted
			foundTarget = true
		}

		leafIndex = newLeafIndex
	}

	if foundTarget {
		return extracted, leafIndex
	}

	return nil, leafIndex
}

// PrevOutputFetcher creates a PrevOutputFetcher that can provide transaction
// outputs for all transactions in the tree, starting with the initial previous
// output that the root transaction spends.
func (n *Node) PrevOutputFetcher(initialPrevOut *wire.TxOut) (
	txscript.PrevOutputFetcher, error) {

	outputs := make(map[wire.OutPoint]*wire.TxOut)

	// Add the initial previous output that the root spends.
	outputs[n.Input] = initialPrevOut

	// Walk the tree and collect all transaction outputs.
	for node := range n.NodesIter() {
		// Get this node's transaction hash.
		txHash, err := node.TXID()
		if err != nil {
			return nil, fmt.Errorf("failed to get transaction ID "+
				"for node: %w", err)
		}

		// Add all outputs from this transaction.
		for i, output := range node.Outputs {
			outpoint := wire.OutPoint{
				Hash:  txHash,
				Index: uint32(i),
			}
			outputs[outpoint] = output
		}
	}

	return txscript.NewMultiPrevOutFetcher(outputs), nil
}

// GetLeafForCoSigner returns the leaf Node for a specific cosigner.
// Returns nil if no leaf is found for the cosigner.
func (n *Node) GetLeafForCoSigner(targetKey *btcec.PublicKey) *Node {
	for leaf := range n.LeavesIter() {
		if ContainsCosigner(leaf.CoSigners, targetKey) {
			return leaf
		}
	}

	return nil
}

// GetNonAnchorOutpoint returns the outpoint for the non-anchor output of this
// leaf node. Leaf nodes have exactly 2 outputs: one VTXO/connector output and
// one anchor output. This method returns the outpoint for the non-anchor output
// by checking which output is not the anchor script.
func (n *Node) GetNonAnchorOutpoint() (*wire.OutPoint, error) {
	// Verify this is actually a leaf node.
	if !n.IsLeaf() {
		return nil, fmt.Errorf("node is not a leaf (has %d children)",
			len(n.Children))
	}

	// Get the transaction hash for this leaf.
	txHash, err := n.TXID()
	if err != nil {
		return nil, fmt.Errorf("failed to get transaction ID: %w",
			err)
	}

	// Get the anchor script to compare against.
	anchorScript := scripts.AnchorOutput().PkScript

	// Find the non-anchor output (the one that is NOT the anchor script).
	for i, output := range n.Outputs {
		if !bytes.Equal(output.PkScript, anchorScript) {
			return &wire.OutPoint{
				Hash:  txHash,
				Index: uint32(i),
			}, nil
		}
	}

	return nil, fmt.Errorf("no non-anchor output found in leaf node")
}

// Verify recursively verifies that the node structure is consistent.
func (n *Node) Verify() error {
	// Get this node's txid.
	txHash, err := n.TXID()
	if err != nil {
		return fmt.Errorf("failed to create transaction for node: %w",
			err)
	}

	// Verify each child points to the correct parent output.
	for outputIndex, child := range n.Children {
		// Verify output index exists in parent's outputs.
		if int(outputIndex) >= len(n.Outputs) {
			return fmt.Errorf("child references non-existent "+
				"output index %d (parent has %d outputs)",
				outputIndex, len(n.Outputs))
		}

		// Check child's input references this node's transaction.
		expectedOutPoint := wire.OutPoint{
			Hash:  txHash,
			Index: outputIndex,
		}

		if child.Input != expectedOutPoint {
			return fmt.Errorf("child at output index %d has "+
				"incorrect input: got %s, expected %s",
				outputIndex, child.Input, expectedOutPoint)
		}

		// Recursively verify children.
		if err := child.Verify(); err != nil {
			return fmt.Errorf("child at output index %d failed "+
				"verification: %w", outputIndex, err)
		}
	}

	return nil
}

// VerifySigned verifies that all nodes in the tree have valid signatures.
func (n *Node) VerifySigned(prevOutFetcher txscript.PrevOutputFetcher) error {
	return n.ForEach(func(node *Node) error {
		// Check that signature is present.
		if node.Signature == nil {
			txHash, _ := node.TXID()
			return fmt.Errorf("no signature found for "+
				"transaction %s", txHash.String())
		}

		// Verify the signature.
		return node.verifyNodeSignature(prevOutFetcher)
	})
}

// verifyNodeSignature verifies the signature for a single node.
func (n *Node) verifyNodeSignature(
	prevOutFetcher txscript.PrevOutputFetcher) error {

	// Sanity check: FinalKey should always be set by constructors.
	if n.FinalKey == nil {
		return fmt.Errorf("node has no FinalKey set; must be " +
			"constructed with NewLeafNode or NewBranchNode")
	}

	// Create the transaction to verify.
	tx, err := n.ToTx()
	if err != nil {
		return fmt.Errorf("failed to create transaction: %w", err)
	}

	// Calculate the signature hash.
	sigHash, err := txscript.CalcTaprootSignatureHash(
		txscript.NewTxSigHashes(tx, prevOutFetcher),
		txscript.SigHashDefault, tx, 0, prevOutFetcher,
	)
	if err != nil {
		return fmt.Errorf("failed to calculate signature hash: %w",
			err)
	}

	// Verify the signature against the cached final key.
	if !n.Signature.Verify(sigHash, n.FinalKey) {
		return fmt.Errorf("signature verification failed for "+
			"transaction %s", tx.TxHash())
	}

	return nil
}

// ContainsCosigner checks if a target key is present in the cosigners list.
func ContainsCosigner(cosigners []*btcec.PublicKey,
	targetKey *btcec.PublicKey) bool {

	for _, cosigner := range cosigners {
		if targetKey.IsEqual(cosigner) {
			return true
		}
	}

	return false
}

// PrettyPrint returns a human-readable string representation of the tree
// structure with transaction IDs, amounts, and cosigner information.
func (n *Node) PrettyPrint() string {
	var result string
	result += "=== Transaction Tree ===\n\n"

	n.printNode(&result, "", true, make(map[string]string), 0)

	return result
}

// printNode recursively prints the node structure with indentation.
func (n *Node) printNode(result *string, prefix string, isLast bool,
	keyAliases map[string]string, counter int) {

	if n == nil {
		return
	}

	// Get TXID.
	txid, _ := n.TXID()
	txidStr := txid.String()[:8]

	// Calculate total amount (excluding anchor).
	var totalAmount int64
	for i, output := range n.Outputs {
		// Skip last output if it's anchor (value 0).
		if i == len(n.Outputs)-1 && output.Value == 0 {
			continue
		}
		totalAmount += output.Value
	}

	// Format cosigners with aliases.
	var cosignerStr string
	for i, cosigner := range n.CoSigners {
		keyHex := cosigner.SerializeCompressed()[0:8]
		alias, exists := keyAliases[string(keyHex)]
		if !exists {
			alias = fmt.Sprintf("K%d", len(keyAliases))
			keyAliases[string(keyHex)] = alias
		}
		if i > 0 {
			cosignerStr += ","
		}
		cosignerStr += alias
	}

	// Determine node type.
	nodeType := "Branch"
	if n.IsLeaf() {
		nodeType = "Leaf"
	}

	// Print connector.
	connector := "├── "
	if isLast {
		connector = "└── "
	}

	*result += fmt.Sprintf("%s%s [%s] %s (%d sats) [%s]\n",
		prefix, connector, txidStr, nodeType, totalAmount, cosignerStr)

	// Print children.
	if len(n.Children) > 0 {
		childPrefix := prefix
		if isLast {
			childPrefix += "    "
		} else {
			childPrefix += "│   "
		}

		// Get sorted indices.
		indices := sortedChildIndices(n.Children)

		for i, idx := range indices {
			child := n.Children[idx]
			childIsLast := i == len(indices)-1
			child.printNode(result, childPrefix, childIsLast,
				keyAliases, counter)
		}
	}
}

// UniqueCosigners removes duplicate cosigner keys while preserving order.
func UniqueCosigners(cosigners []*btcec.PublicKey) []*btcec.PublicKey {
	seen := make(map[[33]byte]struct{}, len(cosigners))
	unique := make([]*btcec.PublicKey, 0, len(cosigners))

	for _, cosigner := range cosigners {
		var keyArr [33]byte
		copy(keyArr[:], schnorr.SerializePubKey(cosigner))

		if _, exists := seen[keyArr]; !exists {
			seen[keyArr] = struct{}{}
			unique = append(unique, cosigner)
		}
	}

	return unique
}
