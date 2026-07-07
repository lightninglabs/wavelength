package tree

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/wire/v2"
)

// MaterializeParams contains the parameters for materializing a single node.
type MaterializeParams struct {
	// Input is the outpoint this node spends.
	Input wire.OutPoint
}

// Materializer fills in transaction data for a tree structure. Different
// implementations handle BTC-only vs Asset trees.
type Materializer interface {
	// MaterializeNode fills in Input, Outputs, FinalKey, and OutputsMeta
	// for a single node. Returns child params for each child index.
	MaterializeNode(ctx context.Context, node *Node,
		params MaterializeParams) (map[uint32]MaterializeParams, error)
}

// materializeItem represents a node and its parameters in the materialization
// queue. Named to avoid conflict with workItem in build.go.
type materializeItem struct {
	node   *Node
	params MaterializeParams
}

// Materialize walks the tree top-down iteratively and materializes each node
// using the provided Materializer. This is pass 2 of the two-pass tree
// construction.
//
// The function uses an explicit stack for depth-first traversal, avoiding
// recursion to prevent stack overflow for deep trees.
func Materialize(ctx context.Context, root *Node, rootParams MaterializeParams,
	mat Materializer) error {

	if root == nil {
		return nil
	}

	// Use a stack for depth-first traversal. Parent must be materialized
	// before children since we need the parent's TXID for child inputs.
	stack := []materializeItem{{node: root, params: rootParams}}

	for len(stack) > 0 {
		// Pop from stack.
		item := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		// Materialize this node.
		childParams, err := mat.MaterializeNode(
			ctx, item.node, item.params,
		)
		if err != nil {
			return fmt.Errorf("materialize node: %w", err)
		}

		// Push children onto stack in reverse order for correct DFS
		// ordering (so first child is processed first).
		indices := sortedChildIndices(item.node.Children)
		for i := len(indices) - 1; i >= 0; i-- {
			idx := indices[i]
			child := item.node.Children[idx]

			params, ok := childParams[idx]
			if !ok {
				return fmt.Errorf("missing params for child %d",
					idx)
			}

			stack = append(stack, materializeItem{
				node:   child,
				params: params,
			})
		}
	}

	return nil
}
