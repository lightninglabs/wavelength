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
	"github.com/lightninglabs/darepo-client/lib/closure"
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

	exitDelay := closure.RelativeLocktime{
		Type:  closure.LocktimeTypeBlock,
		Value: 144,
	}
	vtxoAmount := btcutil.Amount(5_000)

	vtxoDesc, err := tree.NewDefaultVTXODescriptor(
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

	vtxoScript := scripts.NewDefaultVtxoScript(
		clientKey, operatorKey, exitDelay,
	)

	vtxoCtx := &tx.VTXOSpendContext{
		Outpoint:   *vtxoOutpoint,
		Output:     vtxoOutput,
		VtxoScript: vtxoScript,
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

	witness, err := scripts.VtxoCollabSpendWitness(
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

// TestForfeitWithCustomVTXO verifies that forfeit transactions work correctly
// with non-default VTXO closure configurations, specifically testing that the
// collaborative spend path functions with CSVMultisigClosure exit closures.
func TestForfeitWithCustomVTXO(t *testing.T) {
	t.Parallel()

	// Create keys for a 2-of-2 client exit and 3-of-3 collab path.
	client1Key, client1Wallet := testutils.CreateKey(1)
	client1KeyDesc := &keychain.KeyDescriptor{
		PubKey:     client1Key,
		KeyLocator: keychain.KeyLocator{Family: 1, Index: 0},
	}

	client2Key, client2Wallet := testutils.CreateKey(2)
	client2KeyDesc := &keychain.KeyDescriptor{
		PubKey:     client2Key,
		KeyLocator: keychain.KeyLocator{Family: 2, Index: 0},
	}

	operatorKey, operatorWallet := testutils.CreateKey(3)
	operatorKeyDesc := &keychain.KeyDescriptor{
		PubKey:     operatorKey,
		KeyLocator: keychain.KeyLocator{Family: 3, Index: 0},
	}

	sweepKey, _ := testutils.CreateKey(4)

	exitDelay := closure.RelativeLocktime{
		Type:  closure.LocktimeTypeBlock,
		Value: 144,
	}
	vtxoAmount := btcutil.Amount(5_000)

	// Create custom VTXO with CSVMultisigClosure for exit (2-of-2)
	// and MultisigClosure for collab (3-of-3).
	customVtxoScript := &closure.TapscriptsVtxoScript{
		Closures: []closure.Closure{
			// Exit: 2-of-2 multisig after CSV delay.
			&closure.CSVMultisigClosure{
				MultisigClosure: closure.MultisigClosure{
					PubKeys: []*btcec.PublicKey{
						client1Key, client2Key,
					},
					Type: closure.MultisigTypeChecksig,
				},
				Locktime: exitDelay,
			},
			// Collab: all three parties.
			&closure.MultisigClosure{
				PubKeys: []*btcec.PublicKey{
					client1Key, client2Key, operatorKey,
				},
				Type: closure.MultisigTypeChecksig,
			},
		},
	}

	vtxoDesc, err := tree.NewVTXODescriptor(
		vtxoAmount, customVtxoScript, client1Key,
	)
	require.NoError(t, err)

	batchOutput, err := tree.BuildBatchOutput(
		[]tree.VTXODescriptor{*vtxoDesc}, operatorKey, sweepKey,
		exitDelay,
	)
	require.NoError(t, err)

	batchOutpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("custom-forfeit-commitment")),
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

	// Get the VTXO script for spend context.
	vtxoScript, err := vtxoDesc.VtxoScript()
	require.NoError(t, err)

	vtxoCtx := &tx.VTXOSpendContext{
		Outpoint:   *vtxoOutpoint,
		Output:     vtxoOutput,
		VtxoScript: vtxoScript,
	}

	// Build connector tree (same as original test).
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
		Hash:  chainhash.HashH([]byte("custom-connector-forfeit")),
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

	// Get collab spend info for the custom VTXO.
	collabSpendInfo, err := scripts.VtxoCollabSpendInfo(vtxoScript)
	require.NoError(t, err)

	// Sign with all three parties for the 3-of-3 collab path.
	signVTXOInput := func(signer input.Signer,
		keyDesc *keychain.KeyDescriptor) input.Signature {

		signDesc := scripts.VTXOSignDesc(
			*keyDesc, vtxoOutput, sigHashes, prevFetcher,
			tx.ForfeitVTXOInputIndex, collabSpendInfo,
		)

		sig, err := signer.SignOutputRaw(forfeitTx, signDesc)
		require.NoError(t, err)

		return sig
	}

	client1Sig := signVTXOInput(client1Wallet, client1KeyDesc)
	client2Sig := signVTXOInput(client2Wallet, client2KeyDesc)
	operatorSig := signVTXOInput(operatorWallet, operatorKeyDesc)

	// Build witness for 3-of-3 multisig: sig3, sig2, sig1, script, control.
	witness := wire.TxWitness{
		operatorSig.Serialize(),
		client2Sig.Serialize(),
		client1Sig.Serialize(),
		collabSpendInfo.WitnessScript,
		collabSpendInfo.ControlBlock,
	}
	forfeitTx.TxIn[tx.ForfeitVTXOInputIndex].Witness = witness

	// Sign connector input with operator key spend.
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

	// Validate the VTXO input.
	engine, err := txscript.NewEngine(
		vtxoOutput.PkScript, forfeitTx, tx.ForfeitVTXOInputIndex,
		txscript.StandardVerifyFlags, nil, sigHashes,
		vtxoOutput.Value, prevFetcher,
	)
	require.NoError(t, err)
	require.NoError(t, engine.Execute())

	// Validate the connector input.
	engine, err = txscript.NewEngine(
		connectorLeafOutput.PkScript, forfeitTx,
		tx.ForfeitConnectorInputIndex,
		txscript.StandardVerifyFlags, nil, sigHashes,
		connectorLeafOutput.Value, prevFetcher,
	)
	require.NoError(t, err)
	require.NoError(t, engine.Execute())
}
