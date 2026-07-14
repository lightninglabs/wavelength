package arkrpc

import (
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/wavelength/lib/tree"
)

// MaxAncestryTreeWalkDepth bounds depth-walk recursion across an
// indexer-supplied AncestryPath's reconstructed tree. The proto layer
// itself is iterative (TreePathToTree builds nodes from a flat array),
// but the wired-up child pointers can still form an arbitrarily deep
// chain that would overflow the goroutine stack if walked recursively.
// 32 matches the structural cap enforced by db.MaxTreeDeserializeDepth
// so any tree that survives DB decode also survives this walk.
const MaxAncestryTreeWalkDepth = 32

// AncestryPathFromTree builds a proto AncestryPath from a tree fragment, the
// commitment txid that anchors the fragment, and the Ark tx input indices
// served by the fragment.
//
// The tree depth is derived from the longest root-to-leaf path in t. Empty
// inputIndices are preserved as-is (round-direct VTXOs report no input
// indices because they are not produced by an OOR Ark tx).
//
// Returns an error if the tree depth exceeds MaxAncestryTreeWalkDepth,
// matching the bound the receive path enforces on the decoded blob.
func AncestryPathFromTree(t *tree.Tree, commitmentTxID chainhash.Hash,
	inputIndices []uint32) (*AncestryPath, error) {

	if t == nil {
		return nil, fmt.Errorf("ancestry tree must not be nil")
	}

	tp, err := TreePathFromTree(t)
	if err != nil {
		return nil, fmt.Errorf("flatten tree: %w", err)
	}

	depth, err := treeMaxDepth(t)
	if err != nil {
		return nil, fmt.Errorf("tree depth: %w", err)
	}

	// Copy inputIndices to avoid aliasing the caller's slice.
	indices := make([]uint32, len(inputIndices))
	copy(indices, inputIndices)

	return &AncestryPath{
		TreePath:       tp,
		CommitmentTxid: append([]byte(nil), commitmentTxID[:]...),
		InputIndices:   indices,
		TreeDepth:      uint32(depth),
	}, nil
}

// AncestryPathToTree reconstructs a tree.Tree from an AncestryPath. Returns
// (nil, nil) when the path or its embedded tree_path is unset; callers must
// treat a nil tree as an absent ancestry.
func AncestryPathToTree(p *AncestryPath) (*tree.Tree, error) {
	if p == nil || p.TreePath == nil {
		return nil, nil
	}

	return TreePathToTree(p.TreePath)
}

// ValidateAncestryPathDepth checks the indexer-supplied tree_depth scalar
// against the reconstructed tree path. The receive path treats indexer
// responses as untrusted, so this is the trust boundary where a
// zero/oversized/inconsistent depth must fail closed.
//
// A claimed depth of zero is rejected outright: it can be persisted as a
// valid-looking ancestry but later trips the unroll proof-unavailable
// guard, silently stranding an OOR VTXO. Any value above
// MaxAncestryTreeWalkDepth is rejected because the receive-path decoder
// itself caps tree walks at that bound, so larger claims cannot be
// honoured by the same client that persisted them.
//
// When a reconstructed tree is supplied, the claim must equal the tree's
// actual depth. The descriptor's MaxTreeDepth drives expiry-monitoring
// timing, so a low claim against a deeper real tree could under-report
// the worst-case unilateral-exit window and delay the refresh/exit
// decision past the safe deadline.
//
// reconstructed may be nil; in that case only the range check is
// applied. Callers that require a non-nil tree (i.e. usable ancestry)
// enforce that separately.
func ValidateAncestryPathDepth(claimed uint32, reconstructed *tree.Tree) error {
	if claimed == 0 {
		return fmt.Errorf("ancestry tree_depth must be non-zero")
	}

	if claimed > MaxAncestryTreeWalkDepth {
		return fmt.Errorf("ancestry tree_depth %d exceeds max %d",
			claimed, MaxAncestryTreeWalkDepth)
	}

	if reconstructed == nil {
		return nil
	}

	// Use the locally-bounded walker rather than tree.Tree.Depth(),
	// which recurses without a depth cap. The tree was reconstructed
	// from indexer-supplied bytes; a linear chain of N nodes is
	// structurally valid for TreePathToTree (children must have a
	// strictly higher index but no overall length cap), so an
	// unbounded recursive Depth() would blow the goroutine stack on
	// a hostile path even when the claimed scalar is within range.
	actual, err := treeMaxDepth(reconstructed)
	if err != nil {
		return fmt.Errorf("walk reconstructed tree: %w", err)
	}

	if uint32(actual) != claimed {
		return fmt.Errorf("ancestry tree_depth %d does not match "+
			"reconstructed path depth %d", claimed, actual)
	}

	return nil
}

// AncestryCommitmentTxID extracts the commitment txid carried by the
// AncestryPath into a typed chainhash.Hash. Returns an error when the
// embedded txid byte slice is the wrong length.
func AncestryCommitmentTxID(p *AncestryPath) (chainhash.Hash, error) {
	if p == nil {
		return chainhash.Hash{}, fmt.Errorf("nil ancestry path")
	}

	if len(p.CommitmentTxid) != chainhash.HashSize {
		return chainhash.Hash{}, fmt.Errorf("invalid commitment_txid "+
			"length %d, want %d", len(p.CommitmentTxid),
			chainhash.HashSize)
	}

	var h chainhash.Hash
	copy(h[:], p.CommitmentTxid)

	return h, nil
}

// treeMaxDepth returns the longest root-to-leaf depth (in transactions)
// across the recursive node structure of t. A tree containing only the
// root has depth 1.
//
// Returns an error if traversal would exceed MaxAncestryTreeWalkDepth.
// The walk is bounded so an indexer-supplied tree whose child pointers
// form a deep linear chain cannot crash the receive path with a stack
// overflow.
func treeMaxDepth(t *tree.Tree) (int, error) {
	if t == nil || t.Root == nil {
		return 0, nil
	}

	return nodeMaxDepth(t.Root, 1)
}

// nodeMaxDepth recursively walks the children to find the longest
// path. depth is the depth of n itself (1 for the root) and is bounded
// by MaxAncestryTreeWalkDepth.
func nodeMaxDepth(n *tree.Node, depth int) (int, error) {
	if n == nil {
		return 0, nil
	}

	if depth > MaxAncestryTreeWalkDepth {
		return 0, fmt.Errorf("tree depth exceeds max %d",
			MaxAncestryTreeWalkDepth)
	}

	if len(n.Children) == 0 {
		return depth, nil
	}

	deepest := depth
	for _, child := range n.Children {
		d, err := nodeMaxDepth(child, depth+1)
		if err != nil {
			return 0, err
		}

		if d > deepest {
			deepest = d
		}
	}

	return deepest, nil
}
