package recovery

import (
	"bytes"
	"fmt"
	"sort"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/lib/tree"
)

// NodeKind identifies the type of recovery transaction a node represents.
type NodeKind int

const (
	// NodeKindTree marks a round tree transaction.
	NodeKindTree NodeKind = iota

	// NodeKindCheckpoint marks a checkpoint transaction in OOR lineage.
	NodeKindCheckpoint

	// NodeKindArk marks an Ark transaction that spends checkpoints.
	NodeKindArk
)

// String returns the stable debug label for a NodeKind.
func (k NodeKind) String() string {
	switch k {
	case NodeKindTree:
		return "tree"

	case NodeKindCheckpoint:
		return "checkpoint"

	case NodeKindArk:
		return "ark"

	default:
		return fmt.Sprintf("unknown(%d)", k)
	}
}

// Node is one recovery transaction in a proof graph.
type Node struct {
	// Kind describes the role of this transaction in the proof.
	Kind NodeKind

	// Tx is the unsigned or signed transaction to materialize. The txid is
	// identical in either form, so the recovery planner only requires the
	// transaction itself.
	Tx *wire.MsgTx
}

// TXID returns the transaction hash for this recovery node.
func (n *Node) TXID() (chainhash.Hash, error) {
	if n == nil {
		return chainhash.Hash{}, fmt.Errorf("node cannot be nil")
	}

	if n.Tx == nil {
		return chainhash.Hash{}, fmt.Errorf("node tx cannot be nil")
	}

	return n.Tx.TxHash(), nil
}

// Output returns the output at the requested index.
func (n *Node) Output(index uint32) (*wire.TxOut, error) {
	if n == nil {
		return nil, fmt.Errorf("node cannot be nil")
	}

	if n.Tx == nil {
		return nil, fmt.Errorf("node tx cannot be nil")
	}

	if int(index) >= len(n.Tx.TxOut) {
		return nil, fmt.Errorf("output index %d out of bounds", index)
	}

	return n.Tx.TxOut[index], nil
}

// AnchorOutputIndex returns the unique anchor output index, if present.
func (n *Node) AnchorOutputIndex() (uint32, bool, error) {
	if n == nil {
		return 0, false, fmt.Errorf("node cannot be nil")
	}

	if n.Tx == nil {
		return 0, false, fmt.Errorf("node tx cannot be nil")
	}

	found := false
	var foundIndex uint32

	for index, out := range n.Tx.TxOut {
		if !bytes.Equal(out.PkScript, arkscript.AnchorPkScript) {
			continue
		}

		if found {
			return 0, false, fmt.Errorf(
				"multiple anchor outputs found",
			)
		}

		found = true
		foundIndex = uint32(index)
	}

	return foundIndex, found, nil
}

// AnchorOutpoint returns the anchor outpoint, if present.
func (n *Node) AnchorOutpoint() (wire.OutPoint, bool, error) {
	txid, err := n.TXID()
	if err != nil {
		return wire.OutPoint{}, false, err
	}

	index, ok, err := n.AnchorOutputIndex()
	if err != nil {
		return wire.OutPoint{}, false, err
	}

	if !ok {
		return wire.OutPoint{}, false, nil
	}

	return wire.OutPoint{
		Hash:  txid,
		Index: index,
	}, true, nil
}

// MaxCSVDelay is the largest CSV delay we accept. BIP-68 caps height-mode
// sequence values at 16 bits (65535 blocks), so any larger value is either a
// misuse of the BIP-68-encoded form (bits 22+) or a tampered proof. We reject
// both to keep the CSV maturity math comfortably inside int32.
const MaxCSVDelay uint32 = 65535

// MaxProofNodes is the maximum number of recovery nodes we accept in a single
// proof. It is a defensive bound: real recovery proofs have well under a
// thousand nodes, but the Proof constructor walks the graph to verify
// reachability and a deeply adversarial graph could otherwise exhaust memory
// or (before the BFS rewrite) stack.
const MaxProofNodes = 100_000

// Proof is an immutable recovery graph for one target outpoint.
type Proof struct {
	targetOutpoint wire.OutPoint

	// csvDelay is the post-confirmation timeout in raw blocks (NOT a
	// BIP-68-encoded sequence value). Callers working with BIP-68
	// sequences (e.g. arkscript.CSV.Lock) must decode the block count
	// before constructing a Proof.
	csvDelay uint32

	nodes       map[chainhash.Hash]*Node
	parents     map[chainhash.Hash][]chainhash.Hash
	children    map[chainhash.Hash][]chainhash.Hash
	layers      [][]chainhash.Hash
	layerByTxid map[chainhash.Hash]int
}

