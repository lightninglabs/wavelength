package tree

import (
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
	"github.com/stretchr/testify/require"
)

// TestBuildVTXOTree tests the BuildVTXOTree function with various VTXO
// configurations.
func TestBuildVTXOTree(t *testing.T) {
	// Generate test keys.
	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorPubKey := operatorKey.PubKey()

	sweepKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	sweepPubKey := sweepKey.PubKey()

	user1Key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	user1PubKey := user1Key.PubKey()

	user2Key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	user2PubKey := user2Key.PubKey()

	// Test batch outpoint.
	batchOutpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("commitment_tx")),
		Index: 0,
	}

	// Mock batch output.
	batchOutput := &wire.TxOut{
		Value:    10000,
		PkScript: []byte("batch_script"),
	}

	t.Run("single VTXO tree", func(t *testing.T) {
		// Create VTXO script.
		vtxoScript, err := txscript.PayToTaprootScript(user1PubKey)
		require.NoError(t, err)

		vtxos := []VTXODescriptor{
			{
				PkScript:    vtxoScript,
				Amount:      btcutil.Amount(5000),
				CoSignerKey: user1PubKey,
			},
		}

		// Batch output value should match the sum of VTXO amounts.
		singleBatchOutput := &wire.TxOut{
			Value:    5000,
			PkScript: []byte("batch_script"),
		}

		tree, err := BuildVTXOTree(
			batchOutpoint,
			singleBatchOutput,
			vtxos,
			operatorPubKey,
			sweepPubKey,
			144, // sweep delay
			2,   // radix
		)
		require.NoError(t, err)
		require.NotNil(t, tree)

		// Verify tree context.
		require.Equal(t, batchOutpoint, tree.BatchOutpoint)
		require.Equal(t, singleBatchOutput, tree.BatchOutput)
		require.NotNil(t, tree.SweepTapscriptRoot)

		// Root should be a leaf transaction.
		require.Equal(t, batchOutpoint, tree.Root.Input)
		require.True(t, tree.Root.IsLeaf())
		require.Len(t, tree.Root.Children, 0)

		// Should have VTXO output + anchor output.
		require.Len(t, tree.Root.Outputs, 2)
		require.Equal(t, int64(5000), tree.Root.Outputs[0].Value)
		require.Equal(t, vtxoScript, tree.Root.Outputs[0].PkScript)
		require.Equal(t, int64(0), tree.Root.Outputs[1].Value)

		// Should have operator + user1 as cosigners.
		require.Len(t, tree.Root.CoSigners, 2)
		require.Contains(t, tree.Root.CoSigners, operatorPubKey)
		require.Contains(t, tree.Root.CoSigners, user1PubKey)
	})

	t.Run("two VTXO tree", func(t *testing.T) {
		vtxo1Script, err := txscript.PayToTaprootScript(user1PubKey)
		require.NoError(t, err)

		vtxo2Script, err := txscript.PayToTaprootScript(user2PubKey)
		require.NoError(t, err)

		vtxos := []VTXODescriptor{
			{
				PkScript:    vtxo1Script,
				Amount:      btcutil.Amount(5000),
				CoSignerKey: user1PubKey,
			},
			{
				PkScript:    vtxo2Script,
				Amount:      btcutil.Amount(3000),
				CoSignerKey: user2PubKey,
			},
		}

		// Batch output value should match sum of VTXOs:
		// 5000 + 3000 = 8000.
		twoBatchOutput := &wire.TxOut{
			Value:    8000,
			PkScript: []byte("batch_script"),
		}

		tree, err := BuildVTXOTree(
			batchOutpoint, twoBatchOutput, vtxos, operatorPubKey,
			sweepPubKey, 144, 2,
		)
		require.NoError(t, err)
		require.NotNil(t, tree)

		// Root should be a branch with 2 children.
		require.Equal(t, batchOutpoint, tree.Root.Input)
		require.False(t, tree.Root.IsLeaf())
		require.Len(t, tree.Root.Children, 2)

		// Should have 2 branch outputs + anchor.
		require.Len(t, tree.Root.Outputs, 3)
		require.Equal(t, int64(0), tree.Root.Outputs[2].Value) // Anchor

		// Root should have all cosigners.
		require.Len(t, tree.Root.CoSigners, 3) // op + user1 + user2
		require.Contains(t, tree.Root.CoSigners, operatorPubKey)
		require.Contains(t, tree.Root.CoSigners, user1PubKey)
		require.Contains(t, tree.Root.CoSigners, user2PubKey)

		// Verify children are leaf transactions.
		child0, exists := tree.Root.Children[0]
		require.True(t, exists)
		require.True(t, child0.IsLeaf())
		require.Len(t, child0.CoSigners, 2) // operator + one user

		child1, exists := tree.Root.Children[1]
		require.True(t, exists)
		require.True(t, child1.IsLeaf())
		require.Len(t, child1.CoSigners, 2)

		// Verify children have correct inputs.
		rootTXID, err := tree.Root.TXID()
		require.NoError(t, err)

		require.Equal(t, rootTXID, child0.Input.Hash)
		require.Equal(t, uint32(0), child0.Input.Index)

		require.Equal(t, rootTXID, child1.Input.Hash)
		require.Equal(t, uint32(1), child1.Input.Index)
	})

	t.Run("five VTXO tree with radix 2", func(t *testing.T) {
		// Create 5 VTXOs with different amounts.
		// Total: 5000+4000+3000+2000+1000 = 15000
		vtxos := make([]VTXODescriptor, 5)
		for i := 0; i < 5; i++ {
			key, err := btcec.NewPrivateKey()
			require.NoError(t, err)

			script, err := txscript.PayToTaprootScript(key.PubKey())
			require.NoError(t, err)

			vtxos[i] = VTXODescriptor{
				PkScript:    script,
				Amount:      btcutil.Amount(1000 * (5 - i)),
				CoSignerKey: key.PubKey(),
			}
		}

		// Batch output should match total VTXO amounts.
		fiveBatchOutput := &wire.TxOut{
			Value:    15000,
			PkScript: []byte("batch_script"),
		}

		tree, err := BuildVTXOTree(
			batchOutpoint, fiveBatchOutput, vtxos, operatorPubKey,
			sweepPubKey, 144, 2,
		)
		require.NoError(t, err)
		require.NotNil(t, tree)

		// Should create a multi-level tree.
		require.Len(t, tree.Root.Children, 2)

		// Verify all leaves are present.
		leaves := tree.Root.GetLeafNodes()
		require.Len(t, leaves, 5)

		// Verify total amount is preserved.
		var totalAmount int64
		for _, leaf := range leaves {
			// Leaf has VTXO output + anchor output.
			require.Len(t, leaf.Outputs, 2)
			totalAmount += leaf.Outputs[0].Value
		}
		require.Equal(t, int64(15000), totalAmount)

		// Verify tree depth (should be 4 for 5 leaves, radix 2).
		depth := tree.Root.Depth()
		require.Equal(t, 4, depth)
	})

	t.Run("error cases", func(t *testing.T) {
		vtxoScript, err := txscript.PayToTaprootScript(user1PubKey)
		require.NoError(t, err)

		validVTXO := VTXODescriptor{
			PkScript:    vtxoScript,
			Amount:      btcutil.Amount(1000),
			CoSignerKey: user1PubKey,
		}

		t.Run("empty VTXOs fails", func(t *testing.T) {
			tree, err := BuildVTXOTree(
				batchOutpoint, batchOutput, []VTXODescriptor{},
				operatorPubKey, sweepPubKey, 144, 2,
			)
			require.Error(t, err)
			require.Nil(t, tree)
			require.Contains(
				t, err.Error(),
				"invalid VTXO descriptors",
			)
		})

		t.Run("radix too small fails", func(t *testing.T) {
			tree, err := BuildVTXOTree(
				batchOutpoint, batchOutput,
				[]VTXODescriptor{validVTXO}, operatorPubKey,
				sweepPubKey, 144, 1,
			)
			require.Error(t, err)
			require.Nil(t, tree)
			require.Contains(
				t, err.Error(),
				"radix must be at least 2",
			)
		})

		t.Run("nil operator key fails", func(t *testing.T) {
			tree, err := BuildVTXOTree(
				batchOutpoint, batchOutput,
				[]VTXODescriptor{validVTXO}, nil, sweepPubKey,
				144, 2,
			)
			require.Error(t, err)
			require.Nil(t, tree)
			require.Contains(t, err.Error(), "operator co-sign key")
		})

		t.Run("nil sweep key fails", func(t *testing.T) {
			tree, err := BuildVTXOTree(
				batchOutpoint, batchOutput,
				[]VTXODescriptor{validVTXO}, operatorPubKey,
				nil, 144, 2,
			)
			require.Error(t, err)
			require.Nil(t, tree)
			require.Contains(t, err.Error(), "sweep key")
		})
	})
}

