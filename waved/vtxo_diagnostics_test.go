package waved

import (
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/tree"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/stretchr/testify/require"
)

// TestDiagnosticAnchorsPreserveActualOutputs pins nonzero vouts, multiple
// commitments, shared fragments and unknown ancestry without exporting keys.
func TestDiagnosticAnchorsPreserveActualOutputs(t *testing.T) {
	first := &tree.Tree{
		BatchOutpoint: wire.OutPoint{
			Hash: chainhash.Hash{
				1,
			},
			Index: 7,
		},
		BatchOutput: &wire.TxOut{
			Value: 250000,
			PkScript: []byte{
				0x51,
			},
		},
	}
	second := &tree.Tree{
		BatchOutpoint: wire.OutPoint{
			Hash: chainhash.Hash{
				2,
			},
			Index: 3,
		},
		BatchOutput: &wire.TxOut{
			Value: 700000,
			PkScript: []byte{
				0x52,
			},
		},
	}
	desc := &vtxo.Descriptor{Ancestry: []vtxo.Ancestry{
		{
			TreePath: first,
		}, {
			TreePath:         first,
			CommitmentHeight: 123,
		},
		{
			TreePath:         second,
			CommitmentHeight: 124,
		}, {},
	}}
	anchors := diagnosticAnchors(desc)
	require.Len(t, anchors, 2)
	require.Equal(t, first.BatchOutpoint.String(), anchors[0].Outpoint)
	require.EqualValues(t, 250000, anchors[0].AmountSat)
	require.EqualValues(t, 123, anchors[0].ConfirmationHeight)
	require.Equal(t, "51", anchors[0].PkScript)
	require.Equal(t, second.BatchOutpoint.String(), anchors[1].Outpoint)
	require.Empty(t, diagnosticAnchors(&vtxo.Descriptor{}))
}
