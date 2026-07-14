package treeconv

import (
	"github.com/lightninglabs/wavelength/arkrpc"
	"github.com/lightninglabs/wavelength/lib/tree"
)

// TreePathFromTree converts a tree.Tree into its proto TreePath
// representation.
func TreePathFromTree(t *tree.Tree) (*arkrpc.TreePath, error) {
	return arkrpc.TreePathFromTree(t)
}

// TreePathToTree converts a proto TreePath back into a tree.Tree.
func TreePathToTree(tp *arkrpc.TreePath) (*tree.Tree, error) {
	return arkrpc.TreePathToTree(tp)
}