// TestBuildVTXOTreeStableSort tests that tree construction is deterministic
// when VTXOs have the same amount.
func TestBuildVTXOTreeStableSort(t *testing.T) {
	operatorKey, _ := testutils.CreateKey(1)
	sweepKey, _ := testutils.CreateKey(2)

	batchOutpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("commitment")),
		Index: 0,
	}

	// Batch output value must match sum of VTXOs: 3 * 1000 = 3000.
	batchOutput := &wire.TxOut{Value: 3000, PkScript: []byte("batch")}

	// Create 3 VTXOs with SAME amount but different scripts.
	// The scripts are intentionally out of lexicographic order.
	key10, _ := testutils.CreateKey(10)
	script1, _ := txscript.PayToTaprootScript(key10)

	key20, _ := testutils.CreateKey(20)
	script2, _ := txscript.PayToTaprootScript(key20)

	key30, _ := testutils.CreateKey(30)
	script3, _ := txscript.PayToTaprootScript(key30)

	coSigner1, _ := testutils.CreateKey(101)
	coSigner2, _ := testutils.CreateKey(102)
	coSigner3, _ := testutils.CreateKey(103)

	vtxos := []VTXODescriptor{
		{
			PkScript:    script3,
			Amount:      1000,
			CoSignerKey: coSigner3,
		},
		{
			PkScript:    script1,
			Amount:      1000,
			CoSignerKey: coSigner1,
		},
		{
			PkScript:    script2,
			Amount:      1000,
			CoSignerKey: coSigner2,
		},
	}

	// Build tree multiple times - should be identical.
	tree1, err := BuildVTXOTree(
		batchOutpoint, batchOutput, vtxos, operatorKey, sweepKey, 144,
		2,
	)
	require.NoError(t, err)

	tree2, err := BuildVTXOTree(
		batchOutpoint, batchOutput, vtxos, operatorKey, sweepKey, 144,
		2,
	)
	require.NoError(t, err)

	// Trees should be identical (same TXIDs).
	txid1, err := tree1.Root.TXID()
	require.NoError(t, err)

	txid2, err := tree2.Root.TXID()
	require.NoError(t, err)

	require.Equal(
		t, txid1, txid2, "tree construction should be deterministic",
	)

	// Verify leaves are sorted by PkScript (tiebreaker).
	leaves1 := tree1.Root.GetLeafNodes()
	leaves2 := tree2.Root.GetLeafNodes()

	require.Len(t, leaves1, 3)
	require.Len(t, leaves2, 3)

	for i := range leaves1 {
		require.Equal(
			t, leaves1[i].Outputs[0].PkScript,
			leaves2[i].Outputs[0].PkScript, "leaf %d should "+
				"have same script", i,
		)
	}
}

