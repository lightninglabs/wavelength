package recovery

import (
	"bytes"
	"fmt"
	"sort"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightningnetwork/lnd/fn/v2"
)

// NodeKind identifies the type of recovery transaction a node represents.
// The kind is metadata only — it does not affect topological planning, which
// treats every node uniformly by txid and parent list. We keep the kind on
// the Node so downstream consumers (UI, logs, broadcaster policy) can make
// type-specific decisions (for example, fee-bumping policy differs between a
// tree tx and a checkpoint tx) without having to re-classify.
type NodeKind int

const (
	// NodeKindTree marks a round tree transaction — an intermediate node in
	// the VTXO Merkle tree produced by an Ark round.
	NodeKindTree NodeKind = iota

	// NodeKindCheckpoint marks a checkpoint transaction in OOR lineage. A
	// checkpoint pins a user's outgoing Ark payment to the chain before the
	// Ark tx itself is finalized.
	NodeKindCheckpoint

	// NodeKindArk marks an Ark transaction that spends checkpoint outputs
	// to produce new VTXOs. In the recovery graph it is typically the
	// target (or a near-target ancestor).
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

// Node is one recovery transaction in a proof graph. A Node is a value
// object: the planner never mutates it once it is handed to NewProof. The
// planner also does not care whether the caller hands in a signed or
// unsigned tx — txid is invariant across signing for the transaction shapes
// used in Ark recovery (segwit / taproot inputs only), so the graph
// computation is the same either way. Callers who intend to actually
// broadcast must of course sign before doing so.
type Node struct {
	// Kind describes the role of this transaction in the proof.
	Kind NodeKind

	// Tx is the unsigned or signed transaction to materialize. The txid is
	// identical in either form, so the recovery planner only requires the
	// transaction itself.
	Tx *wire.MsgTx
}

// TXID returns the transaction hash for this recovery node. It guards
// against both a nil receiver and a nil Tx so the caller can safely propagate
// the error rather than panicking on a deferred dereference.
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
// Ark transactions typically carry a single ephemeral-anchor output
// (arkscript.AnchorPkScript) that downstream fee-bumping tools (CPFP,
// package relay) target. Having two would be a policy violation by the
// tx constructor; we surface it as an error so the planner sees the bug
// immediately rather than silently picking one.
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
			return 0, false, fmt.Errorf("multiple anchor outputs " +
				"found")
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

// Proof is an immutable recovery graph for one target outpoint. All fields
// are computed by NewProof and never mutated afterwards; the Proof is safe
// to share across goroutines without synchronization.
//
// The redundancy between `parents`, `children`, `layers`, and `layerByTxid`
// is intentional: each serves a hot-path query in the planner (and saves us
// from having to traverse the DAG at plan time):
//
//   - parents  : answer "is this tx ready?" by checking the confirmation
//     state of each parent.
//   - children : answer "which nodes become ready after this one
//     confirms?" (used in the topological sort, and potentially by
//     downstream consumers that want to preempt broadcasts).
//   - layers   : answer "what should I try next?" — SnapshotAt walks layers
//     in order so earlier nodes are never reported as blocked on later
//     nodes.
//   - layerByTxid: O(1) lookup of a node's layer index when the caller
//     already holds a txid.
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

// NewProof constructs and validates a recovery proof for one target
// outpoint. csvDelay must be a raw block count (not a BIP-68-encoded
// sequence) in the inclusive range [0, MaxCSVDelay].
//
// Validation runs in five stages and short-circuits on the first failure:
//
//  1. Size & delay bounds — cheap guards against adversarial inputs before
//     we allocate any per-node state.
//  2. Node indexing — build a txid→Node map, rejecting nil nodes and
//     duplicate txids. A duplicate would make the rest of the graph
//     ambiguous (two different bodies for the same hash).
//  3. Parent/child derivation — for each node, walk its TxIn list and
//     connect only those inputs whose previous-outpoint txid is ALSO a
//     node in this proof. External inputs (e.g. the user's funding utxo
//     on a root tx) are intentionally ignored: the proof only tracks
//     in-graph dependencies. Duplicate parent edges (two inputs from the
//     same parent) are deduplicated because they represent the same
//     scheduling dependency.
//  4. Reachability — every node must be an ancestor of the target.
//     Nodes that are not reachable can never affect the target's
//     spendability, and their presence almost always indicates a caller
//     bug. We fail fast rather than silently ignore them.
//  5. Topological layering — a Kahn-style sort that both detects cycles
//     and produces the deterministic layer index every downstream
//     consumer uses for ordered broadcast.
//
// Determinism note: parent/child lists are sorted by raw txid byte order
// after construction so that two proofs built from the same node set in a
// different order produce byte-identical internal state. This matters for
// the TLV codec in proof_codec.go (its output hashes must be stable across
// invocations) and for deterministic test fixtures.
func NewProof(targetOutpoint wire.OutPoint, csvDelay uint32,
	nodes ...*Node) (*Proof, error) {

	// Stage 1: cheap size / delay bounds.
	if len(nodes) == 0 {
		return nil, fmt.Errorf("at least one node is required")
	}

	if len(nodes) > MaxProofNodes {
		return nil, fmt.Errorf("proof exceeds max node count (%d > %d)",
			len(nodes), MaxProofNodes)
	}

	if csvDelay > MaxCSVDelay {
		return nil, fmt.Errorf("csv delay %d exceeds max %d (BIP-68 "+
			"height-mode limit)", csvDelay, MaxCSVDelay)
	}

	// Stage 2: build the txid→Node index and reject nil / duplicates.
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

	// The target txid must exist in the proof; otherwise the graph cannot
	// "terminate" at the target outpoint. The output index must also fit
	// the target transaction's output count.
	targetNode, ok := nodeMap[targetOutpoint.Hash]
	if !ok {
		return nil, fmt.Errorf("target txid %s not found in proof",
			targetOutpoint.Hash)
	}

	if int(targetOutpoint.Index) >= len(targetNode.Tx.TxOut) {
		return nil, fmt.Errorf("target output index %d out of bounds",
			targetOutpoint.Index)
	}

	// Stage 3: derive in-graph parent/child adjacencies.
	//
	// Only transaction inputs whose previous-outpoint txid is ALSO a node
	// in this proof become edges. External inputs (e.g. the user's
	// funding utxo that seeds a root tx) are intentionally invisible
	// here: the proof tracks scheduling dependencies, not the full input
	// graph.
	parents := make(map[chainhash.Hash][]chainhash.Hash, len(nodeMap))
	children := make(map[chainhash.Hash][]chainhash.Hash, len(nodeMap))

	for txid, node := range nodeMap {
		// A tx may legitimately spend the same parent at multiple
		// outputs (e.g. to both an amount output and an anchor output
		// sweep). Collapse those to a single edge — they express one
		// scheduling dependency, not many.
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

	// Sort edges deterministically so the Proof's internal state is a
	// canonical function of its inputs regardless of Go map iteration
	// order on the outer loop above.
	for txid := range parents {
		sortHashes(parents[txid])
	}
	for txid := range children {
		sortHashes(children[txid])
	}

	// Stage 4: reachability. Every node must be an ancestor of the
	// target; otherwise it can't possibly affect the target's
	// spendability and its presence is a caller bug. We do this in its
	// own iterative BFS (see collectReachableAncestors) rather than
	// folding it into the topological sort because (a) the failure
	// message is more actionable — we tell the caller exactly which
	// node is orphaned — and (b) the BFS is naturally stack-safe against
	// adversarial depth.
	reachable := collectReachableAncestors(
		targetOutpoint.Hash, parents,
	)

	for txid := range nodeMap {
		if reachable.Contains(txid) {
			continue
		}

		return nil, fmt.Errorf("node %s does not contribute to "+
			"target %s", txid, targetOutpoint)
	}

	// Stage 5: topological layering. This both detects cycles (any node
	// that can't be processed leaves `processed < len(nodes)`, which
	// buildLayers surfaces as an explicit "contains a cycle" error) and
	// produces the layer index every downstream consumer uses to walk
	// the graph in dependency order without recursion.
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
// The returned value is a raw block count (always in [0, MaxCSVDelay]) so
// callers can add it to a block height without further validation.
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
// The returned slice is a defensive copy; the caller may freely mutate it
// without affecting the immutable Proof.
func (p *Proof) ParentTxids(txid chainhash.Hash) ([]chainhash.Hash, error) {
	if _, ok := p.nodes[txid]; !ok {
		return nil, fmt.Errorf("unknown txid %s", txid)
	}

	return append([]chainhash.Hash(nil), p.parents[txid]...), nil
}

// ChildTxids returns the in-proof child txids for the requested node. Like
// ParentTxids, the returned slice is a defensive copy.
func (p *Proof) ChildTxids(txid chainhash.Hash) ([]chainhash.Hash, error) {
	if _, ok := p.nodes[txid]; !ok {
		return nil, fmt.Errorf("unknown txid %s", txid)
	}

	return append([]chainhash.Hash(nil), p.children[txid]...), nil
}

// RootTxids returns the txids that have no in-proof parents. These are
// the first transactions the caller has to broadcast; every other node
// transitively depends on at least one of them.
func (p *Proof) RootTxids() []chainhash.Hash {
	if len(p.layers) == 0 {
		return nil
	}

	return append([]chainhash.Hash(nil), p.layers[0]...)
}

// Layer returns the topological layer index for the requested txid. Layer 0
// is the set of roots; the target node's layer is always the maximum layer
// index.
func (p *Proof) Layer(txid chainhash.Hash) (int, error) {
	layer, ok := p.layerByTxid[txid]
	if !ok {
		return 0, fmt.Errorf("unknown txid %s", txid)
	}

	return layer, nil
}

// Layers returns the full topological layering from roots to target. The
// result is a fresh two-level slice copy so consumers can freely mutate it.
// Consumers that only need to read should consider caching the result
// rather than calling Layers in a hot loop.
func (p *Proof) Layers() [][]chainhash.Hash {
	result := make([][]chainhash.Hash, 0, len(p.layers))
	for _, layer := range p.layers {
		result = append(result,
			append([]chainhash.Hash(nil), layer...))
	}

	return result
}

// buildLayers computes a deterministic topological layering for the proof.
//
// This is Kahn's algorithm, with two notable twists:
//
//  1. We emit "layers" (sets of nodes with no remaining unprocessed
//     parents) rather than a single flat topological order. Grouping
//     lets the planner reason about parallelism: every node in layer N
//     can, in principle, be broadcast concurrently once layer N-1 is
//     confirmed. The planner in `unrollplan` doesn't currently exploit
//     this, but the shape is there for a future implementation.
//  2. The `ready` frontier is sorted (raw byte order) between rounds so
//     the output is deterministic. Without the sort, Go's randomized
//     map iteration would produce different `layers[i]` orderings on
//     different runs, which would break the canonical TLV encoding.
//
// Cycle detection is a side-effect of the Kahn invariant: if the graph
// contains a cycle, no node in the cycle ever reaches in-degree zero, so
// the loop exits with `processed < len(nodes)`. We report that as an
// explicit "cycle" error rather than hiding it behind a generic "could
// not build layers" message so a debugger knows exactly what's wrong.
func buildLayers(nodes map[chainhash.Hash]*Node,
	parents map[chainhash.Hash][]chainhash.Hash,
	children map[chainhash.Hash][]chainhash.Hash) ([][]chainhash.Hash,
	map[chainhash.Hash]int, error) {

	// Seed the in-degree table. Every node starts with an in-degree
	// equal to its in-graph parent count (external inputs are excluded
	// from `parents` by NewProof, so they don't inflate this).
	indegree := make(map[chainhash.Hash]int, len(nodes))
	for txid := range nodes {
		indegree[txid] = len(parents[txid])
	}

	// The initial ready frontier is every node with no parents — the
	// roots of the DAG.
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
		// Snapshot the current frontier as the next layer. We copy so
		// the layers slice doesn't alias the `ready` backing array we
		// reuse below.
		current := append([]chainhash.Hash(nil), ready...)
		ready = nil

		layerIndex := len(layers)
		layers = append(layers, current)

		// Track which children become newly-ready so we don't add the
		// same child twice if it has multiple parents in the current
		// layer.
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

	// If we processed fewer nodes than exist, the remainder is a cycle
	// (or strongly-connected component); there is no safe broadcast
	// order for it, so we reject the proof.
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
	parents map[chainhash.Hash][]chainhash.Hash) fn.Set[chainhash.Hash] {

	reachable := fn.NewSet[chainhash.Hash]()
	queue := tree.NewQueue[chainhash.Hash]()
	queue.Enqueue(start)
	reachable.Add(start)

	for {
		txid, ok := queue.Dequeue()
		if !ok {
			break
		}

		for _, parent := range parents[txid] {
			if reachable.Contains(parent) {
				continue
			}

			reachable.Add(parent)
			queue.Enqueue(parent)
		}
	}

	return reachable
}