// NewProof constructs and validates a recovery proof for one target outpoint.
// csvDelay must be a raw block count (not a BIP-68-encoded sequence) in the
// inclusive range [0, MaxCSVDelay].
func NewProof(targetOutpoint wire.OutPoint, csvDelay uint32,
	nodes ...*Node) (*Proof, error) {

	if len(nodes) == 0 {
		return nil, fmt.Errorf("at least one node is required")
	}

	if len(nodes) > MaxProofNodes {
		return nil, fmt.Errorf("proof exceeds max node count "+
			"(%d > %d)", len(nodes), MaxProofNodes)
	}

	if csvDelay > MaxCSVDelay {
		return nil, fmt.Errorf("csv delay %d exceeds max %d "+
			"(BIP-68 height-mode limit)",
			csvDelay, MaxCSVDelay)
	}

	nodeMap := make(map[chainhash.Hash]*Node, len(nodes))
	for _, node := range nodes {
		if node == nil {
			return nil, fmt.Errorf("node cannot be nil")
		}

		txid, err := node.TXID()
		if err != nil {
			return nil, err
		}

		if _, exists := nodeMap[txid]; exists {
			return nil, fmt.Errorf("duplicate node txid %s", txid)
		}

		nodeMap[txid] = node
	}

	targetNode, ok := nodeMap[targetOutpoint.Hash]
	if !ok {
		return nil, fmt.Errorf("target txid %s not found in proof",
			targetOutpoint.Hash)
	}

	if int(targetOutpoint.Index) >= len(targetNode.Tx.TxOut) {
		return nil, fmt.Errorf("target output index %d out of bounds",
			targetOutpoint.Index)
	}

	parents := make(map[chainhash.Hash][]chainhash.Hash, len(nodeMap))
	children := make(map[chainhash.Hash][]chainhash.Hash, len(nodeMap))

	for txid, node := range nodeMap {
		seenParents := make(map[chainhash.Hash]struct{})

		for _, txIn := range node.Tx.TxIn {
			parentTxid := txIn.PreviousOutPoint.Hash
			if _, exists := nodeMap[parentTxid]; !exists {
				continue
			}

			if _, seen := seenParents[parentTxid]; seen {
				continue
			}

			seenParents[parentTxid] = struct{}{}
			parents[txid] = append(parents[txid], parentTxid)
			children[parentTxid] = append(
				children[parentTxid], txid,
			)
		}
	}

	for txid := range parents {
		sortHashes(parents[txid])
	}
	for txid := range children {
		sortHashes(children[txid])
	}

	reachable := collectReachableAncestors(
		targetOutpoint.Hash, parents,
	)

	for txid := range nodeMap {
		if _, ok := reachable[txid]; ok {
			continue
		}

		return nil, fmt.Errorf(
			"node %s does not contribute to target %s",
			txid, targetOutpoint,
		)
	}

	layers, layerByTxid, err := buildLayers(nodeMap, parents, children)
	if err != nil {
		return nil, err
	}

	return &Proof{
		targetOutpoint: targetOutpoint,
		csvDelay:       csvDelay,
		nodes:          nodeMap,
		parents:        parents,
		children:       children,
		layers:         layers,
		layerByTxid:    layerByTxid,
	}, nil
}

// TargetOutpoint returns the outpoint this proof materializes.
func (p *Proof) TargetOutpoint() wire.OutPoint {
	return p.targetOutpoint
}

// CSVDelay returns the CSV delay that applies after the target confirms.
func (p *Proof) CSVDelay() uint32 {
	return p.csvDelay
}

// Node returns the recovery node for a txid, if present.
func (p *Proof) Node(txid chainhash.Hash) (*Node, bool) {
	node, ok := p.nodes[txid]
	return node, ok
}

// TargetNode returns the node that creates the target outpoint.
func (p *Proof) TargetNode() (*Node, error) {
	node, ok := p.Node(p.targetOutpoint.Hash)
	if !ok {
		return nil, fmt.Errorf("target node %s not found",
			p.targetOutpoint.Hash)
	}

	return node, nil
}

// TargetOutput returns the txout referenced by the target outpoint.
func (p *Proof) TargetOutput() (*wire.TxOut, error) {
	node, err := p.TargetNode()
	if err != nil {
		return nil, err
	}

	return node.Output(p.targetOutpoint.Index)
}