// TestBuildConnectorTree tests connector tree construction with various
// configurations.
func TestBuildConnectorTree(t *testing.T) {
	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorPubKey := operatorKey.PubKey()

	connectorScript, err := txscript.PayToTaprootScript(operatorPubKey)
	require.NoError(t, err)

	batchOutpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("commitment_tx")),
		Index: 1,
	}

	batchOutput := &wire.TxOut{
		Value:    2000,
		PkScript: []byte("connector_batch_script"),
	}

	t.Run("single connector leaf", func(t *testing.T) {
		connector := ConnectorDescriptor{
			PkScript:  connectorScript,
			NumLeaves: 1,
			Amount:    btcutil.Amount(330),
		}

		// Batch output must match the connector amount for single leaf.
		singleBatchOutput := &wire.TxOut{
			Value:    330,
			PkScript: []byte("connector_batch_script"),
		}

		tree, err := BuildConnectorTree(
			batchOutpoint,
			singleBatchOutput,
			connector,
			operatorPubKey,
			4, // radix
		)
		require.NoError(t, err)
		require.NotNil(t, tree)

		// Verify tree context.
		require.Equal(t, batchOutpoint, tree.BatchOutpoint)
		require.Nil(t, tree.SweepTapscriptRoot)

		// Root should be a leaf.
		require.True(t, tree.Root.IsLeaf())
		require.Len(t, tree.Root.Outputs, 2)

		// Verify connector output.
		require.Equal(t, int64(330), tree.Root.Outputs[0].Value)
		require.Equal(t, connectorScript, tree.Root.Outputs[0].PkScript)

		// Verify anchor output.
		require.Equal(t, int64(0), tree.Root.Outputs[1].Value)
	})

	t.Run("four connector leaves with radix 4", func(t *testing.T) {
		connector := ConnectorDescriptor{
			PkScript:  connectorScript,
			NumLeaves: 4,
			Amount:    btcutil.Amount(330),
		}

		// Batch output = 4 * 330 = 1320.
		fourBatchOutput := &wire.TxOut{
			Value:    1320,
			PkScript: []byte("connector_batch_script"),
		}

		tree, err := BuildConnectorTree(
			batchOutpoint, fourBatchOutput, connector,
			operatorPubKey, 4,
		)
		require.NoError(t, err)
		require.NotNil(t, tree)

		// With radix 4, root should be branch with 4 children.
		require.False(t, tree.Root.IsLeaf())
		require.Len(t, tree.Root.Children, 4)

		// Each child should be a leaf.
		for i := uint32(0); i < 4; i++ {
			child, exists := tree.Root.Children[i]
			require.True(t, exists, "child %d", i)
			require.True(t, child.IsLeaf(), "child %d", i)

			// Verify connector output is identical (1320/4 = 330).
			require.Equal(t, int64(330), child.Outputs[0].Value)
			require.Equal(
				t, connectorScript, child.Outputs[0].PkScript,
			)
		}

		// Verify total leaves.
		leaves := tree.Root.GetLeafNodes()
		require.Len(t, leaves, 4)

		// All leaves should have identical scripts.
		for _, leaf := range leaves {
			require.Equal(
				t, connectorScript, leaf.Outputs[0].PkScript,
			)
			require.Equal(t, int64(330), leaf.Outputs[0].Value)
		}
	})

	t.Run("eight connector leaves with radix 4", func(t *testing.T) {
		connector := ConnectorDescriptor{
			PkScript:  connectorScript,
			NumLeaves: 8,
			Amount:    btcutil.Amount(330),
		}

		// Batch output = 8 * 330 = 2640.
		eightBatchOutput := &wire.TxOut{
			Value:    2640,
			PkScript: []byte("connector_batch_script"),
		}

		tree, err := BuildConnectorTree(
			batchOutpoint, eightBatchOutput, connector,
			operatorPubKey, 4,
		)
		require.NoError(t, err)
		require.NotNil(t, tree)

		// Should have multiple levels.
		require.False(t, tree.Root.IsLeaf())

		// Verify all leaves present.
		leaves := tree.Root.GetLeafNodes()
		require.Len(t, leaves, 8)

		// Verify all identical.
		for i, leaf := range leaves {
			require.Equal(
				t, connectorScript, leaf.Outputs[0].PkScript,
				"leaf %d", i,
			)
			require.Equal(
				t, int64(330), leaf.Outputs[0].Value, "leaf %d",
				i,
			)
		}
	})

	t.Run("connector leaves dedupe operator cosigner", func(t *testing.T) {
		connectorTapKey := txscript.ComputeTaprootOutputKey(
			operatorPubKey, nil,
		)
		connectorScript, err := txscript.PayToTaprootScript(
			connectorTapKey,
		)
		require.NoError(t, err)

		connector := ConnectorDescriptor{
			PkScript:  connectorScript,
			NumLeaves: 3,
			Amount:    btcutil.Amount(330),
		}
		deepBatchOutput := &wire.TxOut{
			Value:    990,
			PkScript: connectorScript,
		}

		tree, err := BuildConnectorTree(
			batchOutpoint, deepBatchOutput, connector,
			operatorPubKey, 2,
		)
		require.NoError(t, err)

		prevOuts, err := tree.Root.PrevOutputFetcher(
			tree.BatchOutput,
		)
		require.NoError(t, err)

		require.NoError(
			t, tree.Root.ForEach(func(node *Node) error {
				require.Len(t, node.CoSigners, 1)
				require.True(
					t, node.CoSigners[0].IsEqual(
						operatorPubKey,
					),
				)

				tx, err := node.ToTx()
				require.NoError(t, err)

				prevOut := prevOuts.FetchPrevOutput(
					tx.TxIn[0].PreviousOutPoint,
				)
				require.NotNil(t, prevOut)
				require.Equal(
					t, connectorScript, prevOut.PkScript,
				)

				return nil
			}),
		)
	})

	t.Run("extract by index works", func(t *testing.T) {
		connector := ConnectorDescriptor{
			PkScript:  connectorScript,
			NumLeaves: 4,
			Amount:    btcutil.Amount(330),
		}

		// Batch output = 4 * 330 = 1320.
		extractBatchOutput := &wire.TxOut{
			Value:    1320,
			PkScript: []byte("connector_batch_script"),
		}

		tree, err := BuildConnectorTree(
			batchOutpoint, extractBatchOutput, connector,
			operatorPubKey, 2,
		)
		require.NoError(t, err)

		// Extract each connector by index.
		for i := 0; i < 4; i++ {
			extracted, err := tree.ExtractPathForIndices(i)
			require.NoError(t, err, "index %d", i)
			require.NotNil(t, extracted, "index %d", i)

			// Should have exactly one leaf.
			leaves := extracted.Root.GetLeafNodes()
			require.Len(t, leaves, 1, "index %d", i)

			// Leaf should have correct connector output.
			leaf := leaves[0]
			outpoint, err := leaf.GetNonAnchorOutpoint()
			require.NoError(t, err)
			require.NotNil(t, outpoint)
		}
	})

	t.Run("error cases", func(t *testing.T) {
		connector := ConnectorDescriptor{
			PkScript:  connectorScript,
			NumLeaves: 4,
			Amount:    btcutil.Amount(330),
		}

		t.Run("invalid connector descriptor fails", func(t *testing.T) {
			badConnector := ConnectorDescriptor{
				PkScript:  connectorScript,
				NumLeaves: 0, // Invalid!
				Amount:    btcutil.Amount(330),
			}

			tree, err := BuildConnectorTree(
				batchOutpoint, batchOutput, badConnector,
				operatorPubKey, 4,
			)
			require.Error(t, err)
			require.Nil(t, tree)
			require.Contains(
				t, err.Error(),
				"invalid connector descriptor",
			)
		})

		t.Run("radix too small fails", func(t *testing.T) {
			tree, err := BuildConnectorTree(
				batchOutpoint, batchOutput, connector,
				operatorPubKey, 1,
			)
			require.Error(t, err)
			require.Nil(t, tree)
			require.Contains(
				t, err.Error(),
				"radix must be at least 2",
			)
		})

		t.Run("nil operator key fails", func(t *testing.T) {
			tree, err := BuildConnectorTree(
				batchOutpoint, batchOutput, connector, nil, 4,
			)
			require.Error(t, err)
			require.Nil(t, tree)
			require.Contains(t, err.Error(), "operator key")
		})
	})

	t.Run("connector leaves are truly identical", func(t *testing.T) {
		connector := ConnectorDescriptor{
			PkScript:  connectorScript,
			NumLeaves: 4,
			Amount:    btcutil.Amount(330),
		}

		// Batch output = 4 * 330 = 1320.
		identicalBatchOutput := &wire.TxOut{
			Value:    1320,
			PkScript: []byte("connector_batch_script"),
		}

		tree, err := BuildConnectorTree(
			batchOutpoint, identicalBatchOutput, connector,
			operatorPubKey, 2,
		)
		require.NoError(t, err)

		leaves := tree.Root.GetLeafNodes()
		require.Len(t, leaves, 4)

		// All leaves should have identical outputs.
		firstLeafOutput := leaves[0].Outputs[0]
		for i, leaf := range leaves {
			require.Equal(
				t, firstLeafOutput.Value, leaf.Outputs[0].Value,
				"leaf %d value", i,
			)
			require.Equal(
				t, firstLeafOutput.PkScript,
				leaf.Outputs[0].PkScript, "leaf %d script", i,
			)
		}

		// All leaves should have same cosigner (operator).
		for i, leaf := range leaves {
			require.Len(t, leaf.CoSigners, 1, "leaf %d", i)
			require.True(
				t, leaf.CoSigners[0].IsEqual(
					operatorPubKey,
				),
				"leaf %d should contain operator",
				i,
			)
		}
	})
}

