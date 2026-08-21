package tree

import (
	"fmt"
	"math"

	"github.com/btcsuite/btcd/wire/v2"
)

// AssetTreeContext carries the Taproot Asset side of an asset-aware tree
// through construction, signing, and persistence, keeping the shared Node
// and Tree types asset-agnostic.
//
// Entries live in two keying phases. During structure building, nodes have
// no inputs yet, so per-node asset amounts are keyed by node identity.
// Materialization assigns every node its input outpoint and records
// signing tweaks and sealed packages keyed by that outpoint: unlike node
// pointers, outpoints survive path extraction (which clones nodes) and
// serialization, so everything a signer, validator, or store needs is
// outpoint-addressed.
//
// The context is the mutable side-car of an otherwise immutable published
// *Tree: builders write it, later phases only read it.
type AssetTreeContext struct {
	// amountsByNode holds the subtree asset total per node, keyed by
	// node identity. Populated by the structure pass, consumed by the
	// materializer to size the asset split at every branch.
	amountsByNode map[*Node]uint64

	// amountsByInput re-keys the subtree totals by input outpoint once
	// materialization assigns inputs, so lookups survive path
	// extraction (which clones nodes) and serialization.
	amountsByInput map[wire.OutPoint]uint64

	// leafRootsByInput holds the asset commitment root of the leaf VTXO
	// each leaf node creates, keyed by that node's input outpoint. It is
	// what a leaf owner needs to reproduce the composed output script it
	// was paid to, and to record the VTXO as asset-bearing.
	leafRootsByInput map[wire.OutPoint][]byte

	// tweaksByInput holds the taproot signing tweak for the node
	// transaction spending each outpoint: the parent output's combined
	// tapscript root, committing to both the sweep leaf and the asset
	// commitment root.
	tweaksByInput map[wire.OutPoint][]byte

	// packagesByInput holds the serialized sealed transfer package of
	// the node transaction spending each outpoint. The package is the
	// authoritative record of the node's asset transition: its proof
	// suffixes feed unroll publication and its authenticated summaries
	// feed validation.
	packagesByInput map[wire.OutPoint][]byte

	// assetRef identifies the asset carried by every leaf of the tree,
	// in its canonical string encoding (group key reference for grouped
	// assets).
	assetRef string
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

// SetNodeAssetAmount records the subtree asset total for a node. Nodes
// that already carry their input outpoint are additionally indexed by it,
// so the amount stays resolvable on extracted or deserialized clones.
func (c *AssetTreeContext) SetNodeAssetAmount(node *Node, amount uint64) {
	c.amountsByNode[node] = amount
	if node.Input != (wire.OutPoint{}) {
		c.amountsByInput[node.Input] = amount
	}
}

// NodeAssetAmount returns the subtree asset total for a node, or zero when
// the node carries no assets. Node identity wins; clones resolve through
// their input outpoint.
func (c *AssetTreeContext) NodeAssetAmount(node *Node) uint64 {
	if amount, ok := c.amountsByNode[node]; ok {
		return amount
	}

	return c.amountsByInput[node.Input]
}

// SetSigningTweak records the taproot signing tweak for the node
// transaction spending the given outpoint.
func (c *AssetTreeContext) SetSigningTweak(input wire.OutPoint, tweak []byte) {
	c.tweaksByInput[input] = append([]byte(nil), tweak...)
}

// SigningTweak returns the signing tweak recorded for the node transaction
// spending the given outpoint, or nil when none was recorded.
func (c *AssetTreeContext) SigningTweak(input wire.OutPoint) []byte {
	return c.tweaksByInput[input]
}

// SetSealedPackage records the serialized sealed transfer package of the
// node transaction spending the given outpoint.
func (c *AssetTreeContext) SetSealedPackage(input wire.OutPoint, pkg []byte) {
	c.packagesByInput[input] = append([]byte(nil), pkg...)
}

// SealedPackage returns the sealed transfer package recorded for the node
// transaction spending the given outpoint, or nil when none was recorded.
func (c *AssetTreeContext) SealedPackage(input wire.OutPoint) []byte {
	return c.packagesByInput[input]
}

// SetAssetRef records the canonical string encoding of the tree's asset.
func (c *AssetTreeContext) SetAssetRef(ref string) {
	c.assetRef = ref
}

// AssetRef returns the canonical string encoding of the tree's asset, or
// the empty string when none was recorded.
func (c *AssetTreeContext) AssetRef() string {
	return c.assetRef
}

// IsEmpty reports whether the context carries no asset data at all.
func (c *AssetTreeContext) IsEmpty() bool {
	return len(c.amountsByNode) == 0 && len(c.amountsByInput) == 0 &&
		len(c.tweaksByInput) == 0 && len(c.packagesByInput) == 0 &&
		c.assetRef == ""
}

// TweakLookup adapts the context to the signer sessions: nodes whose input
// has a recorded tweak sign with it, every other node falls back to the
// tree-wide sweep root.
func (c *AssetTreeContext) TweakLookup() TaprootTweakLookup {
	return func(node *Node) []byte {
		return c.SigningTweak(node.Input)
	}
}

// aggregateAssetAmounts folds per-leaf asset amounts into subtree totals
// for every node, bottom-up, rejecting uint64 overflow.
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

// SetLeafAssetRoot records the asset commitment root of the leaf VTXO
// created by the node spending the given outpoint.
func (c *AssetTreeContext) SetLeafAssetRoot(input wire.OutPoint, root []byte) {
	if c == nil || len(root) == 0 {
		return
	}

	c.leafRootsByInput[input] = append([]byte(nil), root...)
}

// LeafAssetRoot returns the asset commitment root of the leaf VTXO
// created by the node spending the given outpoint, or nil when the node
// is not an asset leaf.
func (c *AssetTreeContext) LeafAssetRoot(input wire.OutPoint) []byte {
	if c == nil {
		return nil
	}

	return c.leafRootsByInput[input]
}

// LeafAssetRoot returns the asset commitment root of the leaf VTXO
// created by the node spending the given outpoint, or nil when the tree
// carries no asset context.
func (t *Tree) LeafAssetRoot(input wire.OutPoint) []byte {
	if t == nil {
		return nil
	}

	return t.AssetContext.LeafAssetRoot(input)
}
