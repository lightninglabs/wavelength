package tree

import (
	"fmt"
	"sort"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
)

// workItem represents a unit of work in the tree building queue.
type workItem struct {
	input    wire.OutPoint
	leaves   []LeafDescriptor
	parent   *Node
	outIndex uint32
}

// PartitionWeightFunc returns the weight used to balance leaves during tree
// construction. When nil, leaves are partitioned purely by count.
type PartitionWeightFunc func(LeafDescriptor) int64

// WeightByAssetAmountOrBTC prefers the asset amount (if present and non-zero)
// and falls back to the BTC amount otherwise.
func WeightByAssetAmountOrBTC() PartitionWeightFunc {
	return func(l LeafDescriptor) int64 {
		if l.Asset != nil && l.Asset.AssetAmount > 0 {
			return int64(l.Asset.AssetAmount)
		}
		return int64(l.Amount)
	}
}

// NodeAssembler constructs branch/leaf nodes for the BFS builder. The BTC
// assembler mirrors the legacy behaviour while asset-aware assemblers can
// inject additional witness metadata.
type NodeAssembler interface {
	MakeLeaf(input wire.OutPoint, leaf LeafDescriptor) (*Node, error)
	MakeBranch(input wire.OutPoint, groups [][]LeafDescriptor) (*Node, error)
}

// btcAssembler preserves the historical BTC-only node construction.
type btcAssembler struct {
	operatorKey        *btcec.PublicKey
	sweepTapscriptRoot []byte
}

// newBTCAssembler returns a BTC-only assembler.
func newBTCAssembler(operatorKey *btcec.PublicKey,
	sweepTapscriptRoot []byte) *btcAssembler {

	return &btcAssembler{
		operatorKey:        operatorKey,
		sweepTapscriptRoot: sweepTapscriptRoot,
	}
}

func (b *btcAssembler) MakeLeaf(input wire.OutPoint,
	leaf LeafDescriptor) (*Node, error) {

	return NewLeafNode(input, leaf, b.operatorKey, b.sweepTapscriptRoot)
}

func (b *btcAssembler) MakeBranch(input wire.OutPoint,
	groups [][]LeafDescriptor) (*Node, error) {

	return NewBranchNode(
		input, groups, b.operatorKey, b.sweepTapscriptRoot,
	)
}

// buildTreeBFS builds the tree using breadth-first search with a work queue.
func buildTreeBFS(rootInput wire.OutPoint, leaves []LeafDescriptor,
	assembler NodeAssembler, radix int,
	weightFn PartitionWeightFunc) (*Node, error) {

	// Initialize queue with root work item.
	queue := NewQueue[workItem]()
	queue.Enqueue(workItem{
		input:    rootInput,
		leaves:   leaves,
		parent:   nil,
		outIndex: 0,
	})

	var root *Node

	// Process queue iteratively (BFS).
	for !queue.IsEmpty() {
		work, ok := queue.Dequeue()
		if !ok {
			return nil, fmt.Errorf("unexpected empty queue")
		}

		var node *Node
		var err error

		// Base case: single leaf creates a leaf transaction.
		if len(work.leaves) == 1 {
			node, err = assembler.MakeLeaf(work.input, work.leaves[0])
			if err != nil {
				return nil, fmt.Errorf("failed to create "+
					"leaf node: %w", err)
			}
		} else {
			// Partition leaves into balanced groups.
			groups := partitionLeaves(work.leaves, radix, weightFn)

			// Create branch transaction.
			node, err = assembler.MakeBranch(work.input, groups)
			if err != nil {
				return nil, fmt.Errorf("failed to create "+
					"branch node: %w", err)
			}

			// Enqueue children for processing.
			parentTxHash, err := node.TXID()
			if err != nil {
				return nil, fmt.Errorf("failed to get "+
					"parent TXID: %w", err)
			}

			for i, group := range groups {
				if len(group) == 0 {
					continue
				}

				childInput := wire.OutPoint{
					Hash:  parentTxHash,
					Index: uint32(i),
				}

				queue.Enqueue(workItem{
					input:    childInput,
					leaves:   group,
					parent:   node,
					outIndex: uint32(i),
				})
			}
		}

		// Link to parent.
		if work.parent != nil {
			work.parent.Children[work.outIndex] = node
		} else {
			root = node
		}
	}

	return root, nil
}

