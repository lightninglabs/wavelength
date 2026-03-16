package oor

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
	"github.com/stretchr/testify/require"
)

// TestApplyFinalizeDataAttachesTapTree asserts that finalization attaches the
// per-input tap tree metadata from the Ark tx PSBT input onto the corresponding
// checkpoint PSBT output.
func TestApplyFinalizeDataAttachesTapTree(t *testing.T) {
	t.Parallel()

	checkpointTx := wire.NewMsgTx(3)
	checkpointTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{1},
			Index: 7,
		},
	})
	checkpointTx.AddTxOut(&wire.TxOut{
		Value:    1234,
		PkScript: []byte{0x51},
	})

	checkpointPsbt, err := psbt.NewFromUnsignedTx(checkpointTx)
	require.NoError(t, err)

	checkpointOutpoint := wire.OutPoint{
		Hash:  checkpointTx.TxHash(),
		Index: 0,
	}

	arkTx := wire.NewMsgTx(3)
	arkTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: checkpointOutpoint,
	})
	arkTx.AddTxOut(&wire.TxOut{
		Value:    1234,
		PkScript: []byte{0x6a, 0x01, 0x01},
	})
	arkTx.AddTxOut(arkscript.AnchorOutput())

	arkPsbt, err := psbt.NewFromUnsignedTx(arkTx)
	require.NoError(t, err)

	encodedTapTree, err := EncodeTapTree([][]byte{
		{0x51, 0x52, 0x53},
		{0x6a},
	})
	require.NoError(t, err)

	err = PutTapTreePSBTInput(arkPsbt, 0, encodedTapTree)
	require.NoError(t, err)

	err = ApplyFinalizeData(arkPsbt, []*psbt.Packet{checkpointPsbt})
	require.NoError(t, err)

	checkpointTapTree := checkpointPsbt.Outputs[0].TaprootTapTree
	require.Equal(t, encodedTapTree, checkpointTapTree)
}

// TestApplyFinalizeDataMissingTapTree asserts we fail if the Ark PSBT input
// does not include the tap tree metadata required for finalization.
func TestApplyFinalizeDataMissingTapTree(t *testing.T) {
	t.Parallel()

	checkpointTx := wire.NewMsgTx(3)
	checkpointTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{1},
			Index: 7,
		},
	})
	checkpointTx.AddTxOut(&wire.TxOut{
		Value:    1234,
		PkScript: []byte{0x51},
	})

	checkpointPsbt, err := psbt.NewFromUnsignedTx(checkpointTx)
	require.NoError(t, err)

	checkpointOutpoint := wire.OutPoint{
		Hash:  checkpointTx.TxHash(),
		Index: 0,
	}

	arkTx := wire.NewMsgTx(3)
	arkTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: checkpointOutpoint,
	})
	arkTx.AddTxOut(&wire.TxOut{
		Value:    1234,
		PkScript: []byte{0x6a, 0x01, 0x01},
	})
	arkTx.AddTxOut(arkscript.AnchorOutput())

	arkPsbt, err := psbt.NewFromUnsignedTx(arkTx)
	require.NoError(t, err)

	err = ApplyFinalizeData(arkPsbt, []*psbt.Packet{checkpointPsbt})
	require.Error(t, err)
}

// TestApplyFinalizeDataMappingMismatch asserts we fail if the Ark tx input does
// not correspond to any checkpoint tx output outpoint.
func TestApplyFinalizeDataMappingMismatch(t *testing.T) {
	t.Parallel()

	checkpointTx := wire.NewMsgTx(3)
	checkpointTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{1},
			Index: 7,
		},
	})
	checkpointTx.AddTxOut(&wire.TxOut{
		Value:    1234,
		PkScript: []byte{0x51},
	})

	checkpointPsbt, err := psbt.NewFromUnsignedTx(checkpointTx)
	require.NoError(t, err)

	arkTx := wire.NewMsgTx(3)
	arkTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{9},
			Index: 0,
		},
	})
	arkTx.AddTxOut(&wire.TxOut{
		Value:    1234,
		PkScript: []byte{0x6a, 0x01, 0x01},
	})
	arkTx.AddTxOut(arkscript.AnchorOutput())

	arkPsbt, err := psbt.NewFromUnsignedTx(arkTx)
	require.NoError(t, err)

	encodedTapTree, err := EncodeTapTree([][]byte{{0x51}})
	require.NoError(t, err)

	err = PutTapTreePSBTInput(arkPsbt, 0, encodedTapTree)
	require.NoError(t, err)

	err = ApplyFinalizeData(arkPsbt, []*psbt.Packet{checkpointPsbt})
	require.Error(t, err)
}
