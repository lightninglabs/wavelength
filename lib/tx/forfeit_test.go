package tx_test

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/internal/testutils"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/lib/tx"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// TestForfeitTransactionFlow verifies that a forfeit transaction can be fully
// constructed, signed, and validated along with the operator's penalty sweep.
func TestForfeitTransactionFlow(t *testing.T) {
	t.Parallel()

	operatorKey, operatorWallet := testutils.CreateKey(1)
	operatorKeyDesc := &keychain.KeyDescriptor{
		PubKey:     operatorKey,
		KeyLocator: keychain.KeyLocator{Family: 1, Index: 0},
	}

	sweepKey, _ := testutils.CreateKey(2)

	clientKey, clientWallet := testutils.CreateKey(10)
	clientKeyDesc := &keychain.KeyDescriptor{
		PubKey:     clientKey,
		KeyLocator: keychain.KeyLocator{Family: 10, Index: 0},
	}

	const exitDelay = 144
	vtxoAmount := btcutil.Amount(5_000)

	vtxoDesc, err := tree.NewVTXODescriptor(
		vtxoAmount, clientKey, operatorKey, exitDelay,
	)
	require.NoError(t, err)

	batchOutput, err := tree.BuildBatchOutput(
		[]tree.VTXODescriptor{*vtxoDesc}, operatorKey, sweepKey,
		exitDelay,
	)
	require.NoError(t, err)

	batchOutpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("forfeit-commitment")),
		Index: 0,
	}

	vtxoTree, err := tree.BuildVTXOTree(
		batchOutpoint, batchOutput, []tree.VTXODescriptor{*vtxoDesc},
		operatorKey, sweepKey, exitDelay, 2,
	)
	require.NoError(t, err)

	leaf := vtxoTree.Root.GetLeafNodes()[0]
	vtxoOutpoint, err := leaf.GetNonAnchorOutpoint()
	require.NoError(t, err)

	vtxoOutput := nonAnchorOutput(t, leaf)
	require.NotNil(t, vtxoOutput)

	vtxoTapScript, err := scripts.VTXOTapScript(
		clientKey, operatorKey, exitDelay,
	)
	require.NoError(t, err)

	vtxoCtx := &tx.VTXOSpendContext{
		Outpoint:  *vtxoOutpoint,
		Output:    vtxoOutput,
		TapScript: vtxoTapScript,
	}

	connectorTapKey := txscript.ComputeTaprootOutputKey(operatorKey, nil)
	connectorScript, err := txscript.PayToTaprootScript(connectorTapKey)
	require.NoError(t, err)

	connectorDesc := tree.ConnectorDescriptor{
		PkScript:  connectorScript,
		NumLeaves: 8,
		Amount:    btcutil.Amount(330),
	}

	connectorOutput, err := tree.BuildConnectorOutput(
		connectorDesc.NumLeaves, connectorDesc.Amount,
		mustTaprootAddr(connectorTapKey),
	)
	require.NoError(t, err)

	connectorOutpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("connector-forfeit")),
		Index: 0,
	}

	connectorTree, err := tree.BuildConnectorTree(
		connectorOutpoint, connectorOutput, connectorDesc,
		operatorKey, 2,
	)
	require.NoError(t, err)

	connectorPath, err := connectorTree.ExtractPathForIndex(3)
	require.NoError(t, err)
	connectorLeaf := connectorPath.Root.GetLeafNodes()[0]

	connectorLeafOutpoint, err := connectorLeaf.GetNonAnchorOutpoint()
	require.NoError(t, err)

	connectorLeafOutput := nonAnchorOutput(t, connectorLeaf)
	require.NotNil(t, connectorLeafOutput)

	serverForfeitKey := txscript.ComputeTaprootOutputKey(
		operatorKey, nil,
	)
	serverForfeitScript, err := txscript.PayToTaprootScript(
		serverForfeitKey,
	)
	require.NoError(t, err)

	forfeitTx, err := tx.BuildForfeitTx(
		vtxoOutpoint, vtxoAmount, connectorLeafOutpoint,
		serverForfeitScript,
	)
	require.NoError(t, err)

	prevFetcher, err := tx.NewForfeitPrevOutFetcher(
		vtxoCtx, &tx.ConnectorSpendContext{
			Outpoint: *connectorLeafOutpoint,
			Output:   connectorLeafOutput,
		},
	)
	require.NoError(t, err)

	sigHashes := txscript.NewTxSigHashes(forfeitTx, prevFetcher)

	clientSignDesc, spendInfo, err := tx.NewVTXOCollabSignDescriptor(
		vtxoCtx, *clientKeyDesc, tx.ForfeitVTXOInputIndex,
		sigHashes, prevFetcher,
	)
	require.NoError(t, err)

	clientSig, err := clientWallet.SignOutputRaw(forfeitTx, clientSignDesc)
	require.NoError(t, err)

	operatorSignDesc, _, err := tx.NewVTXOCollabSignDescriptor(
		vtxoCtx, *operatorKeyDesc, tx.ForfeitVTXOInputIndex,
		sigHashes, prevFetcher,
	)
	require.NoError(t, err)

	operatorSig, err := operatorWallet.SignOutputRaw(
		forfeitTx, operatorSignDesc,
	)
	require.NoError(t, err)

	witness, err := scripts.VTXOCollabSpendWitness(
		clientSig, operatorSig, spendInfo,
	)
	require.NoError(t, err)
	forfeitTx.TxIn[tx.ForfeitVTXOInputIndex].Witness = witness

	connectorSignDesc := &input.SignDescriptor{
		KeyDesc:           *operatorKeyDesc,
		Output:            connectorLeafOutput,
		HashType:          txscript.SigHashDefault,
		InputIndex:        tx.ForfeitConnectorInputIndex,
		SignMethod:        input.TaprootKeySpendBIP0086SignMethod,
		SigHashes:         sigHashes,
		PrevOutputFetcher: prevFetcher,
		TapTweak:          []byte{},
	}

	connectorSig, err := operatorWallet.SignOutputRaw(
		forfeitTx, connectorSignDesc,
	)
	require.NoError(t, err)
	forfeitTx.TxIn[tx.ForfeitConnectorInputIndex].Witness = wire.TxWitness{
		connectorSig.Serialize(),
	}

	engine, err := txscript.NewEngine(
		vtxoOutput.PkScript, forfeitTx, tx.ForfeitVTXOInputIndex,
		txscript.StandardVerifyFlags, nil, sigHashes,
		vtxoOutput.Value, prevFetcher,
	)
	require.NoError(t, err)
	require.NoError(t, engine.Execute())

	engine, err = txscript.NewEngine(
		connectorLeafOutput.PkScript, forfeitTx,
		tx.ForfeitConnectorInputIndex,
		txscript.StandardVerifyFlags, nil, sigHashes,
		connectorLeafOutput.Value, prevFetcher,
	)
	require.NoError(t, err)
	require.NoError(t, engine.Execute())
}

// mustTaprootAddr creates a taproot address from a public key for testing.
// It panics if address creation fails.
func mustTaprootAddr(key *btcec.PublicKey) btcutil.Address {
	addr, err := btcutil.NewAddressTaproot(
		schnorr.SerializePubKey(key), &chaincfg.RegressionNetParams,
	)
	if err != nil {
		panic(err)
	}

	return addr
}

// nonAnchorOutput returns the first non-anchor output from a tree node's
// outputs. Anchor outputs are used for CPFP and have zero value.
func nonAnchorOutput(t *testing.T, node *tree.Node) *wire.TxOut {
	t.Helper()

	anchorScript := scripts.AnchorOutput().PkScript
	for _, out := range node.Outputs {
		if !bytes.Equal(out.PkScript, anchorScript) {
			return out
		}
	}

	return nil
}