// TestBuildBatchOutput tests batch output construction for commitment
// transactions.
func TestBuildBatchOutput(t *testing.T) {
	operatorKey, _ := testutils.CreateKey(1)
	sweepKey, _ := testutils.CreateKey(2)

	user1Key, _ := testutils.CreateKey(10)
	user1Script, _ := txscript.PayToTaprootScript(user1Key)

	user2Key, _ := testutils.CreateKey(20)
	user2Script, _ := txscript.PayToTaprootScript(user2Key)

	t.Run("single VTXO", func(t *testing.T) {
		vtxos := []VTXODescriptor{
			{
				PkScript:    user1Script,
				Amount:      btcutil.Amount(5000),
				CoSignerKey: user1Key,
			},
		}

		output, err := BuildBatchOutput(
			vtxos, operatorKey, sweepKey, 144,
		)
		require.NoError(t, err)
		require.NotNil(t, output)

		// Amount should equal the single VTXO amount.
		require.Equal(t, int64(5000), output.Value)

		// Script should be valid P2TR.
		require.True(t, txscript.IsPayToTaproot(output.PkScript))
	})

	t.Run("multiple VTXOs", func(t *testing.T) {
		vtxos := []VTXODescriptor{
			{
				PkScript:    user1Script,
				Amount:      btcutil.Amount(5000),
				CoSignerKey: user1Key,
			},
			{
				PkScript:    user2Script,
				Amount:      btcutil.Amount(3000),
				CoSignerKey: user2Key,
			},
		}

		output, err := BuildBatchOutput(
			vtxos, operatorKey, sweepKey, 144,
		)
		require.NoError(t, err)
		require.NotNil(t, output)

		// Amount should be sum of all VTXOs.
		require.Equal(t, int64(8000), output.Value)

		// Script should be valid P2TR.
		require.True(t, txscript.IsPayToTaproot(output.PkScript))
	})

	t.Run("duplicate cosigners handled", func(t *testing.T) {
		// Same user has 2 VTXOs.
		vtxos := []VTXODescriptor{
			{
				PkScript:    user1Script,
				Amount:      btcutil.Amount(2000),
				CoSignerKey: user1Key,
			},
			{
				PkScript:    user1Script,
				Amount:      btcutil.Amount(3000),
				CoSignerKey: user1Key, // Same key!
			},
		}

		output, err := BuildBatchOutput(
			vtxos, operatorKey, sweepKey, 144,
		)
		require.NoError(t, err)
		require.NotNil(t, output)

		// Amount should still be sum.
		require.Equal(t, int64(5000), output.Value)
	})

	t.Run("error cases", func(t *testing.T) {
		validVTXO := VTXODescriptor{
			PkScript:    user1Script,
			Amount:      btcutil.Amount(1000),
			CoSignerKey: user1Key,
		}

		t.Run("empty VTXOs fails", func(t *testing.T) {
			output, err := BuildBatchOutput(
				[]VTXODescriptor{}, operatorKey, sweepKey, 144,
			)
			require.Error(t, err)
			require.Nil(t, output)
			require.Contains(t, err.Error(), "at least one VTXO")
		})

		t.Run("nil operator key fails", func(t *testing.T) {
			output, err := BuildBatchOutput(
				[]VTXODescriptor{validVTXO}, nil, sweepKey, 144,
			)
			require.Error(t, err)
			require.Nil(t, output)
			require.Contains(t, err.Error(), "operator musig key")
		})

		t.Run("nil sweep key fails", func(t *testing.T) {
			output, err := BuildBatchOutput(
				[]VTXODescriptor{validVTXO}, operatorKey, nil,
				144,
			)
			require.Error(t, err)
			require.Nil(t, output)
			require.Contains(t, err.Error(), "sweep key")
		})

		t.Run("operator cosigner key fails", func(t *testing.T) {
			vtxo := validVTXO
			vtxo.CoSignerKey = operatorKey

			output, err := BuildBatchOutput(
				[]VTXODescriptor{vtxo}, operatorKey, sweepKey,
				144,
			)
			require.Error(t, err)
			require.Nil(t, output)
			require.Contains(t, err.Error(), "operator key")
		})
	})
}

