package tree

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
)

// StructureConfig contains configuration for building tree structure.
type StructureConfig struct {
	// OperatorKey is the operator's public key (included in all cosigner
	// sets).
	OperatorKey *btcec.PublicKey

	// Radix is the maximum number of children per branch node.
	Radix int

	// WeightFn determines how leaves are weighted for partitioning.
	// If nil, defaults to WeightByBtcAmount().
	WeightFn PartitionWeightFunc
}

// Structure contains the outputs from BuildStructure.
type Structure struct {
	// Root is the root node of the tree structure.
	Root *Node

	// LeafScriptMap maps leaf node pointers to their output scripts
	// (pkscript). This is populated during structure building and used by
	// the BTC materializer. We keep BTC leaf data out of Node to keep the
	// shared structure asset-agnostic and avoid touching branch nodes.
	LeafScriptMap map[*Node][]byte
}

// BuildStructure builds the tree structure bottom-up from leaf descriptors.
// This is pass 1 of the two-pass tree construction. It creates the tree shape,
// computes cosigners and internal keys - but does NOT build transactions or
// proofs.
//
// The returned Structure contains:
//   - Root: The root node with CoSigners and Children populated
//   - LeafScriptMap: Map of leaf nodes to output scripts (for BTC trees only)
//
// The following Node fields are NOT set (filled in by Materialize):
//   - Input (zero outpoint)
//   - Outputs (nil)
//   - FinalKey
//   - Signature
func BuildStructure(leaves []LeafDescriptor,
	cfg StructureConfig) (*Structure, error) {

	if len(leaves) == 0 {
		return nil, nil
	}

	weightFn := cfg.WeightFn
	if weightFn == nil {
		weightFn = WeightByBtcAmount()
	}

	// Create leaf scripts map for BTC path (maps leaf nodes to pkscripts).
	leafScripts := make(map[*Node][]byte)

	// Build recursively from leaves up.
	root, err := buildStructureRecursive(
		leaves, cfg.OperatorKey, cfg.Radix, weightFn, leafScripts,
	)
	if err != nil {
		return nil, err
	}

	return &Structure{
		Root:          root,
		LeafScriptMap: leafScripts,
	}, nil
}

// buildStructureRecursive recursively builds the tree structure. For a single
// leaf, it creates a leaf node. For multiple leaves, it partitions them and
// creates a branch node with children built recursively.
func buildStructureRecursive(leaves []LeafDescriptor,
	operatorKey *btcec.PublicKey, radix int, weightFn PartitionWeightFunc,
	leafScripts map[*Node][]byte) (*Node, error) {

	// Base case: single leaf becomes a leaf node.
	if len(leaves) == 1 {
		return buildLeafStructure(leaves[0], operatorKey, leafScripts)
	}

	// Partition leaves into groups.
	groups := partitionLeaves(leaves, radix, weightFn)

	// Build children recursively and collect their cosigners.
	children := make(map[uint32]*Node, len(groups))
	allCosigners := []*btcec.PublicKey{operatorKey}
	var totalBtcAmount btcutil.Amount

	for i, group := range groups {
		if len(group) == 0 {
			continue
		}

		child, err := buildStructureRecursive(
			group, operatorKey, radix, weightFn, leafScripts,
		)
		if err != nil {
			return nil, err
		}

		children[uint32(i)] = child

		// Collect cosigners from child.
		allCosigners = append(allCosigners, child.CoSigners...)

		// Sum BTC amounts from children (stored in Node.Amount).
		totalBtcAmount += child.Amount
	}

	// Deduplicate cosigners.
	allCosigners = UniqueCosigners(allCosigners)

	// Create branch node structure.
	node := &Node{
		// Input, Outputs, FinalKey left empty - filled by Materialize.
		CoSigners: allCosigners,
		Children:  children,
		Amount:    totalBtcAmount,
		Signature: nil,
	}

	return node, nil
}

// buildLeafStructure creates the structure for a leaf node and populates
// the leafScripts map (for BTC trees).
func buildLeafStructure(leaf LeafDescriptor, operatorKey *btcec.PublicKey,
	leafScripts map[*Node][]byte) (*Node, error) {

	if leaf.CoSignerKey == nil {
		return nil, fmt.Errorf("leaf cosigner key cannot be nil")
	}

	// Leaf cosigners: operator + leaf owner. Connector leaves use the
	// operator key for both roles, so dedupe before computing keys.
	cosigners := UniqueCosigners([]*btcec.PublicKey{
		operatorKey,
		leaf.CoSignerKey,
	})

	node := &Node{
		// Input, Outputs, FinalKey left empty - filled by Materialize.
		CoSigners: cosigners,
		Children:  make(map[uint32]*Node),
		Amount:    leaf.Amount,
		Signature: nil,
	}

	// Also store the leaf pkscript in the dedicated map for BTC path.
	// This keeps BTC and asset concerns separate.
	if leafScripts != nil && len(leaf.PkScript) > 0 {
		leafScripts[node] = leaf.PkScript
	}

	return node, nil
}