// ParentTxids returns the in-proof parent txids for the requested node.
func (p *Proof) ParentTxids(txid chainhash.Hash) ([]chainhash.Hash, error) {
	if _, ok := p.nodes[txid]; !ok {
		return nil, fmt.Errorf("unknown txid %s", txid)
	}

	return append([]chainhash.Hash(nil), p.parents[txid]...), nil
}

// ChildTxids returns the in-proof child txids for the requested node.
func (p *Proof) ChildTxids(txid chainhash.Hash) ([]chainhash.Hash, error) {
	if _, ok := p.nodes[txid]; !ok {
		return nil, fmt.Errorf("unknown txid %s", txid)
	}

	return append([]chainhash.Hash(nil), p.children[txid]...), nil
}

// RootTxids returns the txids that have no in-proof parents.
func (p *Proof) RootTxids() []chainhash.Hash {
	if len(p.layers) == 0 {
		return nil
	}

	return append([]chainhash.Hash(nil), p.layers[0]...)
}

// Layer returns the topological layer index for the requested txid.
func (p *Proof) Layer(txid chainhash.Hash) (int, error) {
	layer, ok := p.layerByTxid[txid]
	if !ok {
		return 0, fmt.Errorf("unknown txid %s", txid)
	}

	return layer, nil
}

// Layers returns the full topological layering from roots to target.
func (p *Proof) Layers() [][]chainhash.Hash {
	result := make([][]chainhash.Hash, 0, len(p.layers))
	for _, layer := range p.layers {
		result = append(result,
			append([]chainhash.Hash(nil), layer...))
	}

	return result
}

// buildLayers computes a deterministic topological layering for the proof.
func buildLayers(nodes map[chainhash.Hash]*Node,
	parents map[chainhash.Hash][]chainhash.Hash,
	children map[chainhash.Hash][]chainhash.Hash) ([][]chainhash.Hash,
	map[chainhash.Hash]int, error) {

	indegree := make(map[chainhash.Hash]int, len(nodes))
	for txid := range nodes {
		indegree[txid] = len(parents[txid])
	}

	ready := make([]chainhash.Hash, 0)
	for txid, count := range indegree {
		if count == 0 {
			ready = append(ready, txid)
		}
	}
	sortHashes(ready)

	processed := 0
	layers := make([][]chainhash.Hash, 0)
	layerByTxid := make(map[chainhash.Hash]int, len(nodes))

	for len(ready) > 0 {
		current := append([]chainhash.Hash(nil), ready...)
		ready = nil

		layerIndex := len(layers)
		layers = append(layers, current)

		nextCounts := make(map[chainhash.Hash]struct{})
		for _, txid := range current {
			processed++
			layerByTxid[txid] = layerIndex

			for _, child := range children[txid] {
				indegree[child]--
				if indegree[child] == 0 {
					nextCounts[child] = struct{}{}
				}
			}
		}

		for txid := range nextCounts {
			ready = append(ready, txid)
		}
		sortHashes(ready)
	}

	if processed != len(nodes) {
		return nil, nil, fmt.Errorf("recovery proof contains a cycle")
	}

	return layers, layerByTxid, nil
}

// sortHashes sorts hashes deterministically by raw byte order. We compare
// the 32-byte arrays directly to avoid the two-allocation-per-comparison cost
// of the bitcoin display (hex) form.
func sortHashes(hashes []chainhash.Hash) {
	sort.Slice(hashes, func(i, j int) bool {
		return bytes.Compare(hashes[i][:], hashes[j][:]) < 0
	})
}

// collectReachableAncestors returns the set of txids reachable from start by
// walking the parents map. We traverse iteratively with an explicit queue so a
// pathological proof (e.g. a 1M-deep linear chain) cannot blow the Go
// goroutine stack. The returned set always contains start itself.
func collectReachableAncestors(start chainhash.Hash,
	parents map[chainhash.Hash][]chainhash.Hash) map[chainhash.Hash]struct{} {

	reachable := make(map[chainhash.Hash]struct{})
	queue := tree.NewQueue[chainhash.Hash]()
	queue.Enqueue(start)
	reachable[start] = struct{}{}

	for {
		txid, ok := queue.Dequeue()
		if !ok {
			break
		}

		for _, parent := range parents[txid] {
			if _, seen := reachable[parent]; seen {
				continue
			}

			reachable[parent] = struct{}{}
			queue.Enqueue(parent)
		}
	}

	return reachable
}
