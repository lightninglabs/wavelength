package waved

import (
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/stretchr/testify/require"
)

// sourceWatcherTree creates a one-node ancestry tree and its VTXO leaf.
func sourceWatcherTree(t *testing.T, seed byte) (*tree.Tree, wire.OutPoint) {
	t.Helper()
	root := &tree.Node{
		Input: sourceWatcherOutpoint(seed),
		Outputs: []*wire.TxOut{
			{
				Value: 1_000,
				PkScript: []byte{
					0x51,
					seed,
				},
			},
		},
		Children: make(map[uint32]*tree.Node),
	}
	source, err := root.GetNonAnchorOutpoint()
	require.NoError(t, err)

	return &tree.Tree{
		Root: root,
		BatchOutput: &wire.TxOut{
			Value: 1_000,
			PkScript: []byte{
				0x51,
				seed,
				0,
			},
		},
	}, *source
}

// sourceWatcherOutpoint returns one deterministic test outpoint.
func sourceWatcherOutpoint(seed byte) wire.OutPoint {
	var hash chainhash.Hash
	hash[0] = seed

	return wire.OutPoint{Hash: hash}
}
