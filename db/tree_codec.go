package db

import (
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/lib/treecodec"
)

// MaxTreeDeserializeDepth bounds persisted tree depth before allocation.
const MaxTreeDeserializeDepth = treecodec.MaxTreeDeserializeDepth

// MaxTreeChildrenPerNode bounds each persisted node's children map.
const MaxTreeChildrenPerNode = treecodec.MaxTreeChildrenPerNode

// SerializeTree retains the existing database tree encoding and identity bytes.
func SerializeTree(t *tree.Tree) ([]byte, error) {
	return treecodec.SerializeTree(
		t,
	)
}

// DeserializeTree restores database trees through the shared bounded decoder.
func DeserializeTree(raw []byte) (*tree.Tree, error) {
	return treecodec.DeserializeTree(
		raw,
	)
}