// TestBuildConnectorOutput tests connector output construction for commitment
// transactions.
func TestBuildConnectorOutput(t *testing.T) {
	operatorKey, _ := testutils.CreateKey(1)
	connectorAddr, err := btcaddr.NewAddressTaproot(
		schnorr.SerializePubKey(operatorKey),
		&chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)

	t.Run("valid connector output", func(t *testing.T) {
		output, err := BuildConnectorOutput(
			4,                   // 4 connectors
			btcutil.Amount(330), // dust amount
			connectorAddr,
		)
		require.NoError(t, err)
		require.NotNil(t, output)

		// Amount should be 4 * 330 = 1320.
		require.Equal(t, int64(1320), output.Value)

		// Script should be valid P2TR.
		require.True(t, txscript.IsPayToTaproot(output.PkScript))
	})

	t.Run("single connector", func(t *testing.T) {
		output, err := BuildConnectorOutput(
			1, btcutil.Amount(330), connectorAddr,
		)
		require.NoError(t, err)
		require.Equal(t, int64(330), output.Value)
	})

	t.Run("error cases", func(t *testing.T) {
		t.Run("zero connectors fails", func(t *testing.T) {
			output, err := BuildConnectorOutput(
				0, btcutil.Amount(330), connectorAddr,
			)
			require.Error(t, err)
			require.Nil(t, output)
			require.Contains(
				t, err.Error(),
				"num connectors must be > 0",
			)
		})

		t.Run("zero dust amount fails", func(t *testing.T) {
			output, err := BuildConnectorOutput(
				4, btcutil.Amount(0), connectorAddr,
			)
			require.Error(t, err)
			require.Nil(t, output)
			require.Contains(
				t, err.Error(),
				"dust amount must be > 0",
			)
		})

		t.Run("nil address fails", func(t *testing.T) {
			output, err := BuildConnectorOutput(
				4, btcutil.Amount(330), nil,
			)
			require.Error(t, err)
			require.Nil(t, output)
			require.Contains(t, err.Error(), "connector address")
		})
	})
}

