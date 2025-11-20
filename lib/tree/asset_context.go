package tree

import (
	"github.com/btcsuite/btcd/btcutil"
)

// AssetContext holds asset-specific state for tree nodes, keyed by the
// node pointer. This keeps asset concerns separate from the core tree
// structure, allowing the Node type to remain focused on BTC tree operations.
//
// # Bundling Contract
//
// AssetTreeAssembler.BuildTree returns (*Tree, *AssetContext) as separate
// values. Callers MUST keep these paired throughout the tree's lifecycle:
//
//	tree, assetCtx, err := assembler.BuildTree(ctx, ...)
//	// Pass assetCtx to signing:
//	tweakLookup := TweakLookupFromAssetContext(assetCtx)
//	session, err := tree.NewTreeSignerSession(wallet, key, tweakLookup)
//	// Pass assetCtx to finalization:
//	finalizeTree(tree, assetCtx, ...)
//
// The asset context is not embedded in Tree because Tree is asset-agnostic and
// also used for BTC-only trees (where no asset context is needed).
//
// # Lifecycle
//
//  1. Structure building (pass 1): AssetContext created by PlanStructure,
//     populated with leaf metadata (AssetLeafMetadata).
//  2. Materialization (pass 2): AssetContext passed to AssetMaterializer, which
//     adds taproot tweaks and leaf pkscripts. Branch proofs are NOT stored;
//     they're propagated externally via MaterializeParams.ParentProof.
//  3. Signing: TweakLookupFromAssetContext provides per-node tweaks to MuSig2.
//  4. Finalization: AssetContext provides leaf metadata for proof construction.
//
// # Lookup Semantics
//
//   - Proofs, leaf metadata, and pkscripts are keyed by node pointer only.
//     These are accessed during tree construction/finalization where node
//     pointer identity is preserved.
//   - Taproot tweaks are additionally keyed by input outpoint (via
//     tweaksByOutpoint). This enables tweak lookup for nodes created by
//     ExtractPathForCoSigner, which have different pointer identity but the
//     same input outpoint as the original nodes.
type AssetContext struct {
	// entries maps node pointers to their asset state.
	entries map[*Node]*AssetNodeState

	// tweaksByOutpoint provides tweak lookup by node input outpoint.
	// This is needed because ExtractPathForCoSigner creates new node
	// objects with different pointers, but the same input outpoints.
	// Populated during materialization when tweaks are attached.
	tweaksByOutpoint map[string][]byte
}

// AssetNodeState holds asset-specific data for a single node.
type AssetNodeState struct {
	// AssetAmount is the asset amount for this node. For leaf nodes,
	// this is the leaf's asset amount. For branch nodes, this is the
	// sum of all descendant leaf asset amounts (subtree total).
	AssetAmount uint64

	// AssetProof is the serialized proof for this node's output.
	// This is populated after materialization and used as the input
	// proof for child nodes during tree traversal.
	AssetProof []byte

	// LeafPkScript is the output script for leaf nodes (from tapd).
	// For asset trees, the output script is determined by tapd and
	// stored here for reference. Nil for branch nodes.
	LeafPkScript []byte

	// Leaf contains leaf-specific metadata (nil for branch nodes).
	// This includes funding info, change scripts, etc.
	Leaf *AssetLeafMetadata

	// TaprootTweak is the tweak bytes for MuSig2 signing. This is
	// derived from the parent proof's asset commitment and used to
	// ensure the signing session produces the correct aggregate key.
	TaprootTweak []byte
}

// NewAssetContext creates an empty asset context map.
func NewAssetContext() *AssetContext {
	return &AssetContext{
		entries:          make(map[*Node]*AssetNodeState),
		tweaksByOutpoint: make(map[string][]byte),
	}
}

// Set stores state for the given node. If state already exists for
// this node, it is replaced.
func (s *AssetContext) Set(node *Node, state *AssetNodeState) {
	if s == nil || node == nil || state == nil {
		return
	}

	s.entries[node] = state
}

// Get retrieves state for the given node. Returns nil if no state
// exists for this node.
func (s *AssetContext) Get(node *Node) *AssetNodeState {
	if s == nil || node == nil {
		return nil
	}

	return s.entries[node]
}

// GetProof returns the asset proof for the given node. Returns nil
// if no state exists or if the state has no proof.
func (s *AssetContext) GetProof(node *Node) []byte {
	state := s.Get(node)
	if state == nil {
		return nil
	}

	return state.AssetProof
}

