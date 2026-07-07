package tx_test

import (
	"bytes"
	"testing"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/darepo-client/internal/testutils"
	"github.com/lightninglabs/darepo-client/lib/arkscript"
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
		PubKey: operatorKey,
		KeyLocator: keychain.KeyLocator{
			Family: 1,
			Index:  0,
		},
	}

	sweepKey, _ := testutils.CreateKey(2)

	clientKey, clientWallet := testutils.CreateKey(10)
	clientKeyDesc := &keychain.KeyDescriptor{
		PubKey: clientKey,
		KeyLocator: keychain.KeyLocator{
			Family: 10,
			Index:  0,
		},
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

	vtxoTapScript, err := arkscript.VTXOTapScript(
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
		connectorOutpoint, connectorOutput, connectorDesc, operatorKey,
		2,
	)
	require.NoError(t, err)

	connectorPath, err := connectorTree.ExtractPathForIndices(3)
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
		btcutil.Amount(connectorLeafOutput.Value), serverForfeitScript,
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
		vtxoCtx, *clientKeyDesc, tx.ForfeitVTXOInputIndex, sigHashes,
		prevFetcher,
	)
	require.NoError(t, err)

	clientSig, err := clientWallet.SignOutputRaw(forfeitTx, clientSignDesc)
	require.NoError(t, err)

	operatorSignDesc, _, err := tx.NewVTXOCollabSignDescriptor(
		vtxoCtx, *operatorKeyDesc, tx.ForfeitVTXOInputIndex, sigHashes,
		prevFetcher,
	)
	require.NoError(t, err)

	operatorSig, err := operatorWallet.SignOutputRaw(
		forfeitTx, operatorSignDesc,
	)
	require.NoError(t, err)

	witness, err := spendInfo.CollabWitness(
		clientSig, operatorSig,
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
		txscript.StandardVerifyFlags, nil, sigHashes, vtxoOutput.Value,
		prevFetcher,
	)
	require.NoError(t, err)
	require.NoError(t, engine.Execute())

	engine, err = txscript.NewEngine(
		connectorLeafOutput.PkScript, forfeitTx,
		tx.ForfeitConnectorInputIndex, txscript.StandardVerifyFlags,
		nil, sigHashes, connectorLeafOutput.Value, prevFetcher,
	)
	require.NoError(t, err)
	require.NoError(t, engine.Execute())
}

