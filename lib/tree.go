package lib

import (
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/fn"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

// TreeNode represents a node in a transaction tree. It is made up of a
// transaction along with the children transactions that spend its outputs.
type TreeNode struct {
	// Input is the output being spent by this tree node transaction.
	// This defines the single input of the transaction at this node.
	Input *wire.OutPoint

	// CoSigners is the set of keys involved in the musig2 signing process
	// of the keyspend path for the input of this transaction.
	CoSigners []*btcec.PublicKey

	// Outputs are the outputs created by the transaction at this node.
	// - For leaves:
	//	- In a VTXO tree: the outputs will be VTXO scripts along with a
	//       single zero fee anchor output.
	//	- In a connector tree: the outputs will be a dust connector
	// 	 output that pays directly to the server key along with a single
	// 	 zero fee anchor output.
	// - For branches: the outputs will be the inputs to child transactions
	//   along with a single zero fee anchor output.
	Outputs []*wire.TxOut

	// Children maps output indices to child trees. Each child is a subtree
	// which spends the output at that index in the Root transaction.
	// If there are no children, then this is a leaf node and so the outputs
	// of the root transaction are VTXOs
	Children map[uint32]*TreeNode

	// Signature is the final musig2 signature for this transaction's input.
	// This is populated after the multi-party signing process is complete.
	Signature *schnorr.Signature
}

// Tree represents a complete transaction tree with all the context needed
// for signing operations. It wraps a TreeNode root with additional metadata
// required for musig2 signing sessions.
type Tree struct {
	// Root is the root TreeNode of the transaction tree
	Root *TreeNode

	// SweepKey is the public key used in the sweep script tweak
	SweepKey *btcec.PublicKey

	// SweepDelay is the CSV delay used in the sweep script
	SweepDelay uint32

	// PrevOut is the previous output that the root transaction spends
	PrevOut *wire.TxOut
}

// NewTreeSignerSession creates a new TreeSignerSession for this tree with the
// given wallet and signer key. This encapsulates all the setup logic that was
// previously duplicated between client and server.
func (t *Tree) NewTreeSignerSession(wallet input.MuSig2Signer,
	signerKeyDesc *keychain.KeyDescriptor) (*TreeSignerSession, error) {

	// Create the sweep tap leaf
	sweepTapLeaf, err := UnilateralCSVTimeoutTapLeaf(
		t.SweepKey, t.SweepDelay,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create sweep tap leaf: %w",
			err)
	}

	// Create the prev output fetcher
	prevOutFetcher, err := t.Root.PrevOutputFetcher(t.PrevOut)
	if err != nil {
		return nil, fmt.Errorf("failed to create prev output "+
			"fetcher: %w", err)
	}

	// Create the tree signer session
	return NewTreeSignerSession(
		wallet, signerKeyDesc, sweepTapLeaf.Script,
		prevOutFetcher, t.Root,
	)
}

// NewTreeSignerCoordinator creates a new TreeSignerCoordinator for this tree.
// This encapsulates the setup logic for the coordinator.
func (t *Tree) NewTreeSignerCoordinator() (*TreeSignerCoordinator, error) {
	// Create the sweep tap leaf
	sweepTapLeaf, err := UnilateralCSVTimeoutTapLeaf(
		t.SweepKey, t.SweepDelay,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create sweep tap leaf: %w",
			err)
	}

	// Create the prev output fetcher
	prevOutFetcher, err := t.PrevOutputFetcher()
	if err != nil {
		return nil, fmt.Errorf("failed to create prev output "+
			"fetcher: %w", err)
	}

	// Create the tree signer coordinator
	return NewTreeSignerCoordinator(
		t.Root, sweepTapLeaf.Script, prevOutFetcher,
	)
}

// ExtractPathForCosigner extracts the path relevant for a given cosigner and returns a new Tree
// containing only the nodes where the cosigner's key is present.
func (t *Tree) ExtractPathForCosigner(targetKey *btcec.PublicKey) *Tree {
	extractedRoot := t.Root.ExtractPathForCosigner(targetKey)
	if extractedRoot == nil {
		return nil
	}

	return &Tree{
		Root:       extractedRoot,
		SweepKey:   t.SweepKey,
		SweepDelay: t.SweepDelay,
		PrevOut:    t.PrevOut,
	}
}

// Delegation methods to TreeNode for convenience

// Verify verifies the tree structure
func (t *Tree) Verify() error {
	return t.Root.Verify()
}

// PrevOutputFetcher creates a PrevOutputFetcher for all transactions in the tree
func (t *Tree) PrevOutputFetcher() (txscript.PrevOutputFetcher, error) {
	return t.Root.PrevOutputFetcher(t.PrevOut)
}

// ForEach traverses the tree and applies the callback to each node
func (t *Tree) ForEach(callback func(*TreeNode) error) error {
	return t.Root.ForEach(callback)
}

// ForEachLeaf traverses the tree and applies the callback to each leaf node
func (t *Tree) ForEachLeaf(callback func(*TreeNode) error) error {
	return t.Root.ForEachLeaf(callback)
}

// GetLeafNodes returns all leaf TreeNodes from the tree
func (t *Tree) GetLeafNodes() ([]*TreeNode, error) {
	return t.Root.GetLeafNodes()
}

