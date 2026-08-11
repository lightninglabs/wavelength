package arkrpc

import (
	"fmt"
	"sort"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/tree"
)

// TreePathFromTree converts a tree.Tree (typically an extracted path) into its
// proto TreePath representation by flattening the recursive node structure
// into a pre-order indexed slice.
func TreePathFromTree(t *tree.Tree) (*TreePath, error) {
	if t == nil {
		return nil, nil
	}

	// Flatten nodes in pre-order.
	var nodes []*TreePathNode
	nodeIndex := make(map[*tree.Node]uint32)
	if err := flattenTreePathNode(
		t.Root, &nodes, nodeIndex,
	); err != nil {
		return nil, err
	}

	return &TreePath{
		Nodes:              nodes,
		BatchOutpoint:      outpointToProto(t.BatchOutpoint),
		BatchOutput:        txOutToProto(t.BatchOutput),
		SweepTapscriptRoot: t.SweepTapscriptRoot,
	}, nil
}

// DefaultMaxTreePathNodes is the upper bound on the number of nodes
// allowed in a TreePath decoded from an indexer response. A path is a
// slice through a commitment tree, so it is far smaller than a whole
// tree in practice; the bound is deliberately set to the same generous
// figure roundpb.DefaultMaxTreeNodes uses for a full tree, so the two
// decode boundaries agree and no shape that survives one is rejected by
// the other purely on size.
const DefaultMaxTreePathNodes = 50_000

// TreePathToTreeOption is a functional option for TreePathToTree that
// allows callers to override default validation parameters.
type TreePathToTreeOption func(*treePathToTreeConfig)

// treePathToTreeConfig holds configurable validation parameters for tree
// path deserialization.
type treePathToTreeConfig struct {
	maxNodes int
}

// defaultTreePathToTreeConfig returns the default configuration.
func defaultTreePathToTreeConfig() treePathToTreeConfig {
	return treePathToTreeConfig{
		maxNodes: DefaultMaxTreePathNodes,
	}
}

// WithMaxTreePathNodes sets the maximum number of nodes allowed in a
// deserialized TreePath. A value of 0 disables the limit.
func WithMaxTreePathNodes(maxNodes int) TreePathToTreeOption {
	return func(cfg *treePathToTreeConfig) {
		cfg.maxNodes = maxNodes
	}
}

