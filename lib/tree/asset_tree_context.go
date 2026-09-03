package tree

import (
	"fmt"
	"math"

	"github.com/btcsuite/btcd/wire/v2"
)

// AssetTreeContext contains the data needed to materialize and sign an asset
// tree. Builders populate the context before sharing the tree.
type AssetTreeContext struct {
	// Nodes do not have inputs until materialization, so the structure pass
	// records amounts by node first.
	amountsByNode map[*Node]uint64

	// Input outpoints remain stable when paths are extracted or serialized.
	amountsByInput map[wire.OutPoint]uint64

	leafRootsByInput map[wire.OutPoint][]byte
	tweaksByInput    map[wire.OutPoint][]byte
	packagesByInput  map[wire.OutPoint][]byte
	assetRef         string
}

// NewAssetTreeContext returns an empty asset tree context.
func NewAssetTreeContext() *AssetTreeContext {
	return &AssetTreeContext{
		amountsByNode:    make(map[*Node]uint64),
		amountsByInput:   make(map[wire.OutPoint]uint64),
		leafRootsByInput: make(map[wire.OutPoint][]byte),
		tweaksByInput:    make(map[wire.OutPoint][]byte),
		packagesByInput:  make(map[wire.OutPoint][]byte),
	}
}

// SetNodeAssetAmount records a node's subtree asset amount.
func (c *AssetTreeContext) SetNodeAssetAmount(node *Node, amount uint64) {
	c.amountsByNode[node] = amount
	if node.Input != (wire.OutPoint{}) {
		// Extracted paths contain cloned nodes, but preserve their
		// inputs.
		c.amountsByInput[node.Input] = amount
	}
}

// NodeAssetAmount returns a node's subtree asset amount.
func (c *AssetTreeContext) NodeAssetAmount(node *Node) uint64 {
	if amount, ok := c.amountsByNode[node]; ok {
		return amount
	}

	// Path extraction clones nodes but preserves their inputs.
	return c.amountsByInput[node.Input]
}

// SetSigningTweak records the taproot tweak for a tree transaction.
func (c *AssetTreeContext) SetSigningTweak(input wire.OutPoint, tweak []byte) {
	c.tweaksByInput[input] = append([]byte(nil), tweak...)
}

// SigningTweak returns the taproot tweak for a tree transaction.
func (c *AssetTreeContext) SigningTweak(input wire.OutPoint) []byte {
	return append([]byte(nil), c.tweaksByInput[input]...)
}

// SetSealedPackage records a tree transaction's sealed transfer package.
func (c *AssetTreeContext) SetSealedPackage(input wire.OutPoint, pkg []byte) {
	c.packagesByInput[input] = append([]byte(nil), pkg...)
}

// SealedPackage returns a tree transaction's sealed transfer package.
func (c *AssetTreeContext) SealedPackage(input wire.OutPoint) []byte {
	return append([]byte(nil), c.packagesByInput[input]...)
}

// SetAssetRef records the tree's asset reference.
func (c *AssetTreeContext) SetAssetRef(ref string) {
	c.assetRef = ref
}

// AssetRef returns the tree's asset reference.
func (c *AssetTreeContext) AssetRef() string {
	return c.assetRef
}

// IsEmpty reports whether the context carries no asset data at all.
func (c *AssetTreeContext) IsEmpty() bool {
	if c == nil {
		return true
	}

	return len(c.amountsByNode) == 0 && len(c.amountsByInput) == 0 &&
		len(c.leafRootsByInput) == 0 && len(c.tweaksByInput) == 0 &&
		len(c.packagesByInput) == 0 && c.assetRef == ""
}

// tweakLookup returns the signing tweak for a node's input.
func (c *AssetTreeContext) tweakLookup() func(*Node) []byte {
	return func(node *Node) []byte {
		return c.SigningTweak(node.Input)
	}
}

// aggregateAssetAmounts records the asset amount of every subtree.
func aggregateAssetAmounts(node *Node, leafAssets map[*Node]uint64,
	ctx *AssetTreeContext) (uint64, error) {

	if len(node.Children) == 0 {
		amount := leafAssets[node]
		ctx.SetNodeAssetAmount(node, amount)

		return amount, nil
	}

	var total uint64
	for _, child := range node.Children {
		childAmount, err := aggregateAssetAmounts(
			child, leafAssets, ctx,
		)
		if err != nil {
			return 0, err
		}

		if childAmount > math.MaxUint64-total {
			return 0, fmt.Errorf("asset amount overflow " +
				"aggregating subtree totals")
		}
		total += childAmount
	}

	ctx.SetNodeAssetAmount(node, total)

	return total, nil
}

// SetLeafAssetRoot records an asset leaf's commitment root.
func (c *AssetTreeContext) SetLeafAssetRoot(input wire.OutPoint, root []byte) {
	if c == nil || len(root) == 0 {
		return
	}

	c.leafRootsByInput[input] = append([]byte(nil), root...)
}

// LeafAssetRoot returns an asset leaf's commitment root.
func (c *AssetTreeContext) LeafAssetRoot(input wire.OutPoint) []byte {
	if c == nil {
		return nil
	}

	return append([]byte(nil), c.leafRootsByInput[input]...)
}

// LeafAssetRoot returns an asset leaf's commitment root.
func (t *Tree) LeafAssetRoot(input wire.OutPoint) []byte {
	if t == nil {
		return nil
	}

	return t.AssetContext.LeafAssetRoot(input)
}
