package assets

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/taproot-assets/address"
	tapasset "github.com/lightninglabs/taproot-assets/asset"
	"github.com/stretchr/testify/require"
)

func TestOpTrueBTCAnchorSpec(t *testing.T) {
	spec, err := OpTrueBTCAnchorSpec()
	require.NoError(t, err)

	require.Equal(t, int64(0), spec.ValueSat)
	require.NotNil(t, spec.InternalKey)
	require.NotNil(t, spec.OutputKey)
	require.NotEmpty(t, spec.ControlBlock)
	require.NotEmpty(t, spec.TapLeaf.Script)

	tapHash := spec.TapLeaf.TapHash()
	computed := txscript.ComputeTaprootOutputKey(
		spec.InternalKey, tapHash[:],
	)
	require.True(t, spec.OutputKey.IsEqual(computed))
}

func TestNewEphemeralBTCAnchorSpec(t *testing.T) {
	spec := NewEphemeralBTCAnchorSpec()
	require.Equal(t, int64(0), spec.ValueSat)
	require.Equal(t, BTCAnchorScriptPayToAnchor, spec.ScriptType)
	require.Nil(t, spec.InternalKey)
	require.Nil(t, spec.OutputKey)
	require.Empty(t, spec.ControlBlock)
	require.Empty(t, spec.TapLeaf.Script)

	pkScript := payToAnchorPkScript()
	require.Equal(
		t, []byte{txscript.OP_1, txscript.OP_DATA_2, 0x4e, 0x73},
		pkScript,
	)
}

func TestAddBtcInput(t *testing.T) {
	var assetID tapasset.ID
	params := &address.ChainParams{}
	builder := NewAssetTxBuilder(assetID, params)

	hash := chainhash.Hash{0x01}
	outpoint := wire.OutPoint{
		Hash:  hash,
		Index: 2,
	}
	pkScript := []byte{txscript.OP_TRUE}

	require.NoError(t, builder.AddBtcInput(BtcInputSpec{
		Description: "connector-in",
		Outpoint:    outpoint,
		WitnessUtxo: &wire.TxOut{
			Value:    12345,
			PkScript: pkScript,
		},
		Sequence: 42,
	}))

	inputs := builder.BtcInputs()
	require.Len(t, inputs, 1)
	require.Equal(t, outpoint, inputs[0].Outpoint)
	require.Equal(t, uint32(42), inputs[0].Sequence)
	require.NotNil(t, inputs[0].WitnessUtxo)
	require.Equal(t, int64(12345), inputs[0].WitnessUtxo.Value)
	// Mutate the returned plan to ensure it is a defensive copy.
	inputs[0].WitnessUtxo.PkScript[0] = txscript.OP_FALSE
	refreshed := builder.BtcInputs()
	require.Equal(
		t, byte(txscript.OP_TRUE), refreshed[0].WitnessUtxo.PkScript[0],
	)

	// A missing witness UTXO should be rejected.
	err := builder.AddBtcInput(BtcInputSpec{
		Outpoint: outpoint,
	})
	require.Error(t, err)
}

func TestAddBtcOutput(t *testing.T) {
	var assetID tapasset.ID
	params := &address.ChainParams{}
	builder := NewAssetTxBuilder(assetID, params)

	script := []byte{txscript.OP_TRUE}
	require.NoError(t, builder.AddBtcOutput(BtcOutputSpec{
		Description: "connector-tree",
		ValueSat:    2100,
		PkScript:    script,
	}))

	outputs := builder.BtcOutputs()
	require.Len(t, outputs, 1)
	require.Equal(t, int64(2100), outputs[0].ValueSat)
	require.Equal(t, -1, outputs[0].OutputIndex)
	require.Equal(t, script, outputs[0].PkScript)

	// Confirm the slice is a defensive copy.
	outputs[0].PkScript[0] = txscript.OP_FALSE
	refreshed := builder.BtcOutputs()
	require.Equal(t, byte(txscript.OP_TRUE), refreshed[0].PkScript[0])

	// Negative values must fail.
	err := builder.AddBtcOutput(BtcOutputSpec{
		ValueSat: -1,
		PkScript: script,
	})
	require.Error(t, err)
}