// TestValidateVTXODescriptors tests the ValidateVTXODescriptors function
// with various VTXO descriptor configurations to ensure proper validation
// of VTXO parameters including amounts, scripts, and co-signer keys.
func TestValidateVTXODescriptors(t *testing.T) {
	// Generate test keys.
	key1, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	key1Pub := key1.PubKey()

	key2, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	key2Pub := key2.PubKey()

	key3, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	key3Pub := key3.PubKey()

	// Create a valid taproot script for testing.
	validTaprootScript, err := txscript.PayToTaprootScript(key1Pub)
	require.NoError(t, err)

	t.Run("valid single VTXO", func(t *testing.T) {
		vtxos := []VTXODescriptor{
			{
				PkScript:    validTaprootScript,
				Amount:      btcutil.Amount(10000),
				CoSignerKey: key1Pub,
			},
		}

		err := ValidateVTXODescriptors(vtxos)
		require.NoError(t, err)
	})

	t.Run("valid multiple VTXOs", func(t *testing.T) {
		script2, err := txscript.PayToTaprootScript(key2Pub)
		require.NoError(t, err)

		script3, err := txscript.PayToTaprootScript(key3Pub)
		require.NoError(t, err)

		vtxos := []VTXODescriptor{
			{
				PkScript:    validTaprootScript,
				Amount:      btcutil.Amount(10000),
				CoSignerKey: key1Pub,
			},
			{
				PkScript:    script2,
				Amount:      btcutil.Amount(20000),
				CoSignerKey: key2Pub,
			},
			{
				PkScript:    script3,
				Amount:      btcutil.Amount(30000),
				CoSignerKey: key3Pub,
			},
		}

		err = ValidateVTXODescriptors(vtxos)
		require.NoError(t, err)
	})

	t.Run("empty list fails", func(t *testing.T) {
		err := ValidateVTXODescriptors([]VTXODescriptor{})
		require.Error(t, err)
		require.Contains(t, err.Error(), "no VTXO descriptors")
	})

	t.Run("nil list fails", func(t *testing.T) {
		err := ValidateVTXODescriptors(nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), "no VTXO descriptors")
	})

	t.Run("zero amount fails", func(t *testing.T) {
		vtxos := []VTXODescriptor{
			{
				PkScript:    validTaprootScript,
				Amount:      btcutil.Amount(0),
				CoSignerKey: key1Pub,
			},
		}

		err := ValidateVTXODescriptors(vtxos)
		require.Error(t, err)
		require.Contains(t, err.Error(), "invalid amount")
	})

	t.Run("negative amount fails", func(t *testing.T) {
		vtxos := []VTXODescriptor{
			{
				PkScript:    validTaprootScript,
				Amount:      btcutil.Amount(-1000),
				CoSignerKey: key1Pub,
			},
		}

		err := ValidateVTXODescriptors(vtxos)
		require.Error(t, err)
		require.Contains(t, err.Error(), "invalid amount")
	})

	t.Run("empty PkScript fails", func(t *testing.T) {
		vtxos := []VTXODescriptor{
			{
				PkScript:    []byte{},
				Amount:      btcutil.Amount(10000),
				CoSignerKey: key1Pub,
			},
		}

		err := ValidateVTXODescriptors(vtxos)
		require.Error(t, err)
		require.Contains(t, err.Error(), "empty PkScript")
	})

	t.Run("nil PkScript fails", func(t *testing.T) {
		vtxos := []VTXODescriptor{
			{
				PkScript:    nil,
				Amount:      btcutil.Amount(10000),
				CoSignerKey: key1Pub,
			},
		}

		err := ValidateVTXODescriptors(vtxos)
		require.Error(t, err)
		require.Contains(t, err.Error(), "empty PkScript")
	})

	t.Run("non-taproot script fails", func(t *testing.T) {
		// Create a P2WPKH script (not taproot).
		p2wpkhScript := []byte{
			0x00, 0x14, // OP_0 + 20 bytes
			0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
			0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
			0x11, 0x12, 0x13, 0x14,
		}

		vtxos := []VTXODescriptor{
			{
				PkScript:    p2wpkhScript,
				Amount:      btcutil.Amount(10000),
				CoSignerKey: key1Pub,
			},
		}

		err := ValidateVTXODescriptors(vtxos)
		require.Error(t, err)
		require.Contains(t, err.Error(), "invalid taproot script")
	})

	t.Run("nil co-signer key fails", func(t *testing.T) {
		vtxos := []VTXODescriptor{
			{
				PkScript:    validTaprootScript,
				Amount:      btcutil.Amount(10000),
				CoSignerKey: nil,
			},
		}

		err := ValidateVTXODescriptors(vtxos)
		require.Error(t, err)
		require.Contains(t, err.Error(), "nil co-signer key")
	})

	t.Run("duplicate co-signer keys fail", func(t *testing.T) {
		script2, err := txscript.PayToTaprootScript(key2Pub)
		require.NoError(t, err)

		vtxos := []VTXODescriptor{
			{
				PkScript:    validTaprootScript,
				Amount:      btcutil.Amount(10000),
				CoSignerKey: key1Pub,
			},
			{
				PkScript:    script2,
				Amount:      btcutil.Amount(20000),
				CoSignerKey: key1Pub, // Duplicate!
			},
		}

		err = ValidateVTXODescriptors(vtxos)
		require.Error(t, err)
		require.Contains(t, err.Error(), "duplicate co-signer key")
	})

	t.Run("duplicate detection multiple VTXOs", func(t *testing.T) {
		script2, err := txscript.PayToTaprootScript(key2Pub)
		require.NoError(t, err)

		script3, err := txscript.PayToTaprootScript(key3Pub)
		require.NoError(t, err)

		vtxos := []VTXODescriptor{
			{
				PkScript:    validTaprootScript,
				Amount:      btcutil.Amount(10000),
				CoSignerKey: key1Pub,
			},
			{
				PkScript:    script2,
				Amount:      btcutil.Amount(20000),
				CoSignerKey: key2Pub,
			},
			{
				PkScript:    script3,
				Amount:      btcutil.Amount(30000),
				CoSignerKey: key2Pub, // Duplicate of second!
			},
		}

		err = ValidateVTXODescriptors(vtxos)
		require.Error(t, err)
		require.Contains(t, err.Error(), "duplicate co-signer key")
	})
}

