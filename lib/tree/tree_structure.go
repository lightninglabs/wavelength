package tree

import (
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
)

// TreeStructureConfig contains configuration for building tree structure.
type TreeStructureConfig struct {
	// OperatorKey is the operator's public key (included in all cosigner
	// sets).
	OperatorKey *btcec.PublicKey

	// Radix is the maximum number of children per branch node.
	Radix int

	// WeightFn determines how leaves are weighted for partitioning.
	// If nil, defaults to WeightByAssetAmountOrBTC().
	WeightFn PartitionWeightFunc
}

// BuildTreeStructure builds the tree structure bottom-up from leaf descriptors.
// This is pass 1 of the two-pass tree construction. It creates the tree shape,
// computes cosigners and internal keys, and sets asset amounts - but does NOT
// build transactions or proofs.
//
// The returned Node tree has:
//   - CoSigners populated at each level
//   - Children map wired up correctly
//   - Metadata.Leaf for leaf nodes (with AssetMetadata)
//
// The following are NOT set (filled in by Materialize):
//   - Input (zero outpoint)
//   - Outputs (nil)
//   - FinalKey, TaprootTweak
//   - OutputsMeta
//   - Signature
func BuildTreeStructure(leaves []LeafDescriptor,
	cfg TreeStructureConfig) (*Node, error) {

	if len(leaves) == 0 {
		return nil, nil
	}

	weightFn := cfg.WeightFn
	if weightFn == nil {
		weightFn = WeightByAssetAmountOrBTC()
	}

	// Build recursively from leaves up.
	return buildStructureRecursive(leaves, cfg.OperatorKey, cfg.Radix,
		weightFn)
}

// buildStructureRecursive recursively builds the tree structure. For a single
// leaf, it creates a leaf node. For multiple leaves, it partitions them and
// creates a branch node with children built recursively.
func buildStructureRecursive(leaves []LeafDescriptor,
	operatorKey *btcec.PublicKey, radix int,
	weightFn PartitionWeightFunc) (*Node, error) {

	// Base case: single leaf becomes a leaf node.
	if len(leaves) == 1 {
		return buildLeafStructure(leaves[0], operatorKey)
	}

	// Partition leaves into groups.
	groups := partitionLeaves(leaves, radix, weightFn)

	// Build children recursively and collect their cosigners.
	children := make(map[uint32]*Node, len(groups))
	allCosigners := []*btcec.PublicKey{operatorKey}

	for i, group := range groups {
		if len(group) == 0 {
			continue
		}

		child, err := buildStructureRecursive(
			group, operatorKey, radix, weightFn,
		)
		if err != nil {
			return nil, err
		}

		children[uint32(i)] = child

		// Collect cosigners from child.
		allCosigners = append(allCosigners, child.CoSigners...)
	}

	// Deduplicate cosigners.
	allCosigners = UniqueCosigners(allCosigners)

	// Create branch node structure.
	return &Node{
		// Input, Outputs, FinalKey, TaprootTweak left empty - filled by
		// Materialize.
		CoSigners: allCosigners,
		Children:  children,
		Signature: nil,
		Metadata:  aggregateBranchMetadata(groups),
	}, nil
}

// buildLeafStructure creates the structure for a leaf node.
func buildLeafStructure(leaf LeafDescriptor,
	operatorKey *btcec.PublicKey) (*Node, error) {

	// Leaf cosigners: operator + leaf owner.
	cosigners := []*btcec.PublicKey{operatorKey, leaf.CoSignerKey}

	return &Node{
		// Input, Outputs, FinalKey, TaprootTweak left empty - filled by
		// Materialize.
		CoSigners: cosigners,
		Children:  make(map[uint32]*Node),
		Signature: nil,
		Metadata: &NodeMetadata{
			AssetProof:   proofBytes(leaf.Asset),
			Leaf:         leaf.Asset,
			LeafPkScript: leaf.PkScript,
		},
	}, nil
}

// ComputeChildInternalKeys computes the internal keys (MuSig2 aggregates) for
// each child of a branch node. These are used as anchor keys when building the
// parent's transaction outputs.
//
// Returns a map from child index to internal key.
func ComputeChildInternalKeys(n *Node) (map[uint32]*btcec.PublicKey, error) {
	if n == nil || len(n.Children) == 0 {
		return nil, nil
	}

	keys := make(map[uint32]*btcec.PublicKey, len(n.Children))
	for idx, child := range n.Children {
		internalKey, err := ComputeInternalKey(child.CoSigners)
		if err != nil {
			return nil, err
		}
		keys[idx] = internalKey
	}

	return keys, nil
}

// ComputeChildAssetAmounts returns the total asset amount for each child
// subtree. This is used when building the parent's transaction outputs.
//
// Returns a map from child index to asset amount.
func ComputeChildAssetAmounts(n *Node) map[uint32]uint64 {
	if n == nil || len(n.Children) == 0 {
		return nil
	}

	amounts := make(map[uint32]uint64, len(n.Children))
	for idx, child := range n.Children {
		amounts[idx] = computeNodeAssetAmount(child)
	}

	return amounts
}

// computeNodeAssetAmount returns the total asset amount for a node's subtree.
func computeNodeAssetAmount(n *Node) uint64 {
	if n == nil {
		return 0
	}

	// Leaf node - get amount from metadata.
	if n.IsLeaf() {
		if n.Metadata != nil && n.Metadata.Leaf != nil {
			return n.Metadata.Leaf.AssetAmount
		}
		return 0
	}

	// Branch node - sum children.
	var total uint64
	for _, child := range n.Children {
		total += computeNodeAssetAmount(child)
	}
	return total
}

// GetLeafOwnerKey returns the leaf owner key (non-operator cosigner) for a
// leaf node. Returns nil if not a leaf or no owner key found.
func GetLeafOwnerKey(n *Node, operatorKey *btcec.PublicKey) *btcec.PublicKey {
	if n == nil || !n.IsLeaf() {
		return nil
	}

	for _, k := range n.CoSigners {
		if !k.IsEqual(operatorKey) {
			return k
		}
	}

	return nil
}

// GetLeafAssetAmount returns the asset amount for a leaf node.
func GetLeafAssetAmount(n *Node) uint64 {
	if n == nil || !n.IsLeaf() {
		return 0
	}

	if n.Metadata != nil && n.Metadata.Leaf != nil {
		return n.Metadata.Leaf.AssetAmount
	}

	return 0
}

// TotalAssetAmount returns the sum of all leaf asset amounts in the tree.
func TotalAssetAmount(n *Node) uint64 {
	return computeNodeAssetAmount(n)
}

// TotalBTCFunding returns the sum of all leaf BTC funding amounts in the tree
// by traversing the tree and summing leaf funding values.
func TotalBTCFunding(n *Node) btcutil.Amount {
	if n == nil {
		return 0
	}

	// Leaf node: return its funding amount.
	if n.IsLeaf() {
		if n.Metadata != nil && n.Metadata.Leaf != nil {
			return n.Metadata.Leaf.Funding.Amount
		}

		return 0
	}

	// Branch node: sum funding from all children.
	var total btcutil.Amount
	for _, child := range n.Children {
		total += TotalBTCFunding(child)
	}

	return total
}