// GetLeafForCosigner returns the leaf TreeNode for a specific cosigner
func (t *Tree) GetLeafForCosigner(targetKey *btcec.PublicKey) (*TreeNode, error) {
	return t.Root.GetLeafForCosigner(targetKey)
}

// ExtractPathForIndex extracts the path to a specific leaf by its index.
// This is useful for connector trees where all leaves have the same cosigner key,
// but clients need access to a specific leaf by index.
func (t *Tree) ExtractPathForIndex(leafIndex int) (*Tree, error) {
	extractedRoot, err := t.Root.ExtractPathForIndex(leafIndex)
	if err != nil {
		return nil, err
	}
	if extractedRoot == nil {
		return nil, nil
	}

	return &Tree{
		Root:       extractedRoot,
		SweepKey:   t.SweepKey,
		SweepDelay: t.SweepDelay,
		PrevOut:    t.PrevOut,
	}, nil
}

// SubmitTreeSigs stores signatures in the tree nodes
func (t *Tree) SubmitTreeSigs(sigs map[string]*schnorr.Signature) error {
	return t.Root.SubmitTreeSigs(sigs)
}

// VerifySigned verifies that all nodes in the tree have valid signatures
func (t *Tree) VerifySigned() error {
	sweepTapLeaf, err := UnilateralCSVTimeoutTapLeaf(
		t.SweepKey, t.SweepDelay,
	)
	if err != nil {
		return fmt.Errorf("failed to create sweep tap leaf: %w", err)
	}

	prevOutFetcher, err := t.PrevOutputFetcher()
	if err != nil {
		return fmt.Errorf("failed to create prev output fetcher: %w", err)
	}

	return t.Root.VerifySigned(
		sweepTapLeaf.Script, prevOutFetcher,
	)
}

// PrettyPrint returns a visual representation of the tree
func (t *Tree) PrettyPrint() string {
	return t.Root.PrettyPrint()
}

// ToTx converts the TreeNode into its unsigned wire.MsgTx representation.
func (n *TreeNode) ToTx() (*wire.MsgTx, error) {
	// Virtual transactions are V3 since they use ephemeral anchors.
	tx := wire.NewMsgTx(3)

	// Add the single, unsigned input.
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: *n.Input,
		Sequence:         wire.MaxTxInSequenceNum,
	})

	// Add all outputs.
	for _, output := range n.Outputs {
		tx.AddTxOut(output)
	}

	return tx, nil
}

// TXID computes the transaction ID of the transaction represented by this
// TreeNode.
func (n *TreeNode) TXID() (chainhash.Hash, error) {
	tx, err := n.ToTx()
	if err != nil {
		return chainhash.Hash{},
			fmt.Errorf("failed to create transaction: %w", err)
	}

	return tx.TxHash(), nil
}

// Leaf defines the details of a leaf node in the transaction tree.
type Leaf struct {
	// PkScript is the output script of the VTXO. The details of this
	// script, other than the actual value itself, is not relevant for the
	// tree computation.
	PkScript []byte

	// Amount is the amount locked in the PkScript.
	Amount int64

	// SignerKey is the public key that the VTXO owner (the user) will use
	// during the musig2 signing process of the vtx tree for each node in
	// the tree that leads to this leaf.
	SignerKey *btcec.PublicKey
}

func BuildConnectorTree(
	input *wire.OutPoint,
	numLeaves int,
	dustAmount btcutil.Amount,
	connectorKey *btcec.PublicKey,
	radix int,
	prevOut *wire.TxOut,
) (*Tree, error) {

	tapScript, err := txscript.PayToTaprootScript(connectorKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create connector "+
			"pkScript: %w", err)
	}

	leaves := make([]Leaf, numLeaves)
	for i := 0; i < numLeaves; i++ {
		leaves[i] = Leaf{
			PkScript:  tapScript,
			Amount:    int64(dustAmount),
			SignerKey: connectorKey,
		}
	}

	root, err := buildTree(input, leaves, radix, nil, fn.None[*btcec.PublicKey]())
	if err != nil {
		return nil, err
	}

	return &Tree{
		Root:       root,
		SweepKey:   nil, // Connector trees don't have sweep keys
		SweepDelay: 0,   // Connector trees don't have sweep delays
		PrevOut:    prevOut,
	}, nil
}

func BuildVTXOTree(
	// input is the outpoint in the commitment transaction that the
	// root transaction will spend.
	input *wire.OutPoint,
	// leaves is the list of leaves that define the VTXOs at the leaves of
	// the tree.
	leaves []Leaf,
	// sweepCSVDelay is the CSV delay to use in the sweep script tweak.
	sweepCSVDelay uint32,
	// operatorSweepKey is the public key that the operator will use
	// in the sweep script tweak.
	operatorSweepKey,
	// operatorSigningKey is the public key that the operator will use
	// during the musig2 signing process of the vtx tree for each node.
	operatorSigningKey *btcec.PublicKey,
	// radix is the branching factor of the tree.
	radix int,
	// prevOut is the previous output that this tree's root will spend.
	prevOut *wire.TxOut,
) (*Tree, error) {

	if radix < 2 {
		return nil, fmt.Errorf("radix must be at least 2")
	}

	// Compute the sweep tap leaf. This will be applied as a tweak to all
	// the batch outputs along with all the (non-leaf) VTX outputs.
	sweepTapLeaf, err := UnilateralCSVTimeoutTapLeaf(
		operatorSweepKey, sweepCSVDelay,
	)
	if err != nil {
		return nil, err
	}

	root, err := buildTree(
		input, leaves, radix, sweepTapLeaf.Script,
		fn.Some(operatorSigningKey),
	)
	if err != nil {
		return nil, err
	}

	return &Tree{
		Root:       root,
		SweepKey:   operatorSweepKey,
		SweepDelay: sweepCSVDelay,
		PrevOut:    prevOut,
	}, nil
}