// TestValidateConnectorDescriptor tests the ValidateConnectorDescriptor
// function to ensure proper validation of connector descriptor parameters
// including the number of leaves, amounts, and scripts.
func TestValidateConnectorDescriptor(t *testing.T) {
	// Generate test key for valid script.
	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	keyPub := key.PubKey()

	// Create a valid taproot script.
	validTaprootScript, err := txscript.PayToTaprootScript(keyPub)
	require.NoError(t, err)

	t.Run("valid connector descriptor", func(t *testing.T) {
		conn := ConnectorDescriptor{
			PkScript:  validTaprootScript,
			NumLeaves: 4,
			Amount:    btcutil.Amount(330),
		}

		err := ValidateConnectorDescriptor(conn)
		require.NoError(t, err)
	})

	t.Run("zero NumLeaves fails", func(t *testing.T) {
		conn := ConnectorDescriptor{
			PkScript:  validTaprootScript,
			NumLeaves: 0,
			Amount:    btcutil.Amount(330),
		}

		err := ValidateConnectorDescriptor(conn)
		require.Error(t, err)
		require.Contains(t, err.Error(), "invalid NumLeaves")
	})

	t.Run("negative NumLeaves fails", func(t *testing.T) {
		conn := ConnectorDescriptor{
			PkScript:  validTaprootScript,
			NumLeaves: -5,
			Amount:    btcutil.Amount(330),
		}

		err := ValidateConnectorDescriptor(conn)
		require.Error(t, err)
		require.Contains(t, err.Error(), "invalid NumLeaves")
	})

	t.Run("zero amount fails", func(t *testing.T) {
		conn := ConnectorDescriptor{
			PkScript:  validTaprootScript,
			NumLeaves: 4,
			Amount:    btcutil.Amount(0),
		}

		err := ValidateConnectorDescriptor(conn)
		require.Error(t, err)
		require.Contains(t, err.Error(), "invalid amount")
	})

	t.Run("negative amount fails", func(t *testing.T) {
		conn := ConnectorDescriptor{
			PkScript:  validTaprootScript,
			NumLeaves: 4,
			Amount:    btcutil.Amount(-100),
		}

		err := ValidateConnectorDescriptor(conn)
		require.Error(t, err)
		require.Contains(t, err.Error(), "invalid amount")
	})

	t.Run("empty PkScript fails", func(t *testing.T) {
		conn := ConnectorDescriptor{
			PkScript:  []byte{},
			NumLeaves: 4,
			Amount:    btcutil.Amount(330),
		}

		err := ValidateConnectorDescriptor(conn)
		require.Error(t, err)
		require.Contains(t, err.Error(), "empty PkScript")
	})

	t.Run("non-taproot script fails", func(t *testing.T) {
		// Connector trees must use taproot.
		arbitraryScript := []byte{0x51} // OP_TRUE

		conn := ConnectorDescriptor{
			PkScript:  arbitraryScript,
			NumLeaves: 4,
			Amount:    btcutil.Amount(330),
		}

		err := ValidateConnectorDescriptor(conn)
		require.Error(t, err)
		require.Contains(t, err.Error(), "invalid taproot script")
	})
}