// TreePathToTree converts a proto TreePath back to a tree.Tree by
// reconstructing the recursive node structure from the flattened nodes.
//
// This sits on the indexer receive path, which treats responses as
// untrusted, so the reconstructed shape is validated to be exactly one
// connected tree rather than merely an acyclic graph.
func TreePathToTree(tp *TreePath,
	opts ...TreePathToTreeOption) (*tree.Tree, error) {

	if tp == nil {
		return nil, nil
	}

	cfg := defaultTreePathToTreeConfig()
	for _, o := range opts {
		o(&cfg)
	}

	if len(tp.Nodes) == 0 {
		return nil, fmt.Errorf("empty tree path nodes")
	}

	if cfg.maxNodes > 0 && len(tp.Nodes) > cfg.maxNodes {
		return nil, fmt.Errorf("tree path has %d nodes, exceeds "+
			"maximum %d", len(tp.Nodes), cfg.maxNodes)
	}

	// Convert all proto nodes to Go nodes.
	goNodes := make([]*tree.Node, len(tp.Nodes))
	for i, pn := range tp.Nodes {
		node, err := treePathNodeFromProto(pn)
		if err != nil {
			return nil, fmt.Errorf("node[%d]: %w", i, err)
		}
		goNodes[i] = node
	}

	// Wire up children references. The pre-order invariant
	// (childIdx > i) rules out cycles, but on its own it still
	// admits a DAG: two parents at different indices can both name
	// the same higher-indexed child and both satisfy it.
	//
	// That matters most here. Children is a map, so one node may
	// point all of its outputs at the same next node, and every
	// recursive walk over the result then re-visits the shared
	// subtree once per path that reaches it. nodeMaxDepth, which the
	// receive path runs on this very tree to validate the claimed
	// ancestry depth, is exactly such a walk: it has no memoization,
	// so a shared child turns a linear-sized message into
	// exponentially many paths.
	//
	// parentOf records the parent that claimed each child, so a
	// second claim can name both of them. The bounds check runs
	// first so a wild index is reported as out of range rather than
	// as a sharing violation.
	parentOf := make(map[uint32]int, len(tp.Nodes))

	for i, pn := range tp.Nodes {
		for outIdx, childIdx := range pn.Children {
			if childIdx <= uint32(i) {
				return nil, fmt.Errorf("node[%d] child index "+
					"%d must be > parent index (cycle or "+
					"back-reference)", i, childIdx)
			}

			if int(childIdx) >= len(goNodes) {
				return nil, fmt.Errorf("node[%d] child index "+
					"%d out of range", i, childIdx)
			}

			if prev, dup := parentOf[childIdx]; dup {
				return nil, fmt.Errorf("node[%d] child index "+
					"%d is already a child of node[%d]; "+
					"tree must not share children", i,
					childIdx, prev)
			}
			parentOf[childIdx] = i

			if int(outIdx) >= len(goNodes[i].Outputs) {
				return nil, fmt.Errorf("node[%d] child output "+
					"index %d out of range (node has %d "+
					"outputs)", i, outIdx,
					len(goNodes[i].Outputs))
			}

			goNodes[i].Children[outIdx] = goNodes[childIdx]
		}
	}

	// Single-parent plus forward-edges describes a forest, not a
	// tree: nothing above requires a node to be reachable from index
	// 0. Every node other than the root must be claimed exactly
	// once, so counting the claims pins the shape to one connected
	// tree and rejects both padding with unreachable nodes and a
	// second detached root carrying its own subtree.
	// flattenTreePathNode always emits exactly this shape, so no
	// well-formed producer is affected.
	if len(parentOf) != len(tp.Nodes)-1 {
		return nil, fmt.Errorf("tree path has %d nodes but only %d "+
			"are claimed as children; unreachable nodes or "+
			"multiple roots", len(tp.Nodes), len(parentOf))
	}

	batchOP, err := outpointFromProto(tp.BatchOutpoint)
	if err != nil {
		return nil, fmt.Errorf("batch outpoint: %w", err)
	}

	batchOut, err := txOutFromProto(tp.BatchOutput)
	if err != nil {
		return nil, fmt.Errorf("batch output: %w", err)
	}

	return &tree.Tree{
		Root:               goNodes[0],
		BatchOutpoint:      batchOP,
		BatchOutput:        batchOut,
		SweepTapscriptRoot: tp.SweepTapscriptRoot,
	}, nil
}

// flattenTreePathNode recursively flattens a tree node into the nodes slice
// in pre-order.
func flattenTreePathNode(n *tree.Node, nodes *[]*TreePathNode,
	index map[*tree.Node]uint32) error {

	if n == nil {
		return nil
	}

	myIdx := uint32(len(*nodes))
	index[n] = myIdx

	// Convert outputs.
	outputs := make([]*TxOut, len(n.Outputs))
	for i, out := range n.Outputs {
		outputs[i] = txOutToProto(out)
	}

	// Convert co-signers.
	coSigners := make([][]byte, len(n.CoSigners))
	for i, pk := range n.CoSigners {
		coSigners[i] = pk.SerializeCompressed()
	}

	protoNode := &TreePathNode{
		Input:     outpointToProto(n.Input),
		Outputs:   outputs,
		CoSigners: coSigners,
		Children:  make(map[uint32]uint32),
		Amount:    int64(n.Amount),
		Signature: schnorrSigToBytes(n.Signature),
	}

	*nodes = append(*nodes, protoNode)

	// Recurse into children in deterministic order so the
	// flattened output is stable across runs.
	childIndices := make([]uint32, 0, len(n.Children))
	for outIdx := range n.Children {
		childIndices = append(childIndices, outIdx)
	}
	sort.Slice(childIndices, func(i, j int) bool {
		return childIndices[i] < childIndices[j]
	})

	for _, outIdx := range childIndices {
		child := n.Children[outIdx]
		if err := flattenTreePathNode(
			child, nodes, index,
		); err != nil {
			return err
		}

		protoNode.Children[outIdx] = index[child]
	}

	return nil
}