// GetTweak returns the taproot tweak for the given node. Returns nil
// if no state exists or if the state has no tweak.
func (s *AssetContext) GetTweak(node *Node) []byte {
	state := s.Get(node)
	if state == nil {
		return nil
	}

	return state.TaprootTweak
}

// GetLeaf returns the leaf metadata for the given node. Returns nil
// if no state exists or if this is not a leaf node.
func (s *AssetContext) GetLeaf(node *Node) *AssetLeafMetadata {
	state := s.Get(node)
	if state == nil {
		return nil
	}

	return state.Leaf
}

// GetLeafPkScript returns the leaf output script for the given node.
// Returns nil if no state exists or if the state has no script.
func (s *AssetContext) GetLeafPkScript(node *Node) []byte {
	state := s.Get(node)
	if state == nil {
		return nil
	}

	return state.LeafPkScript
}

// Len returns the number of entries in the asset context.
func (s *AssetContext) Len() int {
	if s == nil {
		return 0
	}

	return len(s.entries)
}

// AssetValue returns the asset amount for the given node. Returns 0
// if no state exists for this node.
func (s *AssetContext) AssetValue(node *Node) uint64 {
	state := s.Get(node)
	if state == nil {
		return 0
	}

	return state.AssetAmount
}

// SetAssetValue stores the asset amount for the given node. Creates a
// new state entry if one doesn't exist.
func (s *AssetContext) SetAssetValue(node *Node, amount uint64) {
	if s == nil || node == nil {
		return
	}

	state := s.Get(node)
	if state == nil {
		state = &AssetNodeState{}
	}

	state.AssetAmount = amount
	s.Set(node, state)
}

// SetTweakByOutpoint stores a tweak keyed by the node's input outpoint.
// This enables tweak lookup for nodes created by ExtractPathForCoSigner,
// which have different pointer identity but the same input outpoint.
func (s *AssetContext) SetTweakByOutpoint(node *Node, tweak []byte) {
	if s == nil || node == nil || len(tweak) == 0 {
		return
	}

	key := node.Input.String()
	s.tweaksByOutpoint[key] = tweak
}

// GetTweakByOutpoint retrieves a tweak by the node's input outpoint.
// Returns nil if no tweak exists for this outpoint.
func (s *AssetContext) GetTweakByOutpoint(node *Node) []byte {
	if s == nil || node == nil {
		return nil
	}

	return s.tweaksByOutpoint[node.Input.String()]
}

// TaprootTweakLookup returns the taproot tweak bytes for a node.
// This is called during signing to apply the correct tweak for MuSig2.
// Returns nil if no tweak is needed (BTC-only trees).
type TaprootTweakLookup func(node *Node) []byte

// TweakLookupFromAssetContext creates a TaprootTweakLookup that retrieves
// tweaks from the asset context map. This is the standard way to provide
// tweaks for asset tree signing.
//
// The lookup uses the node's input outpoint as the key, which allows
// it to work with nodes created by ExtractPathForCoSigner (which have
// different pointer identity but the same input outpoint as the
// original nodes).
//
// For BTC-only trees, pass nil as the asset context or use a nil lookup
// function, which will cause signing to fall back to the global
// sweep tapscript root.
func TweakLookupFromAssetContext(ctx *AssetContext) TaprootTweakLookup {
	return func(node *Node) []byte {
		if ctx == nil || node == nil {
			return nil
		}

		return ctx.GetTweakByOutpoint(node)
	}
}

// GetLeafFunding returns the funding amount for a leaf node by looking up the
// metadata in the asset context. Returns zero if the node is not a leaf or no
// metadata exists.
func GetLeafFunding(n *Node, ctx *AssetContext) btcutil.Amount {
	if n == nil || ctx == nil || !n.IsLeaf() {
		return 0
	}

	leaf := ctx.GetLeaf(n)
	if leaf == nil {
		return 0
	}

	return leaf.Funding
}

// TotalBTCFunding returns the sum of all leaf BTC funding amounts in the tree
// by traversing the tree and summing leaf funding values from the asset
// context.
func TotalBTCFunding(n *Node, assetCtx *AssetContext) btcutil.Amount {
	if n == nil {
		return 0
	}

	// Leaf node: return its funding amount from asset context.
	if n.IsLeaf() {
		state := assetCtx.Get(n)
		if state != nil && state.Leaf != nil {
			return state.Leaf.Funding
		}

		return 0
	}

	// Branch node: sum funding from all children.
	var total btcutil.Amount
	for _, child := range n.Children {
		total += TotalBTCFunding(child, assetCtx)
	}

	return total
}