// TestMakeVTXODescriptor tests the VTXO descriptor construction helper.
func TestMakeVTXODescriptor(t *testing.T) {
	timeoutKey, _ := testutils.CreateKey(1)
	cosignerKey, _ := testutils.CreateKey(2)

	t.Run("creates valid descriptor", func(t *testing.T) {
		desc, err := NewVTXODescriptor(
			btcutil.Amount(5000),
			timeoutKey,
			cosignerKey,
			144, // exit delay
		)
		require.NoError(t, err)
		require.NotNil(t, desc)

		// Verify descriptor fields.
		require.Equal(t, btcutil.Amount(5000), desc.Amount)
		require.Equal(t, timeoutKey, desc.CoSignerKey)

		// Verify PkScript is valid taproot.
		require.NotEmpty(t, desc.PkScript)
		require.True(t, txscript.IsPayToTaproot(desc.PkScript))

		// Verify descriptor passes validation.
		err = ValidateVTXODescriptors([]VTXODescriptor{*desc})
		require.NoError(t, err)
	})

	t.Run("integrates with arkscript", func(t *testing.T) {
		// Create multiple VTXOs with different cosigner keys.
		cosigner1, _ := testutils.CreateKey(10)
		operator, _ := testutils.CreateKey(20)
		desc1, err := NewVTXODescriptor(
			btcutil.Amount(1000), cosigner1, operator, 144,
		)
		require.NoError(t, err)

		cosigner2, _ := testutils.CreateKey(20)
		desc2, err := NewVTXODescriptor(
			btcutil.Amount(2000), cosigner2, operator, 144,
		)
		require.NoError(t, err)

		// Both should be valid and have unique cosigners.
		err = ValidateVTXODescriptors([]VTXODescriptor{*desc1, *desc2})
		require.NoError(t, err)
	})
}

// TestNewBranchSweepSpendInfo tests the NewBranchSweepSpendInfo function
// that creates the spending information for sweeping branch outputs after
// the CSV delay has expired.
func TestNewBranchSweepSpendInfo(t *testing.T) {
	t.Parallel()

	internalKey, _ := testutils.CreateKey(5)
	sweepKey, _ := testutils.CreateKey(6)
	csvDelay := uint32(144)

	spendInfo, err := NewBranchSweepSpendInfo(
		internalKey, sweepKey, csvDelay,
	)
	require.NoError(t, err)
	require.NotNil(t, spendInfo)
	require.NotEmpty(t, spendInfo.WitnessScript)
	require.NotEmpty(t, spendInfo.ControlBlock)

	timeoutLeaf, err := arkscript.UnilateralCSVTimeoutTapLeaf(
		sweepKey, csvDelay,
	)
	require.NoError(t, err)
	require.Equal(t, timeoutLeaf.Script, spendInfo.WitnessScript)

	ctrlBlock, err := txscript.ParseControlBlock(
		spendInfo.ControlBlock,
	)
	require.NoError(t, err)
	require.Equal(
		t, schnorr.SerializePubKey(internalKey),
		schnorr.SerializePubKey(ctrlBlock.InternalKey),
	)
	require.Empty(t, ctrlBlock.InclusionProof)
}