func buildTree(input *wire.OutPoint, leaves []Leaf, radix int,
	tweak []byte, extraCosigner fn.Option[*btcec.PublicKey]) (*TreeNode,
	error) {

	if len(leaves) == 0 {
		return nil, fmt.Errorf("empty leaves")
	}

	// Sort receivers by amount (descending) using LPT heuristic for better
	// balance.
	indexed := make([]indexedLeaf, len(leaves))
	for i, r := range leaves {
		indexed[i] = indexedLeaf{
			Leaf:          r,
			originalIndex: i,
		}
	}
	sort.Slice(indexed, func(i, j int) bool {
		// Sort by amount descending, use original index as tiebreaker
		// for stability.
		if indexed[i].Amount != indexed[j].Amount {
			return indexed[i].Amount > indexed[j].Amount
		}
		return indexed[i].originalIndex < indexed[j].originalIndex
	})

	sortedLeaves := make([]Leaf, len(indexed))
	for i, il := range indexed {
		sortedLeaves[i] = il.Leaf
	}

	// Initialize queue with root work item
	queue := []workItem{{
		input:    input,
		leaves:   sortedLeaves,
		parent:   nil,
		outIndex: 0,
	}}

	var root *TreeNode

	// Process queue iteratively (BFS).
	for len(queue) > 0 {
		work := queue[0]
		queue = queue[1:]

		// Base case: single receiver creates a leaf transaction
		if len(work.leaves) == 1 {
			tree, err := createLeafTx(
				work.input,
				work.leaves[0],
				extraCosigner,
			)
			if err != nil {
				return nil, fmt.Errorf("failed to create "+
					"leaf tx: %w", err)
			}

			if work.parent != nil {
				work.parent.Children[work.outIndex] = tree
			} else {
				root = tree
			}

			continue
		}

		// Partition receivers into balanced groups.
		groups := partitionReceivers(work.leaves, radix)

		// Create branch transaction
		tree, err := createBranchTx(
			work.input, groups, tweak, extraCosigner,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to create branch tx: %w", err)
		}

		if work.parent != nil {
			work.parent.Children[work.outIndex] = tree
		} else {
			root = tree
		}

		// Enqueue children for processing
		for i, group := range groups {
			if len(group) == 0 {
				continue
			}

			// Get the transaction hash of the current tree node
			parentTx, err := tree.ToTx()
			if err != nil {
				return nil, fmt.Errorf("failed to "+
					"create parent tx: %w", err)
			}
			parentTxHash := parentTx.TxHash()

			childInput := &wire.OutPoint{
				Hash:  parentTxHash,
				Index: uint32(i),
			}
			queue = append(queue, workItem{
				input:    childInput,
				leaves:   group,
				parent:   tree,
				outIndex: uint32(i),
			})
		}
	}

	return root, nil
}

// createLeafTx creates a leaf virtual transaction. This transaction has:
// - one input: the provided outpoint.
// - two outputs: the vtxo/connector output and the anchor output.
func createLeafTx(input *wire.OutPoint, leaf Leaf,
	extraSigningKey fn.Option[*btcec.PublicKey]) (*TreeNode, error) {

	// The cosigners for a leaf are the leaf owner signing
	// key along with an optional extra collaborative key.
	cosigners := []*btcec.PublicKey{
		leaf.SignerKey,
	}

	extraSigningKey.WhenSome(func(key *btcec.PublicKey) {
		cosigners = append(cosigners, key)
	})

	return &TreeNode{
		Input: input,
		Outputs: []*wire.TxOut{
			// The first output is the spendable output.
			wire.NewTxOut(leaf.Amount, leaf.PkScript),

			// The zero value ephemeral anchor output.
			AnchorOutput(),
		},
		CoSigners: cosigners,
		Children:  make(map[uint32]*TreeNode),
	}, nil
}