// partitionLeaves divides leaves into balanced groups. When a weight function
// is provided, groups are balanced by cumulative weight; otherwise, leaves are
// distributed purely by count (legacy behavior).
func partitionLeaves(leaves []LeafDescriptor, radix int,
	weightFn PartitionWeightFunc) [][]LeafDescriptor {

	if weightFn == nil {
		return partitionLeavesByCount(leaves, radix)
	}

	return partitionLeavesByWeight(leaves, radix, weightFn)
}

// partitionLeavesByCount distributes leaves evenly by count.
func partitionLeavesByCount(leaves []LeafDescriptor,
	radix int) [][]LeafDescriptor {

	M := len(leaves)

	if M <= radix {
		// Each leaf gets its own group.
		groups := make([][]LeafDescriptor, M)
		for i, leaf := range leaves {
			groups[i] = []LeafDescriptor{leaf}
		}

		return groups
	}

	// Calculate target sizes: distribute M items into radix groups.
	base := M / radix
	extra := M % radix
	sizes := make([]int, radix)
	for i := 0; i < radix; i++ {
		sizes[i] = base
		if i < extra {
			// First 'extra' groups get one more item.
			sizes[i]++
		}
	}

	// Round-robin assignment with capacity tracking.
	groups := make([][]LeafDescriptor, radix)
	for i := range groups {
		groups[i] = make([]LeafDescriptor, 0, sizes[i])
	}
	caps := make([]int, radix)
	copy(caps, sizes)

	idx := 0
	for _, leaf := range leaves {
		// Find next group with capacity (cyclic).
		for caps[idx] == 0 {
			idx = (idx + 1) % radix
		}
		groups[idx] = append(groups[idx], leaf)
		caps[idx]--
		idx = (idx + 1) % radix
	}

	// Safety: ensure at least 2 non-empty groups when M > 1.
	nonEmpty := make([][]LeafDescriptor, 0, radix)
	for _, g := range groups {
		if len(g) > 0 {
			nonEmpty = append(nonEmpty, g)
		}
	}

	if len(nonEmpty) <= 1 && M > 1 {
		// Fallback: split in half to guarantee progress.
		mid := M / 2
		return [][]LeafDescriptor{leaves[:mid], leaves[mid:]}
	}

	return nonEmpty
}

// partitionLeavesByWeight balances groups by cumulative weight using a greedy
// assignment of sorted leaves.
func partitionLeavesByWeight(leaves []LeafDescriptor, radix int,
	weightFn PartitionWeightFunc) [][]LeafDescriptor {

	type weightedLeaf struct {
		leaf   LeafDescriptor
		weight int64
		index  int
	}

	weighted := make([]weightedLeaf, len(leaves))
	for i, leaf := range leaves {
		w := weightFn(leaf)
		if w < 0 {
			w = 0
		}
		weighted[i] = weightedLeaf{
			leaf:   leaf,
			weight: w,
			index:  i,
		}
	}

	sort.SliceStable(weighted, func(i, j int) bool {
		if weighted[i].weight == weighted[j].weight {
			return weighted[i].index < weighted[j].index
		}
		return weighted[i].weight > weighted[j].weight
	})

	groups := make([][]LeafDescriptor, radix)
	groupWeights := make([]int64, radix)

	for _, wl := range weighted {
		// Assign to the group with the smallest cumulative weight.
		minIdx := 0
		for i := 1; i < radix; i++ {
			if groupWeights[i] < groupWeights[minIdx] {
				minIdx = i
			}
		}

		groups[minIdx] = append(groups[minIdx], wl.leaf)
		groupWeights[minIdx] += wl.weight
	}

	// Drop empty groups to preserve existing expectations.
	nonEmpty := make([][]LeafDescriptor, 0, radix)
	for _, g := range groups {
		if len(g) > 0 {
			nonEmpty = append(nonEmpty, g)
		}
	}

	return nonEmpty
}
