package db

import (
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/lib/treecodec"
)

const (
	// MaxTreeDeserializeDepth preserves the database codec's depth bound.
	MaxTreeDeserializeDepth = treecodec.MaxTreeDeserializeDepth

	// MaxTreeChildrenPerNode preserves the database codec's fanout bound.
	MaxTreeChildrenPerNode = treecodec.MaxTreeChildrenPerNode
)

// SerializeTree uses the shared TLV representation for database rows and
// actor checkpoints. Existing database encodings remain byte-compatible.
func SerializeTree(t *tree.Tree) ([]byte, error) {
	return treecodec.SerializeTree(t)
}

// DeserializeTree restores a tree using the shared bounded decoder.
func DeserializeTree(data []byte) (*tree.Tree, error) {
	return treecodec.DeserializeTree(data)
}