// createBranchTx creates a branch transaction with outputs for each group of
// leaves.
func createBranchTx(input *wire.OutPoint, groups [][]Leaf,
	sweepTweek []byte, extraSignKey fn.Option[*btcec.PublicKey]) (*TreeNode,
	error) {

	outputs := make([]*wire.TxOut, 0, len(groups)+1)
	allCosigners := make([]*btcec.PublicKey, 0)

	if extraSignKey.IsSome() != (len(sweepTweek) > 0) {
		return nil, fmt.Errorf("extra signing key and sweep " +
			"tweak must both be present or both be absent")
	}

	// Each group will become an output.
	for _, group := range groups {
		// Calculate total amount and collect cosigners for this group.
		var (
			amount         = int64(0)
			groupCosigners []*btcec.PublicKey
		)
		extraSignKey.WhenSome(func(key *btcec.PublicKey) {
			groupCosigners = append(groupCosigners, key)
		})

		for _, leaf := range group {
			amount += leaf.Amount
			groupCosigners = append(groupCosigners, leaf.SignerKey)
		}

		groupCosigners = uniqueCosigners(groupCosigners)
		var tapKey *btcec.PublicKey
		if len(groupCosigners) > 1 {
			// Aggregate keys for this group's output and tweak it
			// with the sweep script tweak.
			aggKey, _, _, err := musig2.AggregateKeys(
				groupCosigners, true,
				musig2.WithTaprootKeyTweak(sweepTweek),
			)
			if err != nil {
				return nil, fmt.Errorf("failed to aggregate "+
					"keys: %w", err)
			}

			tapKey = aggKey.FinalKey
		} else {
			tapKey = groupCosigners[0]
		}

		inputScript, err := txscript.PayToTaprootScript(tapKey)
		if err != nil {
			return nil, fmt.Errorf("failed to create script "+
				"pubkey: %w", err)
		}

		outputs = append(outputs, &wire.TxOut{
			Value:    amount,
			PkScript: inputScript,
		})

		allCosigners = append(allCosigners, groupCosigners...)
	}

	// Add anchor output.
	outputs = append(outputs, AnchorOutput())

	// Use all unique cosigners for the branch transaction.
	allCosigners = uniqueCosigners(allCosigners)

	return &TreeNode{
		Input:     input,
		Outputs:   outputs,
		CoSigners: allCosigners,
		Children:  make(map[uint32]*TreeNode),
	}, nil
}

// indexedLeaf wraps a Leaf with its original index for stable sorting.
type indexedLeaf struct {
	Leaf
	originalIndex int
}

// workItem represents a unit of work in the tree building queue
type workItem struct {
	input    *wire.OutPoint
	leaves   []Leaf
	parent   *TreeNode
	outIndex uint32
}

// partitionReceivers divides receivers into balanced groups using round-robin
// assignment. It ensures even distribution of items across groups to create a
// balanced tree.
func partitionReceivers(receivers []Leaf, radix int) [][]Leaf {
	M := len(receivers)

	if M <= radix {
		// Each receiver gets its own group
		groups := make([][]Leaf, M)
		for i, r := range receivers {
			groups[i] = []Leaf{r}
		}
		return groups
	}

	// Calculate target sizes: distribute M items into radix groups.
	base := M / radix
	extra := M % radix
	sizes := make([]int, radix)
	for i := 0; i < radix; i++ {
		sizes[i] = base
		if i < extra {
			sizes[i]++ // First 'extra' groups get one more item.
		}
	}

	// Round-robin assignment with capacity tracking
	groups := make([][]Leaf, radix)
	for i := range groups {
		groups[i] = make([]Leaf, 0, sizes[i])
	}
	caps := make([]int, radix)
	copy(caps, sizes)

	idx := 0
	for _, receiver := range receivers {
		// Find next group with capacity (cyclic)
		for caps[idx] == 0 {
			idx = (idx + 1) % radix
		}
		groups[idx] = append(groups[idx], receiver)
		caps[idx]--
		idx = (idx + 1) % radix
	}

	// Safety: ensure at least 2 non-empty groups when M > 1
	nonEmpty := make([][]Leaf, 0, radix)
	for _, g := range groups {
		if len(g) > 0 {
			nonEmpty = append(nonEmpty, g)
		}
	}

	if len(nonEmpty) <= 1 && M > 1 {
		// Fallback: split in half to guarantee progress
		mid := M / 2
		return [][]Leaf{receivers[:mid], receivers[mid:]}
	}

	return nonEmpty
}

// uniqueCosigners removes duplicate cosigner keys while preserving order
func uniqueCosigners(cosigners []*btcec.PublicKey) []*btcec.PublicKey {
	seen := make(map[string]struct{})
	unique := make([]*btcec.PublicKey, 0, len(cosigners))

	for _, cosigner := range cosigners {
		keyStr := hex.EncodeToString(schnorr.SerializePubKey(cosigner))
		if _, exists := seen[keyStr]; !exists {
			seen[keyStr] = struct{}{}
			unique = append(unique, cosigner)
		}
	}
	return unique
}

// ExtractPathForCosigner takes a TreeNode and extracts the path that is
// relevant for a given Cosigner. It returns a new TreeNode that contains only
// the nodes and children where the cosigner's key is present in the CoSigners
// list from root to leaf.
func (n *TreeNode) ExtractPathForCosigner(targetKey *btcec.PublicKey) *TreeNode {
	if n == nil {
		return nil
	}

	// Check if the target key is in this node's cosigners
	if !ContainsCosigner(n.CoSigners, targetKey) {
		return nil
	}

	// Create a new node with the same basic info
	extracted := &TreeNode{
		Input:     n.Input,
		CoSigners: n.CoSigners,
		Outputs:   n.Outputs,
		Children:  make(map[uint32]*TreeNode),
	}

	// Recursively extract relevant children
	for outputIndex, child := range n.Children {
		extractedChild := child.ExtractPathForCosigner(targetKey)
		if extractedChild != nil {
			extracted.Children[outputIndex] = extractedChild
		}
	}

	return extracted
}

