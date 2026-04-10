package indexer

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
)

// TestCombineVirtualLineageAllowsInheritedMissingTreePath verifies that a
// virtual child can inherit lineage from a parent that already lacks a unique
// commitment path, instead of failing the query outright.
func TestCombineVirtualLineageAllowsInheritedMissingTreePath(t *testing.T) {
	t.Parallel()

	resolver := &lineageResolver{}
	parentOutpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("parent")),
		Index: 1,
	}
	childOutpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("child")),
		Index: 0,
	}
	parentLineage := &vtxoLineage{
		roundID:        "round-test",
		commitmentTxID: chainhash.HashH([]byte("commitment")),
		batchExpiry:    144,
		relativeExpiry: 12,
		treeDepth:      0,
		chainDepth:     1,
		createdHeight:  99,
	}

	lineage, err := resolver.combineVirtualLineage(
		t.Context(),
		childOutpoint,
		[]VTXORow{{
			Outpoint: parentOutpoint,
		}},
		[]wire.OutPoint{parentOutpoint},
		[]*vtxoLineage{parentLineage},
	)
	require.NoError(t, err)
	require.Equal(t, "round-test", lineage.roundID)
	require.Equal(t, parentLineage.commitmentTxID,
		lineage.commitmentTxID)
	require.Equal(t, int32(144), lineage.batchExpiry)
	require.Equal(t, uint32(12), lineage.relativeExpiry)
	require.Equal(t, 0, lineage.treeDepth)
	require.Equal(t, 2, lineage.chainDepth)
	require.Equal(t, int32(99), lineage.createdHeight)
	require.Nil(t, lineage.treePath)
	require.Nil(t, lineage.treePathTLV)
}
