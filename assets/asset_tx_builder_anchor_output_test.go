package assets

import (
	"testing"

	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
)

func TestAssetTxPlanFirstAnchorOutput(t *testing.T) {
	plan := &AssetTxPlan{
		AnchorOutputs: []*wire.TxOut{
			{
				Value:    1234,
				PkScript: []byte{0x51},
			},
		},
	}

	first := plan.FirstAnchorOutput()
	require.NotNil(t, first)
	require.EqualValues(t, 1234, first.Value)
	require.Equal(t, []byte{0x51}, first.PkScript)

	// Ensure it is a defensive copy.
	first.PkScript[0] = 0x52
	require.Equal(t, byte(0x51), plan.AnchorOutputs[0].PkScript[0])

	// Empty plan yields nil.
	empty := (&AssetTxPlan{}).FirstAnchorOutput()
	require.Nil(t, empty)
}