// ContainsCosigner checks if a target key is present in the cosigners list
func ContainsCosigner(cosigners []*btcec.PublicKey,
	targetKey *btcec.PublicKey) bool {

	for _, cosigner := range cosigners {
		if targetKey.IsEqual(cosigner) {
			return true
		}
	}

	return false
}

// Depth returns the maximum depth of the tree. A single node (leaf) has depth 1,
// a node with children has depth 1 + max depth of children.
func (n *TreeNode) Depth() int {
	if n == nil {
		return 0
	}

	if len(n.Children) == 0 {
		return 1 // Leaf node has depth 1
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

// NumTx returns the total number of transactions in the tree. Each TreeNode
// represents one transaction, so this counts all nodes in the tree.
func (n *TreeNode) NumTx() int {
	if n == nil {
		return 0
	}

	count := 1 // Count this node
	for _, child := range n.Children {
		count += child.NumTx()
	}

	return count
}

// Verify recursively verifies that the tree structure is consistent.
// It checks that each child's input correctly references the parent's transaction
// at the expected output index.
func (n *TreeNode) Verify() error {
	if n == nil {
		return nil
	}

	// Get this node's txid.
	txHash, err := n.TXID()
	if err != nil {
		return fmt.Errorf("failed to create transaction for node: %w",
			err)
	}

	// Verify each child points to the correct parent output
	for outputIndex, child := range n.Children {
		if child == nil {
			continue
		}

		// First verify that the output index exists in parent's
		// outputs.
		if int(outputIndex) >= len(n.Outputs) {
			return fmt.Errorf("child references non-existent "+
				"output index %d (parent has %d outputs)",
				outputIndex, len(n.Outputs))
		}

		// Check that the child's input references this node's
		// transaction.
		expectedOutPoint := wire.OutPoint{
			Hash:  txHash,
			Index: outputIndex,
		}

		if *child.Input != expectedOutPoint {
			return fmt.Errorf("child at output index %d has "+
				"incorrect input: got %s, expected %s",
				outputIndex, child.Input, expectedOutPoint)
		}

		// Recursively verify children
		if err := child.Verify(); err != nil {
			return fmt.Errorf("child at output index %d failed "+
				"verification: %w", outputIndex, err)
		}
	}

	return nil
}

// ForEach traverses the tree and applies the given callback function to each
// node. The traversal is done in depth-first order (pre-order: current node,
// then children). If the callback returns an error, traversal stops and the
// error is returned.
func (n *TreeNode) ForEach(callback func(*TreeNode) error) error {
	if n == nil {
		return nil
	}

	// Apply callback to current node.
	if err := callback(n); err != nil {
		return err
	}

	// Recursively apply to children in sorted order for consistency.
	var outputIndices []uint32
	for idx := range n.Children {
		outputIndices = append(outputIndices, idx)
	}
	sort.Slice(outputIndices, func(i, j int) bool {
		return outputIndices[i] < outputIndices[j]
	})

	for _, idx := range outputIndices {
		if err := n.Children[idx].ForEach(callback); err != nil {
			return err
		}
	}

	return nil
}

// ForEachLeaf traverses the tree and applies the given callback function to
// each leaf node only. If the callback returns an error, traversal stops and
// the error is returned.
func (n *TreeNode) ForEachLeaf(callback func(*TreeNode) error) error {
	if n == nil {
		return nil
	}

	// If this is a leaf node, apply callback
	if len(n.Children) == 0 {
		return callback(n)
	}

	// Recursively apply to children
	var outputIndices []uint32
	for idx := range n.Children {
		outputIndices = append(outputIndices, idx)
	}
	sort.Slice(outputIndices, func(i, j int) bool {
		return outputIndices[i] < outputIndices[j]
	})

	for _, idx := range outputIndices {
		if err := n.Children[idx].ForEachLeaf(callback); err != nil {
			return err
		}
	}

	return nil
}

// GetLeafNodes returns all leaf TreeNodes from this tree node and its descendants
func (n *TreeNode) GetLeafNodes() ([]*TreeNode, error) {
	if n == nil {
		return nil, nil
	}

	var leafNodes []*TreeNode

	err := n.ForEachLeaf(func(leaf *TreeNode) error {
		leafNodes = append(leafNodes, leaf)
		return nil
	})

	if err != nil {
		return nil, err
	}

	return leafNodes, nil
}

// GetLeafForCosigner returns the leaf TreeNode for a specific cosigner.
// Returns nil if no leaf is found for the cosigner.
func (n *TreeNode) GetLeafForCosigner(targetKey *btcec.PublicKey) (*TreeNode, error) {
	if n == nil {
		return nil, nil
	}

	var foundLeaf *TreeNode

	err := n.ForEachLeaf(func(leaf *TreeNode) error {
		if ContainsCosigner(leaf.CoSigners, targetKey) {
			foundLeaf = leaf
			return fmt.Errorf("found") // Use error to break early
		}
		return nil
	})

	// If we found the leaf, the error will be "found"
	if err != nil && err.Error() == "found" {
		return foundLeaf, nil
	} else if err != nil {
		return nil, err
	}

	// No leaf found for this cosigner
	return nil, nil
}

// GetNonAnchorOutpoint returns the outpoint for the non-anchor output of this leaf node.
// Leaf nodes have exactly 2 outputs: one VTXO/connector output and one anchor output (value 0).
// This method returns the outpoint for the non-anchor output.
func (n *TreeNode) GetNonAnchorOutpoint() (*wire.OutPoint, error) {
	if n == nil {
		return nil, fmt.Errorf("cannot get outpoint from nil node")
	}

	// Verify this is actually a leaf node
	if len(n.Children) > 0 {
		return nil, fmt.Errorf("node is not a leaf (has %d children)", len(n.Children))
	}

	// Get the transaction hash for this leaf
	txHash, err := n.TXID()
	if err != nil {
		return nil, fmt.Errorf("failed to get transaction ID: %w", err)
	}

	// Find the non-anchor output (the one with value > 0)
	for i, output := range n.Outputs {
		if output.Value > 0 {
			return &wire.OutPoint{
				Hash:  txHash,
				Index: uint32(i),
			}, nil
		}
	}

	return nil, fmt.Errorf("no non-anchor output found in leaf node")
}

// ExtractPathForIndex extracts the path to a specific leaf by its index.
// The index is 0-based and refers to the leaf position in the tree's in-order traversal.
// Returns nil if the index is out of bounds.
func (n *TreeNode) ExtractPathForIndex(targetIndex int) (*TreeNode, error) {
	if n == nil {
		return nil, nil
	}

	if targetIndex < 0 {
		return nil, fmt.Errorf("leaf index must be non-negative, got %d", targetIndex)
	}

	// First, count total leaves to validate index
	totalLeaves := n.countLeaves()
	if targetIndex >= totalLeaves {
		return nil, nil // Index out of bounds, return nil (no error)
	}

	// Find the path to the target leaf
	extracted, _ := n.extractPathForIndexRecursive(targetIndex, 0)
	return extracted, nil
}

// countLeaves returns the total number of leaf nodes in this subtree
func (n *TreeNode) countLeaves() int {
	if n == nil {
		return 0
	}

	if len(n.Children) == 0 {
		return 1 // This is a leaf
	}

	count := 0
	for _, child := range n.Children {
		count += child.countLeaves()
	}
	return count
}

// extractPathForIndexRecursive recursively extracts the path to the target leaf index.
// Returns the extracted node and the number of leaves consumed in this subtree.
func (n *TreeNode) extractPathForIndexRecursive(targetIndex, currentIndex int) (*TreeNode, int) {
	if n == nil {
		return nil, currentIndex
	}

	// If this is a leaf node
	if len(n.Children) == 0 {
		if currentIndex == targetIndex {
			// Found our target leaf
			return &TreeNode{
				Input:     n.Input,
				CoSigners: n.CoSigners,
				Outputs:   n.Outputs,
				Children:  make(map[uint32]*TreeNode),
				Signature: n.Signature,
			}, currentIndex + 1
		}
		return nil, currentIndex + 1
	}

	// This is a branch node, check children
	extracted := &TreeNode{
		Input:     n.Input,
		CoSigners: n.CoSigners,
		Outputs:   n.Outputs,
		Children:  make(map[uint32]*TreeNode),
		Signature: n.Signature,
	}

	leafIndex := currentIndex
	foundTarget := false

	// Process children in sorted order for consistent indexing
	var outputIndices []uint32
	for idx := range n.Children {
		outputIndices = append(outputIndices, idx)
	}
	sort.Slice(outputIndices, func(i, j int) bool {
		return outputIndices[i] < outputIndices[j]
	})

	for _, idx := range outputIndices {
		child := n.Children[idx]
		childExtracted, newLeafIndex := child.extractPathForIndexRecursive(targetIndex, leafIndex)

		if childExtracted != nil {
			extracted.Children[idx] = childExtracted
			foundTarget = true
		}

		leafIndex = newLeafIndex
	}

	if foundTarget {
		return extracted, leafIndex
	}
	return nil, leafIndex
}

// SubmitTreeSigs stores the final musig2 signatures in the appropriate tree nodes.
// The sigs parameter maps transaction IDs to their corresponding signatures.
// This method walks through the tree and assigns each signature to its corresponding node.
func (n *TreeNode) SubmitTreeSigs(sigs map[string]*schnorr.Signature) error {
	if n == nil {
		return fmt.Errorf("cannot submit signatures to nil tree")
	}

	return n.ForEach(func(node *TreeNode) error {
		txHash, err := node.TXID()
		if err != nil {
			return fmt.Errorf("failed to get transaction ID for node: %w", err)
		}

		sig, exists := sigs[txHash.String()]
		if !exists {
			return fmt.Errorf("signature not found for transaction %s", txHash.String())
		}

		node.Signature = sig
		return nil
	})
}

// ToSignedTx converts the TreeNode into its signed wire.MsgTx representation.
// This requires that the node has a signature set via SubmitTreeSigs.
func (n *TreeNode) ToSignedTx() (*wire.MsgTx, error) {
	if n == nil {
		return nil, fmt.Errorf("cannot create signed transaction from nil node")
	}

	if n.Signature == nil {
		return nil, fmt.Errorf("cannot create signed transaction: no signature present")
	}

	// Start with the unsigned transaction
	tx, err := n.ToTx()
	if err != nil {
		return nil, fmt.Errorf("failed to create base transaction: %w", err)
	}

	// Add the signature as witness data for the single input
	// For taproot keyspend, the witness consists of just the signature
	if len(tx.TxIn) != 1 {
		return nil, fmt.Errorf("expected exactly 1 input, got %d", len(tx.TxIn))
	}

	tx.TxIn[0].Witness = wire.TxWitness{n.Signature.Serialize()}

	return tx, nil
}

// VerifySigned verifies that all nodes in the tree have valid signatures.
// It checks that:
// 1. Every node has a signature
// 2. Each signature is valid for its transaction
//
// Parameters:
//   - sweepTweak: The taproot tweak bytes used in the musig2 signing process
//   - prevOutFetcher: Fetcher for previous outputs needed for signature verification
func (n *TreeNode) VerifySigned(sweepTweak []byte,
	prevOutFetcher txscript.PrevOutputFetcher) error {

	if n == nil {
		return fmt.Errorf("cannot verify nil tree")
	}

	return n.ForEach(func(node *TreeNode) error {
		// Check that signature is present
		if node.Signature == nil {
			txHash, _ := node.TXID()
			return fmt.Errorf("no signature found for "+
				"transaction %s", txHash.String())
		}

		// Verify the signature
		return node.verifyNodeSignature(sweepTweak, prevOutFetcher)
	})
}

// verifyNodeSignature verifies the signature for a single tree node.
func (n *TreeNode) verifyNodeSignature(sweepTweak []byte,
	prevOutFetcher txscript.PrevOutputFetcher) error {

	// Create the transaction to verify
	tx, err := n.ToTx()
	if err != nil {
		return fmt.Errorf("failed to create transaction: %w", err)
	}

	// Aggregate the cosigner keys with the sweep tweak
	aggKey, _, _, err := musig2.AggregateKeys(
		n.CoSigners, true, musig2.WithTaprootKeyTweak(sweepTweak),
	)
	if err != nil {
		return fmt.Errorf("failed to aggregate keys: %w", err)
	}

	// Calculate the signature hash
	sigHash, err := txscript.CalcTaprootSignatureHash(
		txscript.NewTxSigHashes(tx, prevOutFetcher),
		txscript.SigHashDefault, tx, 0, prevOutFetcher,
	)
	if err != nil {
		return fmt.Errorf("failed to calculate signature hash: %w", err)
	}

	// Verify the signature against the aggregated key and message
	if !n.Signature.Verify(sigHash, aggKey.FinalKey) {
		txHash, _ := n.TXID()

		return fmt.Errorf("signature verification failed for "+
			"transaction %s", txHash.String())
	}

	return nil
}

// PrevOutputFetcher creates a PrevOutputFetcher that can provide transaction
// outputs for all transactions in the tree, starting with the initial previous
// output that the root transaction spends.
func (n *TreeNode) PrevOutputFetcher(initialPrevOut *wire.TxOut) (
	txscript.PrevOutputFetcher, error) {

	outputs := make(map[wire.OutPoint]*wire.TxOut)

	// Add the initial previous output that the root spends
	if n.Input != nil {
		outputs[*n.Input] = initialPrevOut
	}

	// Walk the tree and collect all transaction outputs
	err := n.ForEach(func(node *TreeNode) error {
		// Get this node's transaction hash
		txHash, err := node.TXID()
		if err != nil {
			return fmt.Errorf("failed to get transaction ID "+
				"for node: %w", err)
		}

		// Add all outputs from this transaction
		for i, output := range node.Outputs {
			outpoint := wire.OutPoint{
				Hash:  txHash,
				Index: uint32(i),
			}
			outputs[outpoint] = output
		}

		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("failed to build PrevOutputFetcher: %w",
			err)
	}

	return txscript.NewMultiPrevOutFetcher(outputs), nil
}

// PrettyPrint returns a visual representation of the transaction tree.
// It shows a legend mapping keys to aliases and then displays the tree
// structure with transaction IDs, amounts, and participant information.
func (n *TreeNode) PrettyPrint() string {
	if n == nil {
		return "Tree: <nil>\n"
	}

	// Collect all unique cosigners and create aliases
	keyMap := make(map[string]string)
	keyCounter := 0

	n.collectKeys(keyMap, &keyCounter)

	// Build the output
	var result strings.Builder

	// Write legend
	result.WriteString("=== VTXO Tree Structure ===\n\n")
	result.WriteString("Legend (Key Aliases):\n")

	// Sort keys for consistent output
	var sortedKeys []string
	for key := range keyMap {
		sortedKeys = append(sortedKeys, key)
	}
	sort.Strings(sortedKeys)

	for _, key := range sortedKeys {
		alias := keyMap[key]
		result.WriteString(
			fmt.Sprintf("  %s = %s...\n", alias, key[:8]),
		)
	}
	result.WriteString("\n")

	// Write tree structure
	result.WriteString("Tree Structure:\n")
	n.printNode(&result, keyMap, "", true)

	return result.String()
}

// collectKeys recursively collects all unique public keys and assigns aliases
// The operator key is identified as the key that appears in ALL nodes
// (including all leaves)
func (n *TreeNode) collectKeys(keyMap map[string]string, counter *int) {
	if n == nil {
		return
	}

	// First pass: collect all keys without assigning aliases yet
	allKeys := make(map[string]bool)
	n.collectAllKeys(allKeys)

	// Find the operator key (appears in all nodes)
	operatorKey := n.findOperatorKey(allKeys)

	// Assign aliases: operator first, then participants
	for keyStr := range allKeys {
		if _, exists := keyMap[keyStr]; !exists {
			var alias string
			if keyStr == operatorKey {
				alias = "OP"
			} else {
				*counter++
				alias = fmt.Sprintf("P%d", *counter)
			}
			keyMap[keyStr] = alias
		}
	}
}

// collectAllKeys recursively collects all unique public keys in the tree
func (n *TreeNode) collectAllKeys(allKeys map[string]bool) {
	if n == nil {
		return
	}

	for _, cosigner := range n.CoSigners {
		keyStr := hex.EncodeToString(schnorr.SerializePubKey(cosigner))
		allKeys[keyStr] = true
	}

	for _, child := range n.Children {
		child.collectAllKeys(allKeys)
	}
}

// findOperatorKey identifies the operator key as the one that appears in all
// leaf nodes
func (n *TreeNode) findOperatorKey(allKeys map[string]bool) string {
	// Collect all leaves
	var leaves []*TreeNode
	n.collectLeaves(&leaves)

	if len(leaves) == 0 {
		// If no leaves, return the first key (fallback)
		for keyStr := range allKeys {
			return keyStr
		}
		return ""
	}

	// Find keys that appear in ALL leaves
	keyCount := make(map[string]int)
	for _, leaf := range leaves {
		leafKeys := make(map[string]bool)
		for _, cosigner := range leaf.CoSigners {
			keyStr := hex.EncodeToString(
				schnorr.SerializePubKey(cosigner),
			)

			if !leafKeys[keyStr] { // Only count once per leaf.
				leafKeys[keyStr] = true
				keyCount[keyStr]++
			}
		}
	}

	// Find the key that appears in all leaves
	for keyStr, count := range keyCount {
		if count == len(leaves) {
			return keyStr // This is the operator key
		}
	}

	// Fallback: return first key if no key appears in all leaves
	for keyStr := range allKeys {
		return keyStr
	}
	return ""
}

// collectLeaves recursively collects all leaf nodes
func (n *TreeNode) collectLeaves(leaves *[]*TreeNode) {
	if n == nil {
		return
	}

	if len(n.Children) == 0 {
		*leaves = append(*leaves, n)
		return
	}

	for _, child := range n.Children {
		child.collectLeaves(leaves)
	}
}

// printNode recursively prints a node and its children with proper indentation
func (n *TreeNode) printNode(result *strings.Builder, keyMap map[string]string,
	prefix string, isLast bool) {

	if n == nil {
		return
	}

	// Get transaction hash for display
	tx, err := n.ToTx()
	var txIDDisplay string
	if err != nil {
		txIDDisplay = "ERROR"
	} else {
		txHash := tx.TxHash()
		txIDDisplay = fmt.Sprintf("%02x%02x", txHash[0], txHash[1])
	}

	// Determine if this is a leaf node
	isLeaf := len(n.Children) == 0

	// Create node symbol
	var connector string
	if isLast {
		connector = "└── "
	} else {
		connector = "├── "
	}

	// Create aliases for cosigners
	var cosignerAliases []string
	for _, cosigner := range n.CoSigners {
		keyStr := hex.EncodeToString(schnorr.SerializePubKey(cosigner))
		if alias, exists := keyMap[keyStr]; exists {
			cosignerAliases = append(cosignerAliases, alias)
		}
	}
	sort.Strings(cosignerAliases) // Sort for consistent output

	// Calculate total amount
	var totalAmount int64
	for i, output := range n.Outputs {
		// Skip anchor output (usually the last output with 0 value)
		if i < len(n.Outputs)-1 || output.Value > 0 {
			totalAmount += output.Value
		}
	}

	// Build node display
	nodeType := "Branch"
	if isLeaf {
		nodeType = "Leaf"
	}

	result.WriteString(fmt.Sprintf(
		"%s%s[%s] %s (%d sats) [%s]\n", prefix, connector, txIDDisplay,
		nodeType, totalAmount, strings.Join(cosignerAliases, ","),
	))

	// Add output details for non-leaf nodes
	if !isLeaf && len(n.Outputs) > 0 {
		for i, output := range n.Outputs {
			// Skip anchor outputs (0 value)
			if output.Value == 0 {
				continue
			}

			var nextPrefix string
			if isLast {
				nextPrefix = prefix + "    "
			} else {
				nextPrefix = prefix + "│   "
			}

			result.WriteString(
				fmt.Sprintf("%s    → Out[%d]: %d sats\n",
					nextPrefix, i, output.Value,
				),
			)
		}
	}

	// Print children
	if len(n.Children) > 0 {
		// Get sorted output indices for consistent display
		var outputIndices []uint32
		for idx := range n.Children {
			outputIndices = append(outputIndices, idx)
		}
		sort.Slice(outputIndices, func(i, j int) bool {
			return outputIndices[i] < outputIndices[j]
		})

		for i, idx := range outputIndices {
			child := n.Children[idx]
			isLastChild := i == len(outputIndices)-1

			var newPrefix string
			if isLast {
				newPrefix = prefix + "    "
			} else {
				newPrefix = prefix + "│   "
			}

			child.printNode(result, keyMap, newPrefix, isLastChild)
		}
	}
}