// mustTaprootAddr creates a taproot address from a public key for testing.
// It panics if address creation fails.
func mustTaprootAddr(key *btcec.PublicKey) btcaddr.Address {
	addr, err := btcaddr.NewAddressTaproot(
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

	anchorScript := arkscript.AnchorOutput().PkScript
	for _, out := range node.Outputs {
		if !bytes.Equal(out.PkScript, anchorScript) {
			return out
		}
	}

	return nil
}

// TestValidateForfeitTx tests the ValidateForfeitTx function with various
// valid and invalid inputs.
func TestValidateForfeitTx(t *testing.T) {
	t.Parallel()

	// Create test fixtures.
	vtxoOutpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("vtxo-tx")),
		Index: 0,
	}
	connectorOutpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("connector-tx")),
		Index: 1,
	}
	serverForfeitScript := []byte{
		txscript.OP_1, txscript.OP_DATA_32,
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
		0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18,
		0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f, 0x20,
	}
	vtxoAmount := btcutil.Amount(10000)
	connectorAmount := btcutil.Amount(330)

	// Build a valid forfeit tx using BuildForfeitTx.
	validTx, err := tx.BuildForfeitTx(
		&vtxoOutpoint, vtxoAmount, &connectorOutpoint, connectorAmount,
		serverForfeitScript,
	)
	require.NoError(t, err)

	validParams := tx.ForfeitTxParams{
		VTXOOutpoint:        vtxoOutpoint,
		ConnectorOutpoint:   connectorOutpoint,
		ServerForfeitScript: serverForfeitScript,
	}

	tests := []struct {
		name        string
		tx          *wire.MsgTx
		params      tx.ForfeitTxParams
		expectError string
	}{
		{
			name:        "valid forfeit tx",
			tx:          validTx,
			params:      validParams,
			expectError: "",
		},
		{
			name:        "nil tx",
			tx:          nil,
			params:      validParams,
			expectError: "forfeit tx is nil",
		},
		{
			name: "wrong number of inputs - too few",
			tx: func() *wire.MsgTx {
				tx := wire.NewMsgTx(3)
				tx.AddTxIn(newForfeitInput(vtxoOutpoint))
				tx.AddTxOut(&wire.TxOut{
					Value:    int64(vtxoAmount),
					PkScript: serverForfeitScript,
				})
				tx.AddTxOut(arkscript.AnchorOutput())

				return tx
			}(),
			params:      validParams,
			expectError: "has 1 inputs, expected 2",
		},
		{
			name: "wrong number of inputs - too many",
			tx: func() *wire.MsgTx {
				tx := wire.NewMsgTx(3)
				tx.AddTxIn(newForfeitInput(vtxoOutpoint))
				tx.AddTxIn(newForfeitInput(connectorOutpoint))
				tx.AddTxIn(
					newForfeitInput(
						wire.OutPoint{
							Index: 99,
						},
					),
				)
				tx.AddTxOut(&wire.TxOut{
					Value:    int64(vtxoAmount),
					PkScript: serverForfeitScript,
				})
				tx.AddTxOut(arkscript.AnchorOutput())

				return tx
			}(),
			params:      validParams,
			expectError: "has 3 inputs, expected 2",
		},
		{
			name: "wrong VTXO outpoint",
			tx:   validTx,
			params: tx.ForfeitTxParams{
				VTXOOutpoint: wire.OutPoint{
					Hash: chainhash.HashH(
						[]byte("wrong-vtxo"),
					),
					Index: 0,
				},
				ConnectorOutpoint:   connectorOutpoint,
				ServerForfeitScript: serverForfeitScript,
			},
			expectError: "expected VTXO",
		},
		{
			name: "wrong connector outpoint",
			tx:   validTx,
			params: tx.ForfeitTxParams{
				VTXOOutpoint: vtxoOutpoint,
				ConnectorOutpoint: wire.OutPoint{
					Hash: chainhash.HashH(
						[]byte("wrong-connector"),
					),
					Index: 0,
				},
				ServerForfeitScript: serverForfeitScript,
			},
			expectError: "expected connector",
		},
		{
			name: "wrong number of outputs - too few",
			tx: func() *wire.MsgTx {
				tx := wire.NewMsgTx(3)
				tx.AddTxIn(newForfeitInput(vtxoOutpoint))
				tx.AddTxIn(newForfeitInput(connectorOutpoint))
				tx.AddTxOut(&wire.TxOut{
					Value:    int64(vtxoAmount),
					PkScript: serverForfeitScript,
				})

				return tx
			}(),
			params:      validParams,
			expectError: "has 1 outputs, expected 2",
		},
		{
			name: "wrong penalty script",
			tx: func() *wire.MsgTx {
				tx := wire.NewMsgTx(3)
				tx.AddTxIn(newForfeitInput(vtxoOutpoint))
				tx.AddTxIn(newForfeitInput(connectorOutpoint))
				tx.AddTxOut(&wire.TxOut{
					Value: int64(vtxoAmount),
					PkScript: []byte{
						0x00, 0x14, 0x01, 0x02,
					},
				})
				tx.AddTxOut(arkscript.AnchorOutput())

				return tx
			}(),
			params:      validParams,
			expectError: "does not pay to server forfeit script",
		},
		{
			name: "wrong amount when expected",
			tx:   validTx,
			params: tx.ForfeitTxParams{
				VTXOOutpoint:        vtxoOutpoint,
				ConnectorOutpoint:   connectorOutpoint,
				ServerForfeitScript: serverForfeitScript,
				ExpectedAmount:      btcutil.Amount(99999),
			},
			expectError: "penalty output has amount",
		},
		{
			name: "correct amount when expected",
			tx:   validTx,
			params: tx.ForfeitTxParams{
				VTXOOutpoint:        vtxoOutpoint,
				ConnectorOutpoint:   connectorOutpoint,
				ServerForfeitScript: serverForfeitScript,
				ExpectedAmount: vtxoAmount +
					connectorAmount,
			},
			expectError: "",
		},
		{
			name: "non-P2A anchor script",
			tx: func() *wire.MsgTx {
				tx := wire.NewMsgTx(3)
				tx.AddTxIn(newForfeitInput(vtxoOutpoint))
				tx.AddTxIn(newForfeitInput(connectorOutpoint))
				tx.AddTxOut(&wire.TxOut{
					Value:    int64(vtxoAmount),
					PkScript: serverForfeitScript,
				})
				tx.AddTxOut(&wire.TxOut{
					Value:    0,
					PkScript: []byte{0x00, 0x14, 0x01},
				})

				return tx
			}(),
			params:      validParams,
			expectError: "is not a P2A anchor",
		},
		{
			name: "non-zero anchor value",
			tx: func() *wire.MsgTx {
				tx := wire.NewMsgTx(3)
				tx.AddTxIn(newForfeitInput(vtxoOutpoint))
				tx.AddTxIn(newForfeitInput(connectorOutpoint))
				tx.AddTxOut(&wire.TxOut{
					Value:    int64(vtxoAmount),
					PkScript: serverForfeitScript,
				})
				tx.AddTxOut(&wire.TxOut{
					Value:    1000,
					PkScript: arkscript.AnchorPkScript,
				})

				return tx
			}(),
			params:      validParams,
			expectError: "anchor output has non-zero value",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tx.ValidateForfeitTx(tc.tx, tc.params)

			if tc.expectError == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.expectError)
			}
		})
	}
}

