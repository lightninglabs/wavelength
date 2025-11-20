package tree

import (
	"sort"
)

// PartitionWeightFunc returns the weight used to balance leaves during tree
// construction. When nil, leaves are partitioned purely by count.
type PartitionWeightFunc func(LeafDescriptor) int64

// WeightByBtcAmount is a partition function that simply prefers the BTC amount.
func WeightByBtcAmount() PartitionWeightFunc {
	return func(l LeafDescriptor) int64 {
		return int64(l.Amount)
	}
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
