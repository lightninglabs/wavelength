package tree

import (
	"fmt"

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

// buildTreeBFS builds the tree using breadth-first search with a work queue.
func buildTreeBFS(rootInput wire.OutPoint, leaves []LeafDescriptor,
	operatorKey *btcec.PublicKey, sweepTapscriptRoot []byte,
	radix int) (*Node, error) {

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
			node, err = NewLeafNode(
				work.input, work.leaves[0], operatorKey,
				sweepTapscriptRoot,
			)
			if err != nil {
				return nil, fmt.Errorf("failed to create "+
					"leaf node: %w", err)
			}
		} else {
			// Partition leaves into balanced groups.
			groups := partitionLeaves(work.leaves, radix)

			// Create branch transaction.
			node, err = NewBranchNode(
				work.input, groups, operatorKey,
				sweepTapscriptRoot,
			)
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

// partitionLeaves divides leaves into balanced groups using round-robin
// assignment. It ensures even distribution of items across groups to create a
// balanced tree.
func partitionLeaves(leaves []LeafDescriptor, radix int) [][]LeafDescriptor {
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