// newForfeitInput creates a forfeit input with the default final sequence.
func newForfeitInput(outpoint wire.OutPoint) *wire.TxIn {
	return &wire.TxIn{
		PreviousOutPoint: outpoint,
		Sequence:         wire.MaxTxInSequenceNum,
	}
}

// TestValidateForfeitTxRoundTrip verifies that a forfeit tx built with
// BuildForfeitTx passes validation.
func TestValidateForfeitTxRoundTrip(t *testing.T) {
	t.Parallel()

	// Test with various amounts: dust limit, normal, and larger values.
	amounts := []btcutil.Amount{
		btcutil.Amount(546),
		btcutil.Amount(10000),
		btcutil.Amount(1000000),
	}

	for _, amount := range amounts {
		t.Run(amount.String(), func(t *testing.T) {
			vtxoOutpoint := wire.OutPoint{
				Hash:  chainhash.HashH([]byte("vtxo")),
				Index: 0,
			}
			connectorOutpoint := wire.OutPoint{
				Hash:  chainhash.HashH([]byte("connector")),
				Index: 0,
			}
			serverScript := []byte{
				txscript.OP_1, txscript.OP_DATA_32,
				0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
				0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
				0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18,
				0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f, 0x20,
			}

			connectorAmount := btcutil.Amount(330)
			forfeitTx, err := tx.BuildForfeitTx(
				&vtxoOutpoint, amount, &connectorOutpoint,
				connectorAmount, serverScript,
			)
			require.NoError(t, err)

			// Validate without amount check.
			err = tx.ValidateForfeitTx(
				forfeitTx, tx.ForfeitTxParams{
					VTXOOutpoint:        vtxoOutpoint,
					ConnectorOutpoint:   connectorOutpoint,
					ServerForfeitScript: serverScript,
				},
			)
			require.NoError(t, err)

			// Validate with amount check.
			err = tx.ValidateForfeitTx(
				forfeitTx, tx.ForfeitTxParams{
					VTXOOutpoint:        vtxoOutpoint,
					ConnectorOutpoint:   connectorOutpoint,
					ServerForfeitScript: serverScript,
					ExpectedAmount: amount +
						connectorAmount,
				},
			)
			require.NoError(t, err)
		})
	}
}

// TestValidateForfeitTxWithContext verifies that custom sequence and locktime
// requirements round-trip through forfeit tx construction and validation.
func TestValidateForfeitTxWithContext(t *testing.T) {
	t.Parallel()

	vtxoOutpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("vtxo-custom")),
		Index: 0,
	}
	connectorOutpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("connector-custom")),
		Index: 1,
	}
	serverScript := []byte{
		txscript.OP_1, txscript.OP_DATA_32,
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
		0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18,
		0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f, 0x20,
	}

	forfeitTx, err := tx.BuildForfeitTxWithContext(
		&vtxoOutpoint, 42_000, &connectorOutpoint, 330,
		serverScript,
		tx.ForfeitTxContext{
			VTXOSequence: 144,
			LockTime:     500_000,
		},
	)
	require.NoError(t, err)

	err = tx.ValidateForfeitTx(forfeitTx, tx.ForfeitTxParams{
		VTXOOutpoint:        vtxoOutpoint,
		ConnectorOutpoint:   connectorOutpoint,
		ServerForfeitScript: serverScript,
		ExpectedAmount:      42_330,
		ExpectedSequence:    144,
		ExpectedLockTime:    500_000,
	})
	require.NoError(t, err)
}