// treePathNodeFromProto converts a single proto TreePathNode to a tree.Node.
func treePathNodeFromProto(pn *TreePathNode) (*tree.Node, error) {
	input, err := outpointFromProto(pn.Input)
	if err != nil {
		return nil, fmt.Errorf("input: %w", err)
	}

	// Convert outputs.
	outputs := make([]*wire.TxOut, len(pn.Outputs))
	for i, out := range pn.Outputs {
		txOut, err := txOutFromProto(out)
		if err != nil {
			return nil, fmt.Errorf("output[%d]: %w", i, err)
		}
		outputs[i] = txOut
	}

	// Convert co-signers.
	coSigners := make([]*btcec.PublicKey, len(pn.CoSigners))
	for i, pkBytes := range pn.CoSigners {
		pk, err := btcec.ParsePubKey(pkBytes)
		if err != nil {
			return nil, fmt.Errorf("co_signer[%d]: %w", i, err)
		}
		coSigners[i] = pk
	}

	var sig *schnorr.Signature
	if len(pn.Signature) > 0 {
		sig, err = schnorr.ParseSignature(pn.Signature)
		if err != nil {
			return nil, fmt.Errorf("signature: %w", err)
		}
	}

	return &tree.Node{
		Input:     input,
		Outputs:   outputs,
		CoSigners: coSigners,
		Children:  make(map[uint32]*tree.Node),
		Amount:    btcutil.Amount(pn.Amount),
		Signature: sig,
	}, nil
}

// outpointToProto converts a wire.OutPoint to a proto OutPoint.
func outpointToProto(op wire.OutPoint) *OutPoint {
	hash := op.Hash[:]

	return &OutPoint{
		Txid: append([]byte(nil), hash...),
		Vout: op.Index,
	}
}

// outpointFromProto converts a proto OutPoint to a wire.OutPoint.
func outpointFromProto(op *OutPoint) (wire.OutPoint, error) {
	if op == nil {
		return wire.OutPoint{}, fmt.Errorf("nil outpoint")
	}

	if len(op.Txid) != chainhash.HashSize {
		return wire.OutPoint{}, fmt.Errorf("invalid txid length %d",
			len(op.Txid))
	}

	var hash chainhash.Hash
	copy(hash[:], op.Txid)

	return wire.OutPoint{
		Hash:  hash,
		Index: op.Vout,
	}, nil
}

// txOutToProto converts a wire.TxOut to a proto TxOut.
func txOutToProto(out *wire.TxOut) *TxOut {
	if out == nil {
		return nil
	}

	return &TxOut{
		Value:    out.Value,
		PkScript: append([]byte(nil), out.PkScript...),
	}
}

// txOutFromProto converts a proto TxOut to a wire.TxOut.
func txOutFromProto(out *TxOut) (*wire.TxOut, error) {
	if out == nil {
		return nil, nil
	}

	if out.Value < 0 {
		return nil, fmt.Errorf("negative output value %d", out.Value)
	}

	return &wire.TxOut{
		Value:    out.Value,
		PkScript: append([]byte(nil), out.PkScript...),
	}, nil
}

// schnorrSigToBytes serializes a schnorr signature to bytes.
func schnorrSigToBytes(sig *schnorr.Signature) []byte {
	if sig == nil {
		return nil
	}

	return sig.Serialize()
}
