package tree

import (
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
		sweepDelay := closure.RelativeLocktime{
			Type:  closure.LocktimeTypeBlock,
			Value: 144,
		}

		// Create VTXO descriptor using the new constructor.
		vtxoDesc, err := NewDefaultVTXODescriptor(
			btcutil.Amount(5000), user1PubKey, operatorPubKey,
			sweepDelay,
		)
		require.NoError(t, err)

		vtxos := []VTXODescriptor{*vtxoDesc}

		tree, err := BuildVTXOTree(
			batchOutpoint,
			batchOutput,
			vtxos,
			operatorPubKey,
			sweepPubKey,
			sweepDelay,
			2, // radix
		)
		require.NoError(t, err)
		require.NotNil(t, tree)

		// Verify tree context.
		require.Equal(t, batchOutpoint, tree.BatchOutpoint)
		require.Equal(t, batchOutput, tree.BatchOutput)
		require.NotNil(t, tree.SweepTapscriptRoot)

		// Root should be a leaf transaction.
		require.Equal(t, batchOutpoint, tree.Root.Input)
		require.True(t, tree.Root.IsLeaf())
		require.Len(t, tree.Root.Children, 0)

		// Should have VTXO output + anchor output.
		require.Len(t, tree.Root.Outputs, 2)
		require.Equal(t, int64(5000), tree.Root.Outputs[0].Value)

		// Verify output script matches derived PkScript.
		expectedPkScript, err := vtxoDesc.PkScript()
		require.NoError(t, err)
		require.Equal(t, expectedPkScript, tree.Root.Outputs[0].PkScript)
		require.Equal(t, int64(0), tree.Root.Outputs[1].Value)

		// Should have operator + user1 as cosigners.
		require.Len(t, tree.Root.CoSigners, 2)
		require.Contains(t, tree.Root.CoSigners, operatorPubKey)
		require.Contains(t, tree.Root.CoSigners, user1PubKey)
	})

	t.Run("two VTXO tree", func(t *testing.T) {
		sweepDelay := closure.RelativeLocktime{
			Type:  closure.LocktimeTypeBlock,
			Value: 144,
		}

		vtxo1, err := NewDefaultVTXODescriptor(
			btcutil.Amount(5000), user1PubKey, operatorPubKey,
			sweepDelay,
		)
		require.NoError(t, err)

		vtxo2, err := NewDefaultVTXODescriptor(
			btcutil.Amount(3000), user2PubKey, operatorPubKey,
			sweepDelay,
		)
		require.NoError(t, err)

		vtxos := []VTXODescriptor{*vtxo1, *vtxo2}

		tree, err := BuildVTXOTree(
			batchOutpoint,
			batchOutput,
			vtxos,
			operatorPubKey,
			sweepPubKey,
			sweepDelay,
			2,
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
		sweepDelay := closure.RelativeLocktime{
			Type:  closure.LocktimeTypeBlock,
			Value: 144,
		}

		// Create 5 VTXOs with different amounts.
		vtxos := make([]VTXODescriptor, 5)
		for i := 0; i < 5; i++ {
			key, err := btcec.NewPrivateKey()
			require.NoError(t, err)

			vtxo, err := NewDefaultVTXODescriptor(
				btcutil.Amount(1000*(5-i)), key.PubKey(),
				operatorPubKey, sweepDelay,
			)
			require.NoError(t, err)
			vtxos[i] = *vtxo
		}

		tree, err := BuildVTXOTree(
			batchOutpoint,
			batchOutput,
			vtxos,
			operatorPubKey,
			sweepPubKey,
			sweepDelay,
			2,
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
		sweepDelay := closure.RelativeLocktime{
			Type:  closure.LocktimeTypeBlock,
			Value: 144,
		}

		validVTXOPtr, err := NewDefaultVTXODescriptor(
			btcutil.Amount(1000), user1PubKey, operatorPubKey,
			sweepDelay,
		)
		require.NoError(t, err)
		validVTXO := *validVTXOPtr

		t.Run("empty VTXOs fails", func(t *testing.T) {
			tree, err := BuildVTXOTree(
				batchOutpoint, batchOutput,
				[]VTXODescriptor{},
				operatorPubKey, sweepPubKey, sweepDelay, 2,
			)
			require.Error(t, err)
			require.Nil(t, tree)
			require.Contains(t, err.Error(),
				"invalid VTXO descriptors")
		})

		t.Run("radix too small fails", func(t *testing.T) {
			tree, err := BuildVTXOTree(
				batchOutpoint, batchOutput,
				[]VTXODescriptor{validVTXO},
				operatorPubKey, sweepPubKey, sweepDelay, 1,
			)
			require.Error(t, err)
			require.Nil(t, tree)
			require.Contains(t, err.Error(),
				"radix must be at least 2")
		})

		t.Run("nil operator key fails", func(t *testing.T) {
			tree, err := BuildVTXOTree(
				batchOutpoint, batchOutput,
				[]VTXODescriptor{validVTXO},
				nil, sweepPubKey, sweepDelay, 2,
			)
			require.Error(t, err)
			require.Nil(t, tree)
			require.Contains(t, err.Error(), "operator co-sign key")
		})

		t.Run("nil sweep key fails", func(t *testing.T) {
			tree, err := BuildVTXOTree(
				batchOutpoint, batchOutput,
				[]VTXODescriptor{validVTXO},
				operatorPubKey, nil, sweepDelay, 2,
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
	batchOutput := &wire.TxOut{Value: 10000, PkScript: []byte("batch")}

	sweepDelay := closure.RelativeLocktime{
		Type:  closure.LocktimeTypeBlock,
		Value: 144,
	}

	// Create 3 VTXOs with SAME amount but different scripts.
	coSigner1, _ := testutils.CreateKey(101)
	coSigner2, _ := testutils.CreateKey(102)
	coSigner3, _ := testutils.CreateKey(103)

	vtxo1, err := NewDefaultVTXODescriptor(1000, coSigner1, operatorKey,
		sweepDelay)
	require.NoError(t, err)

	vtxo2, err := NewDefaultVTXODescriptor(1000, coSigner2, operatorKey,
		sweepDelay)
	require.NoError(t, err)

	vtxo3, err := NewDefaultVTXODescriptor(1000, coSigner3, operatorKey,
		sweepDelay)
	require.NoError(t, err)

	// Order intentionally mixed.
	vtxos := []VTXODescriptor{*vtxo3, *vtxo1, *vtxo2}

	// Build tree multiple times - should be identical.
	tree1, err := BuildVTXOTree(
		batchOutpoint, batchOutput, vtxos, operatorKey, sweepKey,
		sweepDelay, 2,
	)
	require.NoError(t, err)

	tree2, err := BuildVTXOTree(
		batchOutpoint, batchOutput, vtxos, operatorKey, sweepKey,
		sweepDelay, 2,
	)
	require.NoError(t, err)

	// Trees should be identical (same TXIDs).
	txid1, err := tree1.Root.TXID()
	require.NoError(t, err)

	txid2, err := tree2.Root.TXID()
	require.NoError(t, err)

	require.Equal(t, txid1, txid2,
		"tree construction should be deterministic")

	// Verify leaves are sorted by PkScript (tiebreaker).
	leaves1 := tree1.Root.GetLeafNodes()
	leaves2 := tree2.Root.GetLeafNodes()

	require.Len(t, leaves1, 3)
	require.Len(t, leaves2, 3)

	for i := range leaves1 {
		require.Equal(t, leaves1[i].Outputs[0].PkScript,
			leaves2[i].Outputs[0].PkScript,
			"leaf %d should have same script", i)
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

		tree, err := BuildConnectorTree(
			batchOutpoint,
			batchOutput,
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

		tree, err := BuildConnectorTree(
			batchOutpoint,
			batchOutput,
			connector,
			operatorPubKey,
			4,
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

			// Verify connector output is identical.
			require.Equal(t, int64(330), child.Outputs[0].Value)
			require.Equal(t, connectorScript,
				child.Outputs[0].PkScript)
		}

		// Verify total leaves.
		leaves := tree.Root.GetLeafNodes()
		require.Len(t, leaves, 4)

		// All leaves should have identical scripts.
		for _, leaf := range leaves {
			require.Equal(t, connectorScript,
				leaf.Outputs[0].PkScript)
			require.Equal(t, int64(330), leaf.Outputs[0].Value)
		}
	})

	t.Run("eight connector leaves with radix 4", func(t *testing.T) {
		connector := ConnectorDescriptor{
			PkScript:  connectorScript,
			NumLeaves: 8,
			Amount:    btcutil.Amount(330),
		}

		tree, err := BuildConnectorTree(
			batchOutpoint,
			batchOutput,
			connector,
			operatorPubKey,
			4,
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
			require.Equal(t, connectorScript,
				leaf.Outputs[0].PkScript, "leaf %d", i)
			require.Equal(t, int64(330),
				leaf.Outputs[0].Value, "leaf %d", i)
		}
	})

	t.Run("extract by index works", func(t *testing.T) {
		connector := ConnectorDescriptor{
			PkScript:  connectorScript,
			NumLeaves: 4,
			Amount:    btcutil.Amount(330),
		}

		tree, err := BuildConnectorTree(
			batchOutpoint,
			batchOutput,
			connector,
			operatorPubKey,
			2,
		)
		require.NoError(t, err)

		// Extract each connector by index.
		for i := 0; i < 4; i++ {
			extracted, err := tree.ExtractPathForIndex(i)
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
			require.Contains(t, err.Error(),
				"invalid connector descriptor")
		})

		t.Run("radix too small fails", func(t *testing.T) {
			tree, err := BuildConnectorTree(
				batchOutpoint, batchOutput, connector,
				operatorPubKey, 1,
			)
			require.Error(t, err)
			require.Nil(t, tree)
			require.Contains(t, err.Error(),
				"radix must be at least 2")
		})

		t.Run("nil operator key fails", func(t *testing.T) {
			tree, err := BuildConnectorTree(
				batchOutpoint, batchOutput, connector,
				nil, 4,
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

		tree, err := BuildConnectorTree(
			batchOutpoint,
			batchOutput,
			connector,
			operatorPubKey,
			2,
		)
		require.NoError(t, err)

		leaves := tree.Root.GetLeafNodes()
		require.Len(t, leaves, 4)

		// All leaves should have identical outputs.
		firstLeafOutput := leaves[0].Outputs[0]
		for i, leaf := range leaves {
			require.Equal(t, firstLeafOutput.Value,
				leaf.Outputs[0].Value, "leaf %d value", i)
			require.Equal(t, firstLeafOutput.PkScript,
				leaf.Outputs[0].PkScript, "leaf %d script", i)
		}

		// All leaves should have same cosigner (operator).
		for i, leaf := range leaves {
			require.Len(t, leaf.CoSigners, 2, "leaf %d", i)
			require.Contains(t, leaf.CoSigners, operatorPubKey,
				"leaf %d should contain operator", i)
		}
	})
}

// TestBuildBatchOutput tests batch output construction for commitment
// transactions.
func TestBuildBatchOutput(t *testing.T) {
	operatorKey, _ := testutils.CreateKey(1)
	sweepKey, _ := testutils.CreateKey(2)

	user1Key, _ := testutils.CreateKey(10)
	user2Key, _ := testutils.CreateKey(20)

	sweepDelay := closure.RelativeLocktime{
		Type:  closure.LocktimeTypeBlock,
		Value: 144,
	}

	t.Run("single VTXO", func(t *testing.T) {
		vtxo, err := NewDefaultVTXODescriptor(
			btcutil.Amount(5000), user1Key, operatorKey, sweepDelay,
		)
		require.NoError(t, err)

		output, err := BuildBatchOutput(
			[]VTXODescriptor{*vtxo}, operatorKey, sweepKey, sweepDelay,
		)
		require.NoError(t, err)
		require.NotNil(t, output)

		// Amount should equal the single VTXO amount.
		require.Equal(t, int64(5000), output.Value)

		// Script should be valid P2TR.
		require.True(t, txscript.IsPayToTaproot(output.PkScript))
	})

	t.Run("multiple VTXOs", func(t *testing.T) {
		vtxo1, err := NewDefaultVTXODescriptor(
			btcutil.Amount(5000), user1Key, operatorKey, sweepDelay,
		)
		require.NoError(t, err)

		vtxo2, err := NewDefaultVTXODescriptor(
			btcutil.Amount(3000), user2Key, operatorKey, sweepDelay,
		)
		require.NoError(t, err)

		output, err := BuildBatchOutput(
			[]VTXODescriptor{*vtxo1, *vtxo2}, operatorKey, sweepKey,
			sweepDelay,
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
		vtxo1, err := NewDefaultVTXODescriptor(
			btcutil.Amount(2000), user1Key, operatorKey, sweepDelay,
		)
		require.NoError(t, err)

		vtxo2, err := NewDefaultVTXODescriptor(
			btcutil.Amount(3000), user1Key, operatorKey, sweepDelay,
		)
		require.NoError(t, err)

		output, err := BuildBatchOutput(
			[]VTXODescriptor{*vtxo1, *vtxo2}, operatorKey, sweepKey,
			sweepDelay,
		)
		require.NoError(t, err)
		require.NotNil(t, output)

		// Amount should still be sum.
		require.Equal(t, int64(5000), output.Value)
	})

	t.Run("error cases", func(t *testing.T) {
		validVTXOPtr, err := NewDefaultVTXODescriptor(
			btcutil.Amount(1000), user1Key, operatorKey, sweepDelay,
		)
		require.NoError(t, err)
		validVTXO := *validVTXOPtr

		t.Run("empty VTXOs fails", func(t *testing.T) {
			output, err := BuildBatchOutput(
				[]VTXODescriptor{},
				operatorKey, sweepKey, sweepDelay,
			)
			require.Error(t, err)
			require.Nil(t, output)
			require.Contains(t, err.Error(), "at least one VTXO")
		})

		t.Run("nil operator key fails", func(t *testing.T) {
			output, err := BuildBatchOutput(
				[]VTXODescriptor{validVTXO},
				nil, sweepKey, sweepDelay,
			)
			require.Error(t, err)
			require.Nil(t, output)
			require.Contains(t, err.Error(), "operator musig key")
		})

		t.Run("nil sweep key fails", func(t *testing.T) {
			output, err := BuildBatchOutput(
				[]VTXODescriptor{validVTXO},
				operatorKey, nil, sweepDelay,
			)
			require.Error(t, err)
			require.Nil(t, output)
			require.Contains(t, err.Error(), "sweep key")
		})
	})
}

// TestBuildConnectorOutput tests connector output construction for commitment
// transactions.
func TestBuildConnectorOutput(t *testing.T) {
	operatorKey, _ := testutils.CreateKey(1)
	connectorAddr, err := btcutil.NewAddressTaproot(
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
			require.Contains(t, err.Error(),
				"num connectors must be > 0")
		})

		t.Run("zero dust amount fails", func(t *testing.T) {
			output, err := BuildConnectorOutput(
				4, btcutil.Amount(0), connectorAddr,
			)
			require.Error(t, err)
			require.Nil(t, output)
			require.Contains(
				t, err.Error(), "dust amount must be > 0",
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

	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorPub := operatorKey.PubKey()

	exitDelay := closure.RelativeLocktime{
		Type:  closure.LocktimeTypeBlock,
		Value: 144,
	}

	t.Run("valid single VTXO", func(t *testing.T) {
		vtxo, err := NewDefaultVTXODescriptor(
			btcutil.Amount(10000), key1Pub, operatorPub, exitDelay,
		)
		require.NoError(t, err)

		err = ValidateVTXODescriptors([]VTXODescriptor{*vtxo})
		require.NoError(t, err)
	})

	t.Run("valid multiple VTXOs", func(t *testing.T) {
		vtxo1, err := NewDefaultVTXODescriptor(
			btcutil.Amount(10000), key1Pub, operatorPub, exitDelay,
		)
		require.NoError(t, err)

		vtxo2, err := NewDefaultVTXODescriptor(
			btcutil.Amount(20000), key2Pub, operatorPub, exitDelay,
		)
		require.NoError(t, err)

		vtxo3, err := NewDefaultVTXODescriptor(
			btcutil.Amount(30000), key3Pub, operatorPub, exitDelay,
		)
		require.NoError(t, err)

		err = ValidateVTXODescriptors([]VTXODescriptor{
			*vtxo1, *vtxo2, *vtxo3,
		})
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
		vtxo, err := NewDefaultVTXODescriptor(
			btcutil.Amount(1000), key1Pub, operatorPub, exitDelay,
		)
		require.NoError(t, err)

		// Manually set amount to 0 to test validation.
		vtxo.Amount = 0

		err = ValidateVTXODescriptors([]VTXODescriptor{*vtxo})
		require.Error(t, err)
		require.Contains(t, err.Error(), "invalid amount")
	})

	t.Run("negative amount fails", func(t *testing.T) {
		vtxo, err := NewDefaultVTXODescriptor(
			btcutil.Amount(1000), key1Pub, operatorPub, exitDelay,
		)
		require.NoError(t, err)

		// Manually set amount to negative to test validation.
		vtxo.Amount = btcutil.Amount(-1000)

		err = ValidateVTXODescriptors([]VTXODescriptor{*vtxo})
		require.Error(t, err)
		require.Contains(t, err.Error(), "invalid amount")
	})

	t.Run("empty Scripts fails", func(t *testing.T) {
		vtxos := []VTXODescriptor{
			{
				Scripts:     []string{},
				Amount:      btcutil.Amount(10000),
				CoSignerKey: key1Pub,
			},
		}

		err := ValidateVTXODescriptors(vtxos)
		require.Error(t, err)
		require.Contains(t, err.Error(), "empty Scripts")
	})

	t.Run("nil Scripts fails", func(t *testing.T) {
		vtxos := []VTXODescriptor{
			{
				Scripts:     nil,
				Amount:      btcutil.Amount(10000),
				CoSignerKey: key1Pub,
			},
		}

		err := ValidateVTXODescriptors(vtxos)
		require.Error(t, err)
		require.Contains(t, err.Error(), "empty Scripts")
	})

	t.Run("invalid script hex fails", func(t *testing.T) {
		vtxos := []VTXODescriptor{
			{
				Scripts:     []string{"not_valid_hex"},
				Amount:      btcutil.Amount(10000),
				CoSignerKey: key1Pub,
			},
		}

		err := ValidateVTXODescriptors(vtxos)
		require.Error(t, err)
		require.Contains(t, err.Error(), "invalid scripts")
	})

	t.Run("nil co-signer key fails", func(t *testing.T) {
		vtxo, err := NewDefaultVTXODescriptor(
			btcutil.Amount(10000), key1Pub, operatorPub, exitDelay,
		)
		require.NoError(t, err)

		// Manually set CoSignerKey to nil.
		vtxo.CoSignerKey = nil

		err = ValidateVTXODescriptors([]VTXODescriptor{*vtxo})
		require.Error(t, err)
		require.Contains(t, err.Error(), "nil co-signer key")
	})

	t.Run("duplicate co-signer keys fail", func(t *testing.T) {
		vtxo1, err := NewDefaultVTXODescriptor(
			btcutil.Amount(10000), key1Pub, operatorPub, exitDelay,
		)
		require.NoError(t, err)

		vtxo2, err := NewDefaultVTXODescriptor(
			btcutil.Amount(20000), key2Pub, operatorPub, exitDelay,
		)
		require.NoError(t, err)

		// Set the second VTXO to use the same cosigner key.
		vtxo2.CoSignerKey = key1Pub

		err = ValidateVTXODescriptors([]VTXODescriptor{*vtxo1, *vtxo2})
		require.Error(t, err)
		require.Contains(t, err.Error(), "duplicate co-signer key")
	})

	t.Run("duplicate detection multiple VTXOs", func(t *testing.T) {
		vtxo1, err := NewDefaultVTXODescriptor(
			btcutil.Amount(10000), key1Pub, operatorPub, exitDelay,
		)
		require.NoError(t, err)

		vtxo2, err := NewDefaultVTXODescriptor(
			btcutil.Amount(20000), key2Pub, operatorPub, exitDelay,
		)
		require.NoError(t, err)

		vtxo3, err := NewDefaultVTXODescriptor(
			btcutil.Amount(30000), key3Pub, operatorPub, exitDelay,
		)
		require.NoError(t, err)

		// Set the third VTXO to use key2's cosigner key.
		vtxo3.CoSignerKey = key2Pub

		err = ValidateVTXODescriptors([]VTXODescriptor{
			*vtxo1, *vtxo2, *vtxo3,
		})
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

// TestMakeVTXODescriptor tests the VTXO descriptor construction helpers.
func TestMakeVTXODescriptor(t *testing.T) {
	ownerKey, _ := testutils.CreateKey(1)
	cosignerKey, _ := testutils.CreateKey(2)

	exitDelay := closure.RelativeLocktime{
		Type:  closure.LocktimeTypeBlock,
		Value: 144,
	}

	t.Run("NewDefaultVTXODescriptor creates valid descriptor", func(t *testing.T) {
		desc, err := NewDefaultVTXODescriptor(
			btcutil.Amount(5000),
			ownerKey,
			cosignerKey,
			exitDelay,
		)
		require.NoError(t, err)
		require.NotNil(t, desc)

		// Verify descriptor fields.
		require.Equal(t, btcutil.Amount(5000), desc.Amount)
		require.Equal(t, ownerKey, desc.CoSignerKey)
		require.NotEmpty(t, desc.Scripts)

		// Verify PkScript() derives valid taproot.
		pkScript, err := desc.PkScript()
		require.NoError(t, err)
		require.NotEmpty(t, pkScript)
		require.True(t, txscript.IsPayToTaproot(pkScript))

		// Verify VtxoScript() parses correctly.
		vtxoScript, err := desc.VtxoScript()
		require.NoError(t, err)
		require.Len(t, vtxoScript.Closures, 2) // Exit + Collab

		// Verify descriptor passes validation.
		err = ValidateVTXODescriptors([]VTXODescriptor{*desc})
		require.NoError(t, err)
	})

	t.Run("NewVTXODescriptor accepts custom closures", func(t *testing.T) {
		// Create a custom VTXO script with only an exit closure.
		customScript := &closure.TapscriptsVtxoScript{
			Closures: []closure.Closure{
				&closure.CSVSigClosure{
					PubKey:   ownerKey,
					Locktime: exitDelay,
				},
			},
		}

		desc, err := NewVTXODescriptor(
			btcutil.Amount(3000), customScript, ownerKey,
		)
		require.NoError(t, err)
		require.NotNil(t, desc)

		// Verify closure was preserved.
		vtxoScript, err := desc.VtxoScript()
		require.NoError(t, err)
		require.Len(t, vtxoScript.Closures, 1)

		// Verify it's a CSVSigClosure.
		_, ok := vtxoScript.Closures[0].(*closure.CSVSigClosure)
		require.True(t, ok)
	})

	t.Run("integrates with scripts package", func(t *testing.T) {
		// Create multiple VTXOs with different cosigner keys.
		cosigner1, _ := testutils.CreateKey(10)
		operator, _ := testutils.CreateKey(20)
		desc1, err := NewDefaultVTXODescriptor(
			btcutil.Amount(1000),
			cosigner1,
			operator,
			exitDelay,
		)
		require.NoError(t, err)

		cosigner2, _ := testutils.CreateKey(30)
		desc2, err := NewDefaultVTXODescriptor(
			btcutil.Amount(2000),
			cosigner2,
			operator,
			exitDelay,
		)
		require.NoError(t, err)

		// Both should be valid and have unique cosigners.
		err = ValidateVTXODescriptors([]VTXODescriptor{*desc1, *desc2})
		require.NoError(t, err)
	})

	t.Run("error cases", func(t *testing.T) {
		t.Run("nil vtxo script fails", func(t *testing.T) {
			_, err := NewVTXODescriptor(
				btcutil.Amount(1000), nil, ownerKey,
			)
			require.Error(t, err)
			require.Contains(t, err.Error(), "vtxo script cannot be nil")
		})

		t.Run("nil cosigner key fails", func(t *testing.T) {
			customScript := &closure.TapscriptsVtxoScript{
				Closures: []closure.Closure{
					&closure.CSVSigClosure{
						PubKey:   ownerKey,
						Locktime: exitDelay,
					},
				},
			}
			_, err := NewVTXODescriptor(
				btcutil.Amount(1000), customScript, nil,
			)
			require.Error(t, err)
			require.Contains(t, err.Error(),
				"cosigner key cannot be nil")
		})
	})
}

// TestBuildVTXOTreeWithCustomClosures tests that the VTXO tree can be built
// with non-default closure configurations, verifying that custom VTXODescriptors
// created via NewVTXODescriptor work correctly and that the closure roundtrip
// through VtxoScript() preserves the original closure structure.
func TestBuildVTXOTreeWithCustomClosures(t *testing.T) {
	t.Parallel()

	operatorKey, _ := testutils.CreateKey(1)
	sweepKey, _ := testutils.CreateKey(2)

	batchOutpoint := wire.OutPoint{
		Hash:  chainhash.HashH([]byte("custom_closures_test")),
		Index: 0,
	}
	batchOutput := &wire.TxOut{
		Value:    20000,
		PkScript: []byte("batch_script"),
	}

	sweepDelay := closure.RelativeLocktime{
		Type:  closure.LocktimeTypeBlock,
		Value: 144,
	}

	t.Run("CSVMultisigClosure exit with MultisigClosure collab", func(t *testing.T) {
		client1Key, _ := testutils.CreateKey(10)
		client2Key, _ := testutils.CreateKey(11)

		exitDelay := closure.RelativeLocktime{
			Type:  closure.LocktimeTypeBlock,
			Value: 100,
		}

		// Create a custom VTXO script with CSVMultisigClosure for exit.
		customScript := &closure.TapscriptsVtxoScript{
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

		// Create VTXO descriptor with custom closures.
		vtxoDesc, err := NewVTXODescriptor(
			btcutil.Amount(5000), customScript, client1Key,
		)
		require.NoError(t, err)

		// Verify roundtrip: VtxoScript() should reconstruct closures.
		roundtrip, err := vtxoDesc.VtxoScript()
		require.NoError(t, err)
		require.Len(t, roundtrip.Closures, 2)

		// Verify first closure is CSVMultisigClosure.
		csvMultisig, ok := roundtrip.Closures[0].(*closure.CSVMultisigClosure)
		require.True(t, ok, "first closure should be CSVMultisigClosure")
		require.Len(t, csvMultisig.PubKeys, 2)
		require.Equal(t, exitDelay.Value, csvMultisig.Locktime.Value)

		// Verify second closure is MultisigClosure.
		multisig, ok := roundtrip.Closures[1].(*closure.MultisigClosure)
		require.True(t, ok, "second closure should be MultisigClosure")
		require.Len(t, multisig.PubKeys, 3)

		// Build tree with custom VTXO.
		tree, err := BuildVTXOTree(
			batchOutpoint, batchOutput,
			[]VTXODescriptor{*vtxoDesc},
			operatorKey, sweepKey, sweepDelay, 2,
		)
		require.NoError(t, err)
		require.NotNil(t, tree)

		// Verify tree structure.
		require.True(t, tree.Root.IsLeaf())
		require.Len(t, tree.Root.Outputs, 2) // VTXO + anchor

		// Verify PkScript matches the custom script.
		expectedPkScript, err := vtxoDesc.PkScript()
		require.NoError(t, err)
		require.Equal(t, expectedPkScript, tree.Root.Outputs[0].PkScript)
	})

	t.Run("exit-only VTXO (single closure, no collab)", func(t *testing.T) {
		clientKey, _ := testutils.CreateKey(20)

		exitDelay := closure.RelativeLocktime{
			Type:  closure.LocktimeTypeBlock,
			Value: 50,
		}

		// VTXO with only a single exit closure - no collaborative path.
		customScript := &closure.TapscriptsVtxoScript{
			Closures: []closure.Closure{
				&closure.CSVSigClosure{
					PubKey:   clientKey,
					Locktime: exitDelay,
				},
			},
		}

		vtxoDesc, err := NewVTXODescriptor(
			btcutil.Amount(3000), customScript, clientKey,
		)
		require.NoError(t, err)

		// Verify roundtrip preserves the single closure.
		roundtrip, err := vtxoDesc.VtxoScript()
		require.NoError(t, err)
		require.Len(t, roundtrip.Closures, 1)

		csvSig, ok := roundtrip.Closures[0].(*closure.CSVSigClosure)
		require.True(t, ok, "closure should be CSVSigClosure")
		require.Equal(t, exitDelay.Value, csvSig.Locktime.Value)

		// Build tree with exit-only VTXO.
		tree, err := BuildVTXOTree(
			batchOutpoint, batchOutput,
			[]VTXODescriptor{*vtxoDesc},
			operatorKey, sweepKey, sweepDelay, 2,
		)
		require.NoError(t, err)
		require.NotNil(t, tree)

		// Verify output script.
		expectedPkScript, err := vtxoDesc.PkScript()
		require.NoError(t, err)
		require.Equal(t, expectedPkScript, tree.Root.Outputs[0].PkScript)
	})

	t.Run("multiple VTXOs with different closure configurations", func(t *testing.T) {
		user1Key, _ := testutils.CreateKey(30)
		user2Key, _ := testutils.CreateKey(31)
		user3Key, _ := testutils.CreateKey(32)

		exitDelay := closure.RelativeLocktime{
			Type:  closure.LocktimeTypeBlock,
			Value: 100,
		}

		// First VTXO: default configuration.
		vtxo1, err := NewDefaultVTXODescriptor(
			btcutil.Amount(1000), user1Key, operatorKey, exitDelay,
		)
		require.NoError(t, err)

		// Second VTXO: custom 2-of-2 exit.
		customScript2 := &closure.TapscriptsVtxoScript{
			Closures: []closure.Closure{
				&closure.CSVMultisigClosure{
					MultisigClosure: closure.MultisigClosure{
						PubKeys: []*btcec.PublicKey{
							user2Key, operatorKey,
						},
						Type: closure.MultisigTypeChecksig,
					},
					Locktime: exitDelay,
				},
				&closure.MultisigClosure{
					PubKeys: []*btcec.PublicKey{
						user2Key, operatorKey,
					},
					Type: closure.MultisigTypeChecksig,
				},
			},
		}
		vtxo2, err := NewVTXODescriptor(
			btcutil.Amount(2000), customScript2, user2Key,
		)
		require.NoError(t, err)

		// Third VTXO: exit-only.
		customScript3 := &closure.TapscriptsVtxoScript{
			Closures: []closure.Closure{
				&closure.CSVSigClosure{
					PubKey:   user3Key,
					Locktime: exitDelay,
				},
			},
		}
		vtxo3, err := NewVTXODescriptor(
			btcutil.Amount(3000), customScript3, user3Key,
		)
		require.NoError(t, err)

		// Build tree with mixed VTXO types.
		tree, err := BuildVTXOTree(
			batchOutpoint, batchOutput,
			[]VTXODescriptor{*vtxo1, *vtxo2, *vtxo3},
			operatorKey, sweepKey, sweepDelay, 2,
		)
		require.NoError(t, err)
		require.NotNil(t, tree)

		// Verify all leaves are present.
		leaves := tree.Root.GetLeafNodes()
		require.Len(t, leaves, 3)

		// Verify total amount is preserved.
		var totalAmount int64
		for _, leaf := range leaves {
			totalAmount += leaf.Outputs[0].Value
		}
		require.Equal(t, int64(6000), totalAmount)
	})

	t.Run("CLTVMultisigClosure for absolute timelock", func(t *testing.T) {
		clientKey, _ := testutils.CreateKey(40)

		// CLTV absolute locktime (block height).
		cltvLocktime := closure.AbsoluteLocktime(850000)

		// VTXO with CLTVMultisigClosure for absolute timelock exit.
		customScript := &closure.TapscriptsVtxoScript{
			Closures: []closure.Closure{
				// Exit: after CLTV block height.
				&closure.CLTVMultisigClosure{
					MultisigClosure: closure.MultisigClosure{
						PubKeys: []*btcec.PublicKey{clientKey},
						Type:    closure.MultisigTypeChecksig,
					},
					Locktime: cltvLocktime,
				},
				// Collab path.
				&closure.MultisigClosure{
					PubKeys: []*btcec.PublicKey{
						clientKey, operatorKey,
					},
					Type: closure.MultisigTypeChecksig,
				},
			},
		}

		vtxoDesc, err := NewVTXODescriptor(
			btcutil.Amount(4000), customScript, clientKey,
		)
		require.NoError(t, err)

		// Verify roundtrip.
		roundtrip, err := vtxoDesc.VtxoScript()
		require.NoError(t, err)
		require.Len(t, roundtrip.Closures, 2)

		cltvMultisig, ok := roundtrip.Closures[0].(*closure.CLTVMultisigClosure)
		require.True(t, ok, "first closure should be CLTVMultisigClosure")
		require.Equal(t, cltvLocktime, cltvMultisig.Locktime)

		// Build tree.
		tree, err := BuildVTXOTree(
			batchOutpoint, batchOutput,
			[]VTXODescriptor{*vtxoDesc},
			operatorKey, sweepKey, sweepDelay, 2,
		)
		require.NoError(t, err)
		require.NotNil(t, tree)
		require.True(t, tree.Root.IsLeaf())
	})
}

// TestNewBranchSweepSpendInfo tests the NewBranchSweepSpendInfo function
// that creates the spending information for sweeping branch outputs after
// the CSV delay has expired.
func TestNewBranchSweepSpendInfo(t *testing.T) {
	t.Parallel()

	internalKey, _ := testutils.CreateKey(5)
	sweepKey, _ := testutils.CreateKey(6)
	csvDelay := closure.RelativeLocktime{
		Type:  closure.LocktimeTypeBlock,
		Value: 144,
	}

	spendInfo, err := NewBranchSweepSpendInfo(
		internalKey, sweepKey, csvDelay,
	)
	require.NoError(t, err)
	require.NotNil(t, spendInfo)
	require.NotEmpty(t, spendInfo.WitnessScript)
	require.NotEmpty(t, spendInfo.ControlBlock)

	timeoutLeaf, err := scripts.CSVTimeoutTapLeaf(
		sweepKey, csvDelay,
	)
	require.NoError(t, err)
	require.Equal(t, timeoutLeaf.Script, spendInfo.WitnessScript)

	ctrlBlock, err := txscript.ParseControlBlock(
		spendInfo.ControlBlock,
	)
	require.NoError(t, err)
	require.Equal(t,
		schnorr.SerializePubKey(internalKey),
		schnorr.SerializePubKey(ctrlBlock.InternalKey),
	)
	require.Empty(t, ctrlBlock.InclusionProof)
}
