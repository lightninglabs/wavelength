package tree

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
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

	// AssetContext holds asset-specific data keyed by Node pointer. This is
	// populated during structure building and used by asset materializers.
	AssetContext *AssetContext

	// LeafScriptMap maps leaf node pointers to their output scripts
	// (pkscript). This is populated during structure building and used by
	// the BTC materializer. We keep BTC leaf data out of Node to keep the
	// shared structure asset-agnostic and avoid touching branch nodes.
	// Asset trees use AssetContext instead. Only leaf nodes have entries;
	// branch nodes are not present.
	LeafScriptMap map[*Node][]byte
}

// BuildStructure builds the tree structure bottom-up from leaf descriptors.
// This is pass 1 of the two-pass tree construction. It creates the tree shape,
// computes cosigners and internal keys - but does NOT build transactions or
// proofs.
//
// The returned Structure contains:
//   - Root: The root node with CoSigners and Children populated
//   - AssetContext: Asset-specific node data keyed by Node pointer (for asset
//     trees only), including asset values, proofs, and leaf metadata
//   - LeafScriptMap: Map of leaf nodes to output scripts (for BTC trees only)
//
// The following Node fields are NOT set (filled in by Materialize):
//   - Input (zero outpoint)
//   - Outputs (nil)
//   - FinalKey
//   - Signature
func BuildStructure(leaves []LeafDescriptor, cfg StructureConfig) (
	*Structure, error) {

	if len(leaves) == 0 {
		return nil, nil
	}

	weightFn := cfg.WeightFn
	if weightFn == nil {
		weightFn = WeightByBtcAmount()
	}

	// Create context to hold asset-specific node data.
	assetCtx := NewAssetContext()

	// Create leaf scripts map for BTC path (maps leaf nodes to pkscripts).
	leafScripts := make(map[*Node][]byte)

	// Build recursively from leaves up.
	root, err := buildStructureRecursive(
		leaves, cfg.OperatorKey, cfg.Radix, weightFn,
		assetCtx, leafScripts,
	)
	if err != nil {
		return nil, err
	}

	return &Structure{
		Root:          root,
		AssetContext:  assetCtx,
		LeafScriptMap: leafScripts,
	}, nil
}

// buildStructureRecursive recursively builds the tree structure. For a single
// leaf, it creates a leaf node. For multiple leaves, it partitions them and
// creates a branch node with children built recursively.
func buildStructureRecursive(leaves []LeafDescriptor,
	operatorKey *btcec.PublicKey, radix int, weightFn PartitionWeightFunc,
	assetCtx *AssetContext, leafScripts map[*Node][]byte) (*Node, error) {

	// Base case: single leaf becomes a leaf node.
	if len(leaves) == 1 {
		return buildLeafStructure(
			leaves[0], operatorKey, assetCtx, leafScripts,
		)
	}

	// Partition leaves into groups.
	groups := partitionLeaves(leaves, radix, weightFn)

	// Build children recursively and collect their cosigners.
	children := make(map[uint32]*Node, len(groups))
	allCosigners := []*btcec.PublicKey{operatorKey}
	var totalAssetAmount uint64
	var totalBtcAmount btcutil.Amount

	for i, group := range groups {
		if len(group) == 0 {
			continue
		}

		child, err := buildStructureRecursive(
			group, operatorKey, radix, weightFn, assetCtx,
			leafScripts,
		)
		if err != nil {
			return nil, err
		}

		children[uint32(i)] = child

		// Collect cosigners from child.
		allCosigners = append(allCosigners, child.CoSigners...)

		// Sum asset amounts from children (stored in asset context).
		totalAssetAmount += assetCtx.AssetValue(child)

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

	// Store subtree asset amount in asset context.
	assetCtx.SetAssetValue(node, totalAssetAmount)

	return node, nil
}

// buildLeafStructure creates the structure for a leaf node and populates
// the asset context with leaf-specific data (for asset trees) and the
// leafScripts map (for BTC trees).
func buildLeafStructure(leaf LeafDescriptor, operatorKey *btcec.PublicKey,
	assetCtx *AssetContext, leafScripts map[*Node][]byte) (*Node,
	error) {

	if leaf.CoSignerKey == nil {
		return nil, fmt.Errorf("leaf cosigner key cannot be nil")
	}

	// Leaf cosigners: operator + leaf owner.
	cosigners := []*btcec.PublicKey{operatorKey, leaf.CoSignerKey}

	// Get asset amount from metadata if present.
	var assetAmount uint64
	if leaf.Asset != nil {
		assetAmount = leaf.Asset.AssetAmount
	}

	node := &Node{
		// Input, Outputs, FinalKey left empty - filled by Materialize.
		CoSigners: cosigners,
		Children:  make(map[uint32]*Node),
		Amount:    leaf.Amount,
		Signature: nil,
	}

	// Store leaf-specific data in the asset context, keyed by node pointer.
	// This includes the asset amount for this leaf.
	assetCtx.Set(node, &AssetNodeState{
		AssetAmount:  assetAmount,
		LeafPkScript: leaf.PkScript,
		AssetProof:   proofBytes(leaf.Asset),
		Leaf:         leaf.Asset,
	})

	// Also store the leaf pkscript in the dedicated map for BTC path.
	// This keeps BTC and asset concerns separate.
	if leafScripts != nil && len(leaf.PkScript) > 0 {
		leafScripts[node] = leaf.PkScript
	}

	return node, nil
}

// proofBytes returns a copy of the input proof blob from asset metadata if
// present.
func proofBytes(meta *AssetLeafMetadata) []byte {
	if meta == nil || len(meta.InputProof) == 0 {
		return nil
	}

	proof := make([]byte, len(meta.InputProof))
	copy(proof, meta.InputProof)

	return proof
}
