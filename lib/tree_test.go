package lib

import (
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
)

// TODO(elle): re-write these claude-written tests.

func TestExtractPathForCosigner(t *testing.T) {
	// Generate test keys
	operatorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorPubKey := operatorKey.PubKey()

	user1Key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	user1PubKey := user1Key.PubKey()

	user2Key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	user2PubKey := user2Key.PubKey()

	user3Key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	user3PubKey := user3Key.PubKey()

	// Create a sample tree structure
	// Root node with operator + user1 + user2 + user3
	root := &TreeNode{
		Input: &wire.OutPoint{
			Hash:  [32]byte{1, 2, 3},
			Index: 0,
		},
		CoSigners: []*btcec.PublicKey{operatorPubKey, user1PubKey, user2PubKey, user3PubKey},
		Outputs: []*wire.TxOut{
			{Value: 1000, PkScript: []byte("output1")},
			{Value: 2000, PkScript: []byte("output2")},
			{Value: 0, PkScript: []byte("anchor")},
		},
		Children: make(map[uint32]*TreeNode),
	}

	// Child 1: operator + user1 (user1's path)
	child1 := &TreeNode{
		Input: &wire.OutPoint{
			Hash:  [32]byte{1, 2, 3},
			Index: 0,
		},
		CoSigners: []*btcec.PublicKey{operatorPubKey, user1PubKey},
		Outputs: []*wire.TxOut{
			{Value: 1000, PkScript: []byte("user1_vtxo")},
			{Value: 0, PkScript: []byte("anchor")},
		},
		Children: make(map[uint32]*TreeNode),
	}

	// Child 2: operator + user2 + user3 (not user1's path)
	child2 := &TreeNode{
		Input: &wire.OutPoint{
			Hash:  [32]byte{1, 2, 3},
			Index: 1,
		},
		CoSigners: []*btcec.PublicKey{operatorPubKey, user2PubKey, user3PubKey},
		Outputs: []*wire.TxOut{
			{Value: 1000, PkScript: []byte("output_2_1")},
			{Value: 1000, PkScript: []byte("output_2_2")},
			{Value: 0, PkScript: []byte("anchor")},
		},
		Children: make(map[uint32]*TreeNode),
	}

	// Grandchild 2.1: operator + user2 (user2's leaf)
	grandchild21 := &TreeNode{
		Input: &wire.OutPoint{
			Hash:  [32]byte{1, 2, 3},
			Index: 0,
		},
		CoSigners: []*btcec.PublicKey{operatorPubKey, user2PubKey},
		Outputs: []*wire.TxOut{
			{Value: 1000, PkScript: []byte("user2_vtxo")},
			{Value: 0, PkScript: []byte("anchor")},
		},
		Children: make(map[uint32]*TreeNode),
	}

	// Grandchild 2.2: operator + user3 (user3's leaf)
	grandchild22 := &TreeNode{
		Input: &wire.OutPoint{
			Hash:  [32]byte{1, 2, 3},
			Index: 1,
		},
		CoSigners: []*btcec.PublicKey{operatorPubKey, user3PubKey},
		Outputs: []*wire.TxOut{
			{Value: 1000, PkScript: []byte("user3_vtxo")},
			{Value: 0, PkScript: []byte("anchor")},
		},
		Children: make(map[uint32]*TreeNode),
	}

	// Connect the tree
	root.Children[0] = child1
	root.Children[1] = child2
	child2.Children[0] = grandchild21
	child2.Children[1] = grandchild22

	t.Run("extract user1 path", func(t *testing.T) {
		extracted := root.ExtractPathForCosigner(user1PubKey)
		require.NotNil(t, extracted)

		// Should have root node info
		require.Equal(t, root.Input, extracted.Input)
		require.Equal(t, root.CoSigners, extracted.CoSigners)
		require.Equal(t, root.Outputs, extracted.Outputs)

		// Should only have child 0 (user1's path), not child 1
		require.Len(t, extracted.Children, 1)
		require.Contains(t, extracted.Children, uint32(0))
		require.NotContains(t, extracted.Children, uint32(1))

		// Child 0 should be the leaf node for user1
		extractedChild := extracted.Children[0]
		require.Equal(t, child1.Input, extractedChild.Input)
		require.Equal(t, child1.CoSigners, extractedChild.CoSigners)
		require.Equal(t, child1.Outputs, extractedChild.Outputs)
		require.Len(t, extractedChild.Children, 0)
	})

	t.Run("extract user2 path", func(t *testing.T) {
		extracted := root.ExtractPathForCosigner(user2PubKey)
		require.NotNil(t, extracted)

		// Should have root node info
		require.Equal(t, root.Input, extracted.Input)
		require.Equal(t, root.CoSigners, extracted.CoSigners)
		require.Equal(t, root.Outputs, extracted.Outputs)

		// Should only have child 1 (user2's path), not child 0
		require.Len(t, extracted.Children, 1)
		require.Contains(t, extracted.Children, uint32(1))
		require.NotContains(t, extracted.Children, uint32(0))

		// Child 1 should have the intermediate node
		extractedChild := extracted.Children[1]
		require.Equal(t, child2.Input, extractedChild.Input)
		require.Equal(t, child2.CoSigners, extractedChild.CoSigners)

		// Child 1 should only have grandchild 0 (user2's leaf), not grandchild 1
		require.Len(t, extractedChild.Children, 1)
		require.Contains(t, extractedChild.Children, uint32(0))
		require.NotContains(t, extractedChild.Children, uint32(1))

		// Verify the leaf is correct
		leaf := extractedChild.Children[0]
		require.Equal(t, grandchild21.CoSigners, leaf.CoSigners)
		require.Len(t, leaf.Children, 0)
	})

	t.Run("extract user3 path", func(t *testing.T) {
		extracted := root.ExtractPathForCosigner(user3PubKey)
		require.NotNil(t, extracted)

		// Should only have child 1, and child 1 should only have grandchild 1
		require.Len(t, extracted.Children, 1)
		extractedChild := extracted.Children[1]
		require.Len(t, extractedChild.Children, 1)
		require.Contains(t, extractedChild.Children, uint32(1))

		// Verify the leaf is correct
		leaf := extractedChild.Children[1]
		require.Equal(t, grandchild22.CoSigners, leaf.CoSigners)
	})

	t.Run("extract non-existent cosigner", func(t *testing.T) {
		nonExistentKey, err := btcec.NewPrivateKey()
		require.NoError(t, err)

		extracted := root.ExtractPathForCosigner(nonExistentKey.PubKey())
		require.Nil(t, extracted)
	})

	t.Run("extract from nil node", func(t *testing.T) {
		var nilNode *TreeNode
		extracted := nilNode.ExtractPathForCosigner(user1PubKey)
		require.Nil(t, extracted)
	})

	t.Run("operator path should include all nodes", func(t *testing.T) {
		extracted := root.ExtractPathForCosigner(operatorPubKey)
		require.NotNil(t, extracted)

		// Operator is in all nodes, so should get the full tree
		require.Len(t, extracted.Children, 2)
		require.Contains(t, extracted.Children, uint32(0))
		require.Contains(t, extracted.Children, uint32(1))

		// Child 1 should have both grandchildren
		extractedChild1 := extracted.Children[1]
		require.Len(t, extractedChild1.Children, 2)
		require.Contains(t, extractedChild1.Children, uint32(0))
		require.Contains(t, extractedChild1.Children, uint32(1))
	})
}

func TestContainsCosigner(t *testing.T) {
	key1, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	key1Pub := key1.PubKey()

	key2, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	key2Pub := key2.PubKey()

	key3, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	key3Pub := key3.PubKey()

	cosigners := []*btcec.PublicKey{key1Pub, key2Pub}

	t.Run("key present", func(t *testing.T) {
		require.True(t, ContainsCosigner(cosigners, key1Pub))
		require.True(t, ContainsCosigner(cosigners, key2Pub))
	})

	t.Run("key not present", func(t *testing.T) {
		require.False(t, ContainsCosigner(cosigners, key3Pub))
	})

	t.Run("empty cosigners", func(t *testing.T) {
		require.False(t, ContainsCosigner([]*btcec.PublicKey{}, key1Pub))
	})
}

func TestBuildVTXOTree(t *testing.T) {
	// Generate test keys
	operatorSweepKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorSweepPubKey := operatorSweepKey.PubKey()

	operatorSigningKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorSigningPubKey := operatorSigningKey.PubKey()

	user1Key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	user1PubKey := user1Key.PubKey()

	user2Key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	user2PubKey := user2Key.PubKey()

	user3Key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	user3PubKey := user3Key.PubKey()

	// Test input outpoint
	input := &wire.OutPoint{
		Hash:  [32]byte{1, 2, 3, 4, 5},
		Index: 0,
	}

	// Mock previous output
	prevOut := &wire.TxOut{
		Value:    5000,
		PkScript: []byte("mock_script"),
	}

	t.Run("single leaf tree", func(t *testing.T) {
		leaves := []Leaf{
			{
				PkScript:  []byte("user1_script"),
				Amount:    1000,
				SignerKey: user1PubKey,
			},
		}

		tree, err := BuildVTXOTree(
			input,
			leaves,
			144, // CSV delay
			operatorSweepPubKey,
			operatorSigningPubKey,
			2, // radix
			prevOut,
		)
		require.NoError(t, err)
		require.NotNil(t, tree)

		// Root should be a leaf transaction
		require.Equal(t, input, tree.Root.Input)
		require.Len(t, tree.Root.Children, 0) // No children for single leaf

		// Should have VTXO output + anchor output
		require.Len(t, tree.Root.Outputs, 2)
		require.Equal(t, int64(1000), tree.Root.Outputs[0].Value)
		require.Equal(t, []byte("user1_script"), tree.Root.Outputs[0].PkScript)
		require.Equal(t, int64(0), tree.Root.Outputs[1].Value) // Anchor output

		// Should have operator + user1 as cosigners
		require.Len(t, tree.Root.CoSigners, 2)
		require.Contains(t, tree.Root.CoSigners, operatorSigningPubKey)
		require.Contains(t, tree.Root.CoSigners, user1PubKey)
	})

	t.Run("two leaf tree", func(t *testing.T) {
		leaves := []Leaf{
			{
				PkScript:  []byte("user1_script"),
				Amount:    1000,
				SignerKey: user1PubKey,
			},
			{
				PkScript:  []byte("user2_script"),
				Amount:    2000,
				SignerKey: user2PubKey,
			},
		}

		tree, err := BuildVTXOTree(input, leaves, 144, operatorSweepPubKey, operatorSigningPubKey, 2, prevOut)
		require.NoError(t, err)
		require.NotNil(t, tree)

		// Root should be a branch transaction
		require.Equal(t, input, tree.Root.Input)
		require.Len(t, tree.Root.Children, 2) // Two children for two leaves

		// Should have 2 branch outputs + anchor output
		require.Len(t, tree.Root.Outputs, 3)
		require.Equal(t, int64(0), tree.Root.Outputs[2].Value) // Anchor output

		// Should have all unique cosigners at root
		require.Len(t, tree.Root.CoSigners, 3) // operator + user1 + user2
		require.Contains(t, tree.Root.CoSigners, operatorSigningPubKey)
		require.Contains(t, tree.Root.CoSigners, user1PubKey)
		require.Contains(t, tree.Root.CoSigners, user2PubKey)

		// Verify children are leaf transactions
		child0, exists := tree.Root.Children[0]
		require.True(t, exists)
		require.Len(t, child0.Children, 0)  // Leaf node
		require.Len(t, child0.CoSigners, 2) // operator + one user

		child1, exists := tree.Root.Children[1]
		require.True(t, exists)
		require.Len(t, child1.Children, 0)  // Leaf node
		require.Len(t, child1.CoSigners, 2) // operator + one user
	})

	t.Run("five leaf tree with radix 2", func(t *testing.T) {
		leaves := []Leaf{
			{PkScript: []byte("user1"), Amount: 5000, SignerKey: user1PubKey},
			{PkScript: []byte("user2"), Amount: 3000, SignerKey: user2PubKey},
			{PkScript: []byte("user3"), Amount: 2000, SignerKey: user3PubKey},
			{PkScript: []byte("user1_2"), Amount: 1000, SignerKey: user1PubKey},
			{PkScript: []byte("user2_2"), Amount: 500, SignerKey: user2PubKey},
		}

		tree, err := BuildVTXOTree(
			input,
			leaves,
			144,
			operatorSweepPubKey,
			operatorSigningPubKey,
			2, // Binary tree
			prevOut,
		)
		require.NoError(t, err)
		require.NotNil(t, tree)

		// Should create a multi-level tree
		require.Len(t, tree.Root.Children, 2)

		// Verify tree structure by checking that leaves are sorted by amount (descending)
		// and properly distributed across the tree
		var leafCount int
		var totalAmount int64

		var countLeaves func(*TreeNode)
		countLeaves = func(node *TreeNode) {
			if len(node.Children) == 0 {
				// This is a leaf
				leafCount++
				totalAmount += node.Outputs[0].Value
			} else {
				for _, child := range node.Children {
					countLeaves(child)
				}
			}
		}

		countLeaves(tree.Root)
		require.Equal(t, 5, leafCount)
		require.Equal(t, int64(11500), totalAmount) // 5000+3000+2000+1000+500
	})

	t.Run("error cases", func(t *testing.T) {
		t.Run("empty leaves", func(t *testing.T) {
			tree, err := BuildVTXOTree(input, []Leaf{}, 144, operatorSweepPubKey, operatorSigningPubKey, 2, prevOut)
			require.Error(t, err)
			require.Nil(t, tree)
			require.Contains(t, err.Error(), "empty leaves")
		})

		t.Run("radix too small", func(t *testing.T) {
			leaves := []Leaf{
				{PkScript: []byte("test"), Amount: 1000, SignerKey: user1PubKey},
			}

			tree, err := BuildVTXOTree(
				input,
				leaves,
				144,
				operatorSweepPubKey,
				operatorSigningPubKey,
				1, // Invalid radix
				prevOut,
			)
			require.Error(t, err)
			require.Nil(t, tree)
			require.Contains(t, err.Error(), "radix must be at least 2")
		})
	})

	t.Run("amount sorting", func(t *testing.T) {
		// Create leaves in non-sorted order to verify they get sorted
		leaves := []Leaf{
			{PkScript: []byte("small"), Amount: 100, SignerKey: user1PubKey},
			{PkScript: []byte("large"), Amount: 5000, SignerKey: user2PubKey},
			{PkScript: []byte("medium"), Amount: 1000, SignerKey: user3PubKey},
		}

		tree, err := BuildVTXOTree(input, leaves, 144, operatorSweepPubKey, operatorSigningPubKey, 2, prevOut)
		require.NoError(t, err)
		require.NotNil(t, tree)

		// Verify that the tree was built with proper amount-based sorting
		// The largest amount (5000) should be processed first in the LPT algorithm
		require.Len(t, tree.Root.Children, 2)
	})
}

func TestBuildVTXOTreeAndExtractPaths(t *testing.T) {
	// Generate test keys
	operatorSweepKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorSweepPubKey := operatorSweepKey.PubKey()

	operatorSigningKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorSigningPubKey := operatorSigningKey.PubKey()

	// Generate client keys
	alice, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	alicePubKey := alice.PubKey()

	bob, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	bobPubKey := bob.PubKey()

	charlie, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	charliePubKey := charlie.PubKey()

	dave, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	davePubKey := dave.PubKey()

	eve, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	evePubKey := eve.PubKey()

	// Test input outpoint
	input := &wire.OutPoint{
		Hash:  [32]byte{1, 2, 3, 4, 5, 6, 7, 8},
		Index: 0,
	}

	// Mock previous output
	prevOut := &wire.TxOut{
		Value:    15000, // Total amount for all leaves
		PkScript: []byte("mock_prev_script"),
	}

	t.Run("build tree and extract client paths", func(t *testing.T) {
		// Create leaves for multiple clients with different amounts
		leaves := []Leaf{
			{
				PkScript:  []byte("alice_vtxo_script"),
				Amount:    5000,
				SignerKey: alicePubKey,
			},
			{
				PkScript:  []byte("bob_vtxo_script"),
				Amount:    3000,
				SignerKey: bobPubKey,
			},
			{
				PkScript:  []byte("charlie_vtxo_script"),
				Amount:    2000,
				SignerKey: charliePubKey,
			},
			{
				PkScript:  []byte("dave_vtxo_script"),
				Amount:    1500,
				SignerKey: davePubKey,
			},
			{
				PkScript:  []byte("eve_vtxo_script"),
				Amount:    1000,
				SignerKey: evePubKey,
			},
		}

		// Build the full VTXO tree
		fullTree, err := BuildVTXOTree(
			input,
			leaves,
			144, // CSV delay
			operatorSweepPubKey,
			operatorSigningPubKey,
			2, // Binary tree (radix 2)
			prevOut,
		)
		require.NoError(t, err)
		require.NotNil(t, fullTree)

		// Verify full tree structure
		require.Equal(t, input, fullTree.Root.Input)
		require.Len(t, fullTree.Root.Children, 2) // Binary tree root has 2 children

		// Extract Alice's path
		t.Run("extract Alice's path", func(t *testing.T) {
			alicePath := fullTree.ExtractPathForCosigner(alicePubKey)
			require.NotNil(t, alicePath)

			// Alice's path should maintain root node info
			require.Equal(t, fullTree.Root.Input, alicePath.Root.Input)
			require.Contains(t, alicePath.Root.CoSigners, operatorSigningPubKey)
			require.Contains(t, alicePath.Root.CoSigners, alicePubKey)

			// Find Alice's leaf by traversing her path
			aliceLeaf := findLeafForClient(alicePath.Root, alicePubKey)
			require.NotNil(t, aliceLeaf, "Alice's leaf not found")
			require.Len(t, aliceLeaf.Outputs, 2) // VTXO + anchor
			require.Equal(t, int64(5000), aliceLeaf.Outputs[0].Value)
			require.Equal(t, []byte("alice_vtxo_script"), aliceLeaf.Outputs[0].PkScript)

			// Alice's path should only include nodes where Alice is a cosigner
			verifyPathContainsOnlyClientNodes(t, alicePath.Root, alicePubKey)
		})

		// Extract Bob's path
		t.Run("extract Bob's path", func(t *testing.T) {
			bobPath := fullTree.ExtractPathForCosigner(bobPubKey)
			require.NotNil(t, bobPath)

			bobLeaf := findLeafForClient(bobPath.Root, bobPubKey)
			require.NotNil(t, bobLeaf, "Bob's leaf not found")
			require.Equal(t, int64(3000), bobLeaf.Outputs[0].Value)
			require.Equal(t, []byte("bob_vtxo_script"), bobLeaf.Outputs[0].PkScript)

			verifyPathContainsOnlyClientNodes(t, bobPath.Root, bobPubKey)
		})

		// Extract Charlie's path
		t.Run("extract Charlie's path", func(t *testing.T) {
			charliePath := fullTree.ExtractPathForCosigner(charliePubKey)
			require.NotNil(t, charliePath)

			charlieLeaf := findLeafForClient(charliePath.Root, charliePubKey)
			require.NotNil(t, charlieLeaf, "Charlie's leaf not found")
			require.Equal(t, int64(2000), charlieLeaf.Outputs[0].Value)
			require.Equal(t, []byte("charlie_vtxo_script"), charlieLeaf.Outputs[0].PkScript)

			verifyPathContainsOnlyClientNodes(t, charliePath.Root, charliePubKey)
		})

		// Extract Dave's path
		t.Run("extract Dave's path", func(t *testing.T) {
			davePath := fullTree.ExtractPathForCosigner(davePubKey)
			require.NotNil(t, davePath)

			daveLeaf := findLeafForClient(davePath.Root, davePubKey)
			require.NotNil(t, daveLeaf, "Dave's leaf not found")
			require.Equal(t, int64(1500), daveLeaf.Outputs[0].Value)
			require.Equal(t, []byte("dave_vtxo_script"), daveLeaf.Outputs[0].PkScript)

			verifyPathContainsOnlyClientNodes(t, davePath.Root, davePubKey)
		})

		// Extract Eve's path
		t.Run("extract Eve's path", func(t *testing.T) {
			evePath := fullTree.ExtractPathForCosigner(evePubKey)
			require.NotNil(t, evePath)

			eveLeaf := findLeafForClient(evePath.Root, evePubKey)
			require.NotNil(t, eveLeaf, "Eve's leaf not found")
			require.Equal(t, int64(1000), eveLeaf.Outputs[0].Value)
			require.Equal(t, []byte("eve_vtxo_script"), eveLeaf.Outputs[0].PkScript)

			verifyPathContainsOnlyClientNodes(t, evePath.Root, evePubKey)
		})

		// Verify that extracted paths are different (each client gets their own subset)
		t.Run("verify paths are client-specific", func(t *testing.T) {
			alicePath := fullTree.ExtractPathForCosigner(alicePubKey)
			bobPath := fullTree.ExtractPathForCosigner(bobPubKey)

			// Alice and Bob should have different path structures
			// (unless they happen to share intermediate nodes, which is possible but unlikely with this distribution)
			aliceLeafCount := countLeaves(alicePath.Root)
			bobLeafCount := countLeaves(bobPath.Root)

			// Each client should have exactly one leaf in their extracted path
			require.Equal(t, 1, aliceLeafCount, "Alice should have exactly one leaf in her path")
			require.Equal(t, 1, bobLeafCount, "Bob should have exactly one leaf in his path")

			// Verify operator can extract the full tree (since operator is in all nodes)
			operatorPath := fullTree.ExtractPathForCosigner(operatorSigningPubKey)
			require.NotNil(t, operatorPath)
			operatorLeafCount := countLeaves(operatorPath.Root)
			require.Equal(t, 5, operatorLeafCount, "Operator should see all 5 leaves")
		})
	})

	t.Run("single client tree", func(t *testing.T) {
		// Test with just one client
		singleLeaf := []Leaf{
			{
				PkScript:  []byte("solo_client_script"),
				Amount:    10000,
				SignerKey: alicePubKey,
			},
		}

		tree, err := BuildVTXOTree(input, singleLeaf, 144, operatorSweepPubKey, operatorSigningPubKey, 2, prevOut)
		require.NoError(t, err)

		// Extract Alice's path (should be the entire tree since she's the only client)
		alicePath := tree.ExtractPathForCosigner(alicePubKey)
		require.NotNil(t, alicePath)

		// Should be identical to the original tree structure for single client
		require.Equal(t, tree.Root.Input, alicePath.Root.Input)
		require.Len(t, alicePath.Root.Children, len(tree.Root.Children))
		require.Equal(t, int64(10000), alicePath.Root.Outputs[0].Value)
	})
}

// findLeafForClient traverses the tree to find the leaf node for a specific client
func findLeafForClient(node *TreeNode, clientKey *btcec.PublicKey) *TreeNode {
	if node == nil {
		return nil
	}

	// If this is a leaf node (no children) and contains the client's key, this is their leaf
	if len(node.Children) == 0 && ContainsCosigner(node.CoSigners, clientKey) {
		return node
	}

	// Recursively search children
	for _, child := range node.Children {
		if leaf := findLeafForClient(child, clientKey); leaf != nil {
			return leaf
		}
	}

	return nil
}

// verifyPathContainsOnlyClientNodes verifies that all nodes in the path contain the client's key
func verifyPathContainsOnlyClientNodes(t *testing.T, node *TreeNode, clientKey *btcec.PublicKey) {
	if node == nil {
		return
	}

	// Every node in the client's path must contain their key
	require.True(t, ContainsCosigner(node.CoSigners, clientKey),
		"Node in client path does not contain client's key")

	// Recursively verify all children
	for _, child := range node.Children {
		verifyPathContainsOnlyClientNodes(t, child, clientKey)
	}
}

// countLeaves counts the number of leaf nodes in a tree
func countLeaves(node *TreeNode) int {
	if node == nil {
		return 0
	}

	if len(node.Children) == 0 {
		return 1 // This is a leaf
	}

	count := 0
	for _, child := range node.Children {
		count += countLeaves(child)
	}
	return count
}

func TestTreeDepth(t *testing.T) {
	// Generate test keys
	operatorSweepKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorSweepPubKey := operatorSweepKey.PubKey()

	operatorSigningKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorSigningPubKey := operatorSigningKey.PubKey()

	user1Key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	user1PubKey := user1Key.PubKey()

	user2Key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	user2PubKey := user2Key.PubKey()

	user3Key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	user3PubKey := user3Key.PubKey()

	input := &wire.OutPoint{
		Hash:  [32]byte{1, 2, 3},
		Index: 0,
	}

	// Mock previous output
	prevOut := &wire.TxOut{
		Value:    10000,
		PkScript: []byte("mock_script"),
	}

	t.Run("nil tree depth", func(t *testing.T) {
		var nilTree *TreeNode
		require.Equal(t, 0, nilTree.Depth())
	})

	t.Run("single leaf depth", func(t *testing.T) {
		leaves := []Leaf{
			{
				PkScript:  []byte("single_leaf"),
				Amount:    1000,
				SignerKey: user1PubKey,
			},
		}

		tree, err := BuildVTXOTree(input, leaves, 144, operatorSweepPubKey, operatorSigningPubKey, 2, prevOut)
		require.NoError(t, err)
		require.Equal(t, 1, tree.Root.Depth())
	})

	t.Run("two leaf depth", func(t *testing.T) {
		leaves := []Leaf{
			{
				PkScript:  []byte("leaf1"),
				Amount:    1000,
				SignerKey: user1PubKey,
			},
			{
				PkScript:  []byte("leaf2"),
				Amount:    2000,
				SignerKey: user2PubKey,
			},
		}

		tree, err := BuildVTXOTree(input, leaves, 144, operatorSweepPubKey, operatorSigningPubKey, 2, prevOut)
		require.NoError(t, err)
		require.Equal(t, 2, tree.Root.Depth()) // Root + leaf level
	})

	t.Run("five leaf depth with binary tree", func(t *testing.T) {
		leaves := []Leaf{
			{PkScript: []byte("leaf1"), Amount: 5000, SignerKey: user1PubKey},
			{PkScript: []byte("leaf2"), Amount: 4000, SignerKey: user2PubKey},
			{PkScript: []byte("leaf3"), Amount: 3000, SignerKey: user3PubKey},
			{PkScript: []byte("leaf4"), Amount: 2000, SignerKey: user1PubKey},
			{PkScript: []byte("leaf5"), Amount: 1000, SignerKey: user2PubKey},
		}

		tree, err := BuildVTXOTree(
			input,
			leaves,
			144,
			operatorSweepPubKey,
			operatorSigningPubKey,
			2, // Binary tree
			prevOut,
		)
		require.NoError(t, err)

		// With 5 leaves and radix 2, we expect a multi-level tree
		// The depth should be at least 3 (root, intermediate nodes, leaves)
		depth := tree.Root.Depth()
		require.GreaterOrEqual(t, depth, 3)
		require.LessOrEqual(t, depth, 4) // Shouldn't be deeper than necessary
	})

	t.Run("depth after path extraction", func(t *testing.T) {
		leaves := []Leaf{
			{PkScript: []byte("alice"), Amount: 5000, SignerKey: user1PubKey},
			{PkScript: []byte("bob"), Amount: 3000, SignerKey: user2PubKey},
			{PkScript: []byte("charlie"), Amount: 2000, SignerKey: user3PubKey},
		}

		fullTree, err := BuildVTXOTree(input, leaves, 144, operatorSweepPubKey, operatorSigningPubKey, 2, prevOut)
		require.NoError(t, err)

		fullDepth := fullTree.Root.Depth()

		// Extract Alice's path and check its depth
		alicePath := fullTree.ExtractPathForCosigner(user1PubKey)
		require.NotNil(t, alicePath)

		aliceDepth := alicePath.Root.Depth()

		// Alice's path depth should be <= full tree depth
		require.LessOrEqual(t, aliceDepth, fullDepth)

		// Alice's path should have at least depth 1 (her leaf)
		require.GreaterOrEqual(t, aliceDepth, 1)
	})

	t.Run("manually constructed tree depth", func(t *testing.T) {
		// Manually construct a tree to test specific depth scenarios
		leaf1 := &TreeNode{
			Input:     &wire.OutPoint{Hash: [32]byte{1}, Index: 0},
			CoSigners: []*btcec.PublicKey{operatorSigningPubKey, user1PubKey},
			Outputs:   []*wire.TxOut{{Value: 1000, PkScript: []byte("leaf1")}},
			Children:  make(map[uint32]*TreeNode),
		}

		leaf2 := &TreeNode{
			Input:     &wire.OutPoint{Hash: [32]byte{2}, Index: 0},
			CoSigners: []*btcec.PublicKey{operatorSigningPubKey, user2PubKey},
			Outputs:   []*wire.TxOut{{Value: 2000, PkScript: []byte("leaf2")}},
			Children:  make(map[uint32]*TreeNode),
		}

		branch := &TreeNode{
			Input:     &wire.OutPoint{Hash: [32]byte{3}, Index: 0},
			CoSigners: []*btcec.PublicKey{operatorSigningPubKey, user1PubKey, user2PubKey},
			Outputs:   []*wire.TxOut{{Value: 1000}, {Value: 2000}},
			Children:  map[uint32]*TreeNode{0: leaf1, 1: leaf2},
		}

		root := &TreeNode{
			Input:     input,
			CoSigners: []*btcec.PublicKey{operatorSigningPubKey, user1PubKey, user2PubKey},
			Outputs:   []*wire.TxOut{{Value: 3000}},
			Children:  map[uint32]*TreeNode{0: branch},
		}

		require.Equal(t, 1, leaf1.Depth())
		require.Equal(t, 1, leaf2.Depth())
		require.Equal(t, 2, branch.Depth())
		require.Equal(t, 3, root.Depth())
	})
}

func TestNumTx(t *testing.T) {
	// Generate test keys
	operatorSweepKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorSweepPubKey := operatorSweepKey.PubKey()

	operatorSigningKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorSigningPubKey := operatorSigningKey.PubKey()

	user1Key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	user1PubKey := user1Key.PubKey()

	user2Key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	user2PubKey := user2Key.PubKey()

	user3Key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	user3PubKey := user3Key.PubKey()

	input := &wire.OutPoint{
		Hash:  [32]byte{1, 2, 3},
		Index: 0,
	}

	// Mock previous output
	prevOut := &wire.TxOut{
		Value:    15000,
		PkScript: []byte("mock_script"),
	}

	t.Run("nil tree transaction count", func(t *testing.T) {
		var nilTree *TreeNode
		require.Equal(t, 0, nilTree.NumTx())
	})

	t.Run("single leaf transaction count", func(t *testing.T) {
		leaves := []Leaf{
			{
				PkScript:  []byte("single_leaf"),
				Amount:    1000,
				SignerKey: user1PubKey,
			},
		}

		tree, err := BuildVTXOTree(input, leaves, 144, operatorSweepPubKey, operatorSigningPubKey, 2, prevOut)
		require.NoError(t, err)
		require.Equal(t, 1, tree.Root.NumTx()) // Just one transaction for single leaf
	})

	t.Run("two leaf transaction count", func(t *testing.T) {
		leaves := []Leaf{
			{
				PkScript:  []byte("leaf1"),
				Amount:    1000,
				SignerKey: user1PubKey,
			},
			{
				PkScript:  []byte("leaf2"),
				Amount:    2000,
				SignerKey: user2PubKey,
			},
		}

		tree, err := BuildVTXOTree(input, leaves, 144, operatorSweepPubKey, operatorSigningPubKey, 2, prevOut)
		require.NoError(t, err)
		require.Equal(t, 3, tree.Root.NumTx()) // Root + 2 leaf transactions
	})

	t.Run("five leaf transaction count", func(t *testing.T) {
		leaves := []Leaf{
			{PkScript: []byte("leaf1"), Amount: 5000, SignerKey: user1PubKey},
			{PkScript: []byte("leaf2"), Amount: 4000, SignerKey: user2PubKey},
			{PkScript: []byte("leaf3"), Amount: 3000, SignerKey: user3PubKey},
			{PkScript: []byte("leaf4"), Amount: 2000, SignerKey: user1PubKey},
			{PkScript: []byte("leaf5"), Amount: 1000, SignerKey: user2PubKey},
		}

		tree, err := BuildVTXOTree(
			input,
			leaves,
			144,
			operatorSweepPubKey,
			operatorSigningPubKey,
			2, // Binary tree
			prevOut,
		)
		require.NoError(t, err)

		// With 5 leaves and binary tree, we expect:
		// - 5 leaf transactions
		// - Some number of intermediate/branch transactions
		// Total should be more than 5
		numTx := tree.Root.NumTx()
		require.GreaterOrEqual(t, numTx, 5) // At least the 5 leaves
		require.LessOrEqual(t, numTx, 9)    // Shouldn't need too many intermediate nodes
	})

	t.Run("transaction count after path extraction", func(t *testing.T) {
		leaves := []Leaf{
			{PkScript: []byte("alice"), Amount: 5000, SignerKey: user1PubKey},
			{PkScript: []byte("bob"), Amount: 3000, SignerKey: user2PubKey},
			{PkScript: []byte("charlie"), Amount: 2000, SignerKey: user3PubKey},
		}

		fullTree, err := BuildVTXOTree(input, leaves, 144, operatorSweepPubKey, operatorSigningPubKey, 2, prevOut)
		require.NoError(t, err)

		fullNumTx := fullTree.Root.NumTx()

		// Extract Alice's path and check transaction count
		alicePath := fullTree.ExtractPathForCosigner(user1PubKey)
		require.NotNil(t, alicePath)

		aliceNumTx := alicePath.Root.NumTx()

		// Alice's path should have fewer or equal transactions than the full tree
		require.LessOrEqual(t, aliceNumTx, fullNumTx)

		// Alice's path should have at least 1 transaction (her leaf)
		require.GreaterOrEqual(t, aliceNumTx, 1)

		// Extract Bob's path and verify it's different from Alice's
		bobPath := fullTree.ExtractPathForCosigner(user2PubKey)
		require.NotNil(t, bobPath)
		bobNumTx := bobPath.Root.NumTx()

		require.GreaterOrEqual(t, bobNumTx, 1)
		require.LessOrEqual(t, bobNumTx, fullNumTx)

		// Extract operator path (should see all transactions)
		operatorPath := fullTree.ExtractPathForCosigner(operatorSigningPubKey)
		require.NotNil(t, operatorPath)
		operatorNumTx := operatorPath.Root.NumTx()
		require.Equal(t, fullNumTx, operatorNumTx) // Operator sees full tree
	})

	t.Run("manually constructed tree transaction count", func(t *testing.T) {
		// Manually construct a tree to test specific scenarios
		leaf1 := &TreeNode{
			Input:     &wire.OutPoint{Hash: [32]byte{1}, Index: 0},
			CoSigners: []*btcec.PublicKey{operatorSigningPubKey, user1PubKey},
			Outputs:   []*wire.TxOut{{Value: 1000, PkScript: []byte("leaf1")}},
			Children:  make(map[uint32]*TreeNode),
		}

		leaf2 := &TreeNode{
			Input:     &wire.OutPoint{Hash: [32]byte{2}, Index: 0},
			CoSigners: []*btcec.PublicKey{operatorSigningPubKey, user2PubKey},
			Outputs:   []*wire.TxOut{{Value: 2000, PkScript: []byte("leaf2")}},
			Children:  make(map[uint32]*TreeNode),
		}

		branch := &TreeNode{
			Input:     &wire.OutPoint{Hash: [32]byte{3}, Index: 0},
			CoSigners: []*btcec.PublicKey{operatorSigningPubKey, user1PubKey, user2PubKey},
			Outputs:   []*wire.TxOut{{Value: 1000}, {Value: 2000}},
			Children:  map[uint32]*TreeNode{0: leaf1, 1: leaf2},
		}

		root := &TreeNode{
			Input:     input,
			CoSigners: []*btcec.PublicKey{operatorSigningPubKey, user1PubKey, user2PubKey},
			Outputs:   []*wire.TxOut{{Value: 3000}},
			Children:  map[uint32]*TreeNode{0: branch},
		}

		require.Equal(t, 1, leaf1.NumTx())
		require.Equal(t, 1, leaf2.NumTx())
		require.Equal(t, 3, branch.NumTx()) // branch + 2 leaves
		require.Equal(t, 4, root.NumTx())   // root + branch + 2 leaves
	})

	t.Run("relationship between NumTx and countLeaves", func(t *testing.T) {
		leaves := []Leaf{
			{PkScript: []byte("leaf1"), Amount: 3000, SignerKey: user1PubKey},
			{PkScript: []byte("leaf2"), Amount: 2000, SignerKey: user2PubKey},
			{PkScript: []byte("leaf3"), Amount: 1000, SignerKey: user3PubKey},
		}

		tree, err := BuildVTXOTree(input, leaves, 144, operatorSweepPubKey, operatorSigningPubKey, 2, prevOut)
		require.NoError(t, err)

		numTx := tree.Root.NumTx()
		numLeaves := countLeaves(tree.Root)

		// Number of transactions should always be >= number of leaves
		require.GreaterOrEqual(t, numTx, numLeaves)

		// For this specific case with 3 leaves, we expect some intermediate transactions
		require.Equal(t, 3, numLeaves)
		require.Greater(t, numTx, numLeaves)
	})
}

func TestTreeVerify(t *testing.T) {
	// Generate test keys
	operatorSweepKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorSweepPubKey := operatorSweepKey.PubKey()

	operatorSigningKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorSigningPubKey := operatorSigningKey.PubKey()

	user1Key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	user1PubKey := user1Key.PubKey()

	user2Key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	user2PubKey := user2Key.PubKey()

	user3Key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	user3PubKey := user3Key.PubKey()

	input := &wire.OutPoint{
		Hash:  [32]byte{1, 2, 3, 4, 5},
		Index: 0,
	}

	// Mock previous output
	prevOut := &wire.TxOut{
		Value:    12500,
		PkScript: []byte("mock_script"),
	}

	t.Run("nil tree verification", func(t *testing.T) {
		var nilTree *TreeNode
		err := nilTree.Verify()
		require.NoError(t, err) // nil tree should verify successfully
	})

	t.Run("single leaf tree verification", func(t *testing.T) {
		leaves := []Leaf{
			{
				PkScript:  []byte("single_leaf"),
				Amount:    1000,
				SignerKey: user1PubKey,
			},
		}

		tree, err := BuildVTXOTree(input, leaves, 144, operatorSweepPubKey, operatorSigningPubKey, 2, prevOut)
		require.NoError(t, err)

		// Single leaf tree should verify (no children to check)
		err = tree.Verify()
		require.NoError(t, err)
	})

	t.Run("multi-level tree verification", func(t *testing.T) {
		leaves := []Leaf{
			{PkScript: []byte("alice"), Amount: 5000, SignerKey: user1PubKey},
			{PkScript: []byte("bob"), Amount: 3000, SignerKey: user2PubKey},
			{PkScript: []byte("charlie"), Amount: 2000, SignerKey: user3PubKey},
			{PkScript: []byte("dave"), Amount: 1500, SignerKey: user1PubKey},
			{PkScript: []byte("eve"), Amount: 1000, SignerKey: user2PubKey},
		}

		tree, err := BuildVTXOTree(
			input,
			leaves,
			144,
			operatorSweepPubKey,
			operatorSigningPubKey,
			2, // Binary tree
			prevOut,
		)
		require.NoError(t, err)

		// Multi-level tree should verify after our fix
		err = tree.Verify()
		require.NoError(t, err, "Tree verification should pass after fixing the input outpoint bug")
	})

	t.Run("extracted path verification", func(t *testing.T) {
		leaves := []Leaf{
			{PkScript: []byte("alice"), Amount: 4000, SignerKey: user1PubKey},
			{PkScript: []byte("bob"), Amount: 3000, SignerKey: user2PubKey},
			{PkScript: []byte("charlie"), Amount: 2000, SignerKey: user3PubKey},
		}

		fullTree, err := BuildVTXOTree(input, leaves, 144, operatorSweepPubKey, operatorSigningPubKey, 2, prevOut)
		require.NoError(t, err)

		// Full tree should verify
		err = fullTree.Verify()
		require.NoError(t, err)

		// Extracted paths should also verify
		alicePath := fullTree.ExtractPathForCosigner(user1PubKey)
		require.NotNil(t, alicePath)
		err = alicePath.Root.Verify()
		require.NoError(t, err, "Alice's extracted path should verify")

		bobPath := fullTree.ExtractPathForCosigner(user2PubKey)
		require.NotNil(t, bobPath)
		err = bobPath.Root.Verify()
		require.NoError(t, err, "Bob's extracted path should verify")
	})

	t.Run("broken tree verification fails", func(t *testing.T) {
		// Manually create a tree with incorrect input references to test failure case
		leaf := &TreeNode{
			Input:     &wire.OutPoint{Hash: [32]byte{9, 9, 9}, Index: 0}, // Wrong hash
			CoSigners: []*btcec.PublicKey{operatorSigningPubKey, user1PubKey},
			Outputs:   []*wire.TxOut{{Value: 1000, PkScript: []byte("leaf")}},
			Children:  make(map[uint32]*TreeNode),
		}

		root := &TreeNode{
			Input:     input,
			CoSigners: []*btcec.PublicKey{operatorSigningPubKey, user1PubKey},
			Outputs:   []*wire.TxOut{{Value: 1000}},
			Children:  map[uint32]*TreeNode{0: leaf},
		}

		// This should fail verification because leaf input doesn't match root tx hash
		err := root.Verify()
		require.Error(t, err)
		require.Contains(t, err.Error(), "incorrect input")
	})

	t.Run("verify output index bounds", func(t *testing.T) {
		// Create a tree where child references non-existent output index
		leaf := &TreeNode{
			Input:     &wire.OutPoint{Hash: [32]byte{1, 2, 3}, Index: 5}, // Index 5 doesn't exist
			CoSigners: []*btcec.PublicKey{operatorSigningPubKey, user1PubKey},
			Outputs:   []*wire.TxOut{{Value: 1000, PkScript: []byte("leaf")}},
			Children:  make(map[uint32]*TreeNode),
		}

		root := &TreeNode{
			Input:     input,
			CoSigners: []*btcec.PublicKey{operatorSigningPubKey, user1PubKey},
			Outputs:   []*wire.TxOut{{Value: 1000}},  // Only has output index 0
			Children:  map[uint32]*TreeNode{5: leaf}, // But child is at index 5
		}

		// This should fail verification due to out-of-bounds output index
		err := root.Verify()
		require.Error(t, err)
		require.Contains(t, err.Error(), "non-existent output index")
	})

	t.Run("comprehensive verification", func(t *testing.T) {
		// Test verification on different tree sizes and structures
		testCases := []struct {
			name      string
			numLeaves int
			radix     int
		}{
			{"binary tree 3 leaves", 3, 2},
			{"binary tree 7 leaves", 7, 2},
			{"ternary tree 4 leaves", 4, 3},
			{"quaternary tree 8 leaves", 8, 4},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				leaves := make([]Leaf, tc.numLeaves)
				for i := 0; i < tc.numLeaves; i++ {
					leaves[i] = Leaf{
						PkScript:  []byte(fmt.Sprintf("leaf_%d", i)),
						Amount:    int64(1000 + i*500),
						SignerKey: []*btcec.PublicKey{user1PubKey, user2PubKey, user3PubKey}[i%3],
					}
				}

				tree, err := BuildVTXOTree(input, leaves, 144, operatorSweepPubKey, operatorSigningPubKey, tc.radix, prevOut)
				require.NoError(t, err)

				// Every tree structure should verify correctly
				err = tree.Verify()
				require.NoError(t, err, "Tree with %d leaves and radix %d should verify", tc.numLeaves, tc.radix)
			})
		}
	})
}

func TestForEach(t *testing.T) {
	// Generate test keys
	operatorSweepKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorSweepPubKey := operatorSweepKey.PubKey()

	operatorSigningKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorSigningPubKey := operatorSigningKey.PubKey()

	user1Key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	user1PubKey := user1Key.PubKey()

	user2Key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	user2PubKey := user2Key.PubKey()

	user3Key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	user3PubKey := user3Key.PubKey()

	input := &wire.OutPoint{
		Hash:  [32]byte{1, 2, 3, 4, 5},
		Index: 0,
	}

	// Mock previous output
	prevOut := &wire.TxOut{
		Value:    10000,
		PkScript: []byte("mock_script"),
	}

	t.Run("nil tree ForEach", func(t *testing.T) {
		var nilTree *TreeNode
		callCount := 0
		err := nilTree.ForEach(func(*TreeNode) error {
			callCount++
			return nil
		})
		require.NoError(t, err)
		require.Equal(t, 0, callCount)
	})

	t.Run("single node ForEach", func(t *testing.T) {
		leaves := []Leaf{
			{
				PkScript:  []byte("single_leaf"),
				Amount:    1000,
				SignerKey: user1PubKey,
			},
		}

		tree, err := BuildVTXOTree(input, leaves, 144, operatorSweepPubKey, operatorSigningPubKey, 2, prevOut)
		require.NoError(t, err)

		var visitedNodes []*TreeNode
		err = tree.ForEach(func(node *TreeNode) error {
			visitedNodes = append(visitedNodes, node)
			return nil
		})
		require.NoError(t, err)
		require.Len(t, visitedNodes, 1) // Should visit root (which is also a leaf)
		require.Equal(t, tree.Root, visitedNodes[0])
	})

	t.Run("multi-node ForEach", func(t *testing.T) {
		leaves := []Leaf{
			{PkScript: []byte("leaf1"), Amount: 3000, SignerKey: user1PubKey},
			{PkScript: []byte("leaf2"), Amount: 2000, SignerKey: user2PubKey},
			{PkScript: []byte("leaf3"), Amount: 1000, SignerKey: user3PubKey},
		}

		tree, err := BuildVTXOTree(input, leaves, 144, operatorSweepPubKey, operatorSigningPubKey, 2, prevOut)
		require.NoError(t, err)

		var visitedNodes []*TreeNode
		err = tree.ForEach(func(node *TreeNode) error {
			visitedNodes = append(visitedNodes, node)
			return nil
		})
		require.NoError(t, err)

		// Should visit all nodes in the tree
		expectedCount := tree.Root.NumTx()
		require.Len(t, visitedNodes, expectedCount)

		// First node should be the root
		require.Equal(t, tree.Root, visitedNodes[0])
	})

	t.Run("ForEach error handling", func(t *testing.T) {
		leaves := []Leaf{
			{PkScript: []byte("leaf1"), Amount: 2000, SignerKey: user1PubKey},
			{PkScript: []byte("leaf2"), Amount: 1000, SignerKey: user2PubKey},
		}

		tree, err := BuildVTXOTree(input, leaves, 144, operatorSweepPubKey, operatorSigningPubKey, 2, prevOut)
		require.NoError(t, err)

		callCount := 0
		targetError := fmt.Errorf("test error")

		err = tree.ForEach(func(node *TreeNode) error {
			callCount++
			if callCount == 2 { // Error on second node
				return targetError
			}
			return nil
		})

		require.Error(t, err)
		require.Equal(t, targetError, err)
		require.Equal(t, 2, callCount) // Should have stopped after error
	})

	t.Run("ForEach collects transaction hashes", func(t *testing.T) {
		leaves := []Leaf{
			{PkScript: []byte("alice"), Amount: 4000, SignerKey: user1PubKey},
			{PkScript: []byte("bob"), Amount: 3000, SignerKey: user2PubKey},
		}

		tree, err := BuildVTXOTree(input, leaves, 144, operatorSweepPubKey, operatorSigningPubKey, 2, prevOut)
		require.NoError(t, err)

		var txHashes []string
		err = tree.ForEach(func(node *TreeNode) error {
			tx, err := node.ToTx()
			if err != nil {
				return err
			}
			txHash := tx.TxHash()
			txHashes = append(txHashes, txHash.String())
			return nil
		})
		require.NoError(t, err)

		// Should have collected transaction hashes from all nodes
		require.Len(t, txHashes, tree.Root.NumTx())

		// All hashes should be unique
		uniqueHashes := make(map[string]bool)
		for _, hash := range txHashes {
			require.False(t, uniqueHashes[hash], "Duplicate transaction hash found")
			uniqueHashes[hash] = true
		}
	})
}

func TestForEachLeaf(t *testing.T) {
	// Generate test keys
	operatorSweepKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorSweepPubKey := operatorSweepKey.PubKey()

	operatorSigningKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorSigningPubKey := operatorSigningKey.PubKey()

	user1Key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	user1PubKey := user1Key.PubKey()

	user2Key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	user2PubKey := user2Key.PubKey()

	user3Key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	user3PubKey := user3Key.PubKey()

	input := &wire.OutPoint{
		Hash:  [32]byte{1, 2, 3, 4, 5},
		Index: 0,
	}

	// Mock previous output
	prevOut := &wire.TxOut{
		Value:    10000,
		PkScript: []byte("mock_script"),
	}

	t.Run("nil tree ForEachLeaf", func(t *testing.T) {
		var nilTree *TreeNode
		callCount := 0
		err := nilTree.ForEachLeaf(func(*TreeNode) error {
			callCount++
			return nil
		})
		require.NoError(t, err)
		require.Equal(t, 0, callCount)
	})

	t.Run("single leaf ForEachLeaf", func(t *testing.T) {
		leaves := []Leaf{
			{
				PkScript:  []byte("single_leaf"),
				Amount:    1000,
				SignerKey: user1PubKey,
			},
		}

		tree, err := BuildVTXOTree(input, leaves, 144, operatorSweepPubKey, operatorSigningPubKey, 2, prevOut)
		require.NoError(t, err)

		var visitedLeaves []*TreeNode
		err = tree.ForEachLeaf(func(node *TreeNode) error {
			visitedLeaves = append(visitedLeaves, node)
			return nil
		})
		require.NoError(t, err)
		require.Len(t, visitedLeaves, 1) // Should visit the single leaf
	})

	t.Run("multi-leaf ForEachLeaf", func(t *testing.T) {
		leaves := []Leaf{
			{PkScript: []byte("alice"), Amount: 4000, SignerKey: user1PubKey},
			{PkScript: []byte("bob"), Amount: 3000, SignerKey: user2PubKey},
			{PkScript: []byte("charlie"), Amount: 2000, SignerKey: user3PubKey},
			{PkScript: []byte("dave"), Amount: 1000, SignerKey: user1PubKey},
		}

		tree, err := BuildVTXOTree(input, leaves, 144, operatorSweepPubKey, operatorSigningPubKey, 2, prevOut)
		require.NoError(t, err)

		var visitedLeaves []*TreeNode
		err = tree.ForEachLeaf(func(node *TreeNode) error {
			visitedLeaves = append(visitedLeaves, node)
			// Verify this is actually a leaf
			require.Len(t, node.Children, 0, "ForEachLeaf should only visit leaf nodes")
			return nil
		})
		require.NoError(t, err)

		// Should visit exactly the number of leaves we created
		require.Len(t, visitedLeaves, 4)

		// Verify total amounts match
		var totalAmount int64
		for _, leaf := range visitedLeaves {
			for _, output := range leaf.Outputs {
				if output.Value > 0 { // Skip anchor outputs
					totalAmount += output.Value
				}
			}
		}
		expectedTotal := int64(4000 + 3000 + 2000 + 1000)
		require.Equal(t, expectedTotal, totalAmount)
	})

	t.Run("ForEachLeaf validates cosigners", func(t *testing.T) {
		leaves := []Leaf{
			{PkScript: []byte("alice"), Amount: 2000, SignerKey: user1PubKey},
			{PkScript: []byte("bob"), Amount: 1000, SignerKey: user2PubKey},
		}

		tree, err := BuildVTXOTree(input, leaves, 144, operatorSweepPubKey, operatorSigningPubKey, 2, prevOut)
		require.NoError(t, err)

		err = tree.ForEachLeaf(func(node *TreeNode) error {
			// Every leaf should have exactly 2 cosigners: operator + user
			require.Len(t, node.CoSigners, 2, "Each leaf should have exactly 2 cosigners")

			// Operator should be in every leaf
			require.True(t, ContainsCosigner(node.CoSigners, operatorSigningPubKey),
				"Operator should be a cosigner in every leaf")

			return nil
		})
		require.NoError(t, err)
	})

	t.Run("ForEachLeaf with early termination", func(t *testing.T) {
		leaves := []Leaf{
			{PkScript: []byte("leaf1"), Amount: 3000, SignerKey: user1PubKey},
			{PkScript: []byte("leaf2"), Amount: 2000, SignerKey: user2PubKey},
			{PkScript: []byte("leaf3"), Amount: 1000, SignerKey: user3PubKey},
		}

		tree, err := BuildVTXOTree(input, leaves, 144, operatorSweepPubKey, operatorSigningPubKey, 2, prevOut)
		require.NoError(t, err)

		callCount := 0
		targetError := fmt.Errorf("early termination")

		err = tree.ForEachLeaf(func(node *TreeNode) error {
			callCount++
			if callCount == 2 { // Stop after second leaf
				return targetError
			}
			return nil
		})

		require.Error(t, err)
		require.Equal(t, targetError, err)
		require.Equal(t, 2, callCount) // Should have stopped after error
	})
}

func TestNewPrevOutputFetcher(t *testing.T) {
	// Generate test keys
	operatorSweepKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorSweepPubKey := operatorSweepKey.PubKey()

	operatorSigningKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorSigningPubKey := operatorSigningKey.PubKey()

	user1Key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	user1PubKey := user1Key.PubKey()

	user2Key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	user2PubKey := user2Key.PubKey()

	user3Key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	user3PubKey := user3Key.PubKey()

	// Create initial previous output (that the root will spend)
	initialPrevOut := &wire.TxOut{
		Value:    10000000, // 0.1 BTC
		PkScript: []byte("initial_commitment_script"),
	}

	input := &wire.OutPoint{
		Hash:  [32]byte{1, 2, 3, 4, 5},
		Index: 0,
	}

	t.Run("single leaf tree", func(t *testing.T) {
		leaves := []Leaf{
			{
				PkScript:  []byte("user1_vtxo_script"),
				Amount:    5000000, // 0.05 BTC
				SignerKey: user1PubKey,
			},
		}

		tree, err := BuildVTXOTree(input, leaves, 144, operatorSweepPubKey, operatorSigningPubKey, 2, initialPrevOut)
		require.NoError(t, err)

		fetcher, err := tree.PrevOutputFetcher()
		require.NoError(t, err)
		require.NotNil(t, fetcher)

		// Test that we can fetch the initial previous output
		fetchedInitial := fetcher.FetchPrevOutput(*input)
		require.NotNil(t, fetchedInitial)
		require.Equal(t, initialPrevOut.Value, fetchedInitial.Value)
		require.Equal(t, initialPrevOut.PkScript, fetchedInitial.PkScript)

		// Test that we can fetch outputs from the tree's transaction
		rootTxHash, err := tree.Root.TXID()
		require.NoError(t, err)

		// Fetch the VTXO output (index 0)
		vtxoOutpoint := wire.OutPoint{Hash: rootTxHash, Index: 0}
		fetchedVTXO := fetcher.FetchPrevOutput(vtxoOutpoint)
		require.NotNil(t, fetchedVTXO)
		require.Equal(t, int64(5000000), fetchedVTXO.Value)
		require.Equal(t, []byte("user1_vtxo_script"), fetchedVTXO.PkScript)

		// Fetch the anchor output (index 1)
		anchorOutpoint := wire.OutPoint{Hash: rootTxHash, Index: 1}
		fetchedAnchor := fetcher.FetchPrevOutput(anchorOutpoint)
		require.NotNil(t, fetchedAnchor)
		require.Equal(t, int64(0), fetchedAnchor.Value) // Anchor has 0 value

		// Test non-existent output returns nil
		nonExistentOutpoint := wire.OutPoint{Hash: [32]byte{9, 9, 9}, Index: 0}
		fetchedNonExistent := fetcher.FetchPrevOutput(nonExistentOutpoint)
		require.Nil(t, fetchedNonExistent)
	})

	t.Run("multi-leaf tree with all outputs", func(t *testing.T) {
		leaves := []Leaf{
			{PkScript: []byte("alice_script"), Amount: 3000000, SignerKey: user1PubKey},
			{PkScript: []byte("bob_script"), Amount: 2000000, SignerKey: user2PubKey},
			{PkScript: []byte("charlie_script"), Amount: 1000000, SignerKey: user3PubKey},
		}

		tree, err := BuildVTXOTree(input, leaves, 144, operatorSweepPubKey, operatorSigningPubKey, 2, initialPrevOut)
		require.NoError(t, err)

		fetcher, err := tree.PrevOutputFetcher()
		require.NoError(t, err)

		// Verify we can fetch initial output
		fetchedInitial := fetcher.FetchPrevOutput(*input)
		require.Equal(t, initialPrevOut, fetchedInitial)

		// Count total outputs available through fetcher
		outputCount := 0
		var allOutpoints []wire.OutPoint

		// Walk tree and try to fetch each output
		err = tree.ForEach(func(node *TreeNode) error {
			txHash, err := node.TXID()
			if err != nil {
				return err
			}

			for i := range node.Outputs {
				outpoint := wire.OutPoint{Hash: txHash, Index: uint32(i)}
				allOutpoints = append(allOutpoints, outpoint)

				fetchedOutput := fetcher.FetchPrevOutput(outpoint)
				require.NotNil(t, fetchedOutput, "Should be able to fetch output at %s:%d", txHash, i)
				outputCount++
			}
			return nil
		})
		require.NoError(t, err)

		// Should have fetched outputs from all transactions in tree
		require.GreaterOrEqual(t, outputCount, tree.Root.NumTx()) // At least one output per transaction
		t.Logf("Successfully fetched %d outputs from tree with %d transactions", outputCount, tree.Root.NumTx())

		// Verify specific leaf outputs
		var leafOutputs []wire.OutPoint
		err = tree.ForEachLeaf(func(node *TreeNode) error {
			txHash, err := node.TXID()
			if err != nil {
				return err
			}

			// Check VTXO output (index 0)
			vtxoOutpoint := wire.OutPoint{Hash: txHash, Index: 0}
			leafOutputs = append(leafOutputs, vtxoOutpoint)

			vtxoOutput := fetcher.FetchPrevOutput(vtxoOutpoint)
			require.NotNil(t, vtxoOutput)
			require.Greater(t, vtxoOutput.Value, int64(0), "VTXO output should have positive value")

			return nil
		})
		require.NoError(t, err)
		require.Len(t, leafOutputs, 3, "Should have found 3 leaf outputs")
	})
}

func TestGetLeafNodes(t *testing.T) {
	// Generate test keys
	operatorSweepKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorSweepPubKey := operatorSweepKey.PubKey()

	operatorSigningKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorSigningPubKey := operatorSigningKey.PubKey()

	user1Key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	user1PubKey := user1Key.PubKey()

	user2Key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	user2PubKey := user2Key.PubKey()

	user3Key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	user3PubKey := user3Key.PubKey()

	input := &wire.OutPoint{
		Hash:  [32]byte{1, 2, 3, 4, 5},
		Index: 0,
	}

	prevOut := &wire.TxOut{
		Value:    15000,
		PkScript: []byte("mock_script"),
	}

	t.Run("nil tree GetLeafNodes", func(t *testing.T) {
		var nilTree *TreeNode
		leaves, err := nilTree.GetLeafNodes()
		require.NoError(t, err)
		require.Nil(t, leaves)
	})

	t.Run("single leaf tree", func(t *testing.T) {
		leaves := []Leaf{
			{
				PkScript:  []byte("single_leaf"),
				Amount:    1000,
				SignerKey: user1PubKey,
			},
		}

		tree, err := BuildVTXOTree(input, leaves, 144, operatorSweepPubKey, operatorSigningPubKey, 2, prevOut)
		require.NoError(t, err)

		leafNodes, err := tree.GetLeafNodes()
		require.NoError(t, err)
		require.Len(t, leafNodes, 1)

		// Should be the root itself (which is also a leaf in single-leaf tree)
		require.Equal(t, tree.Root, leafNodes[0])
		require.Len(t, leafNodes[0].Children, 0) // Should have no children
		require.Equal(t, int64(1000), leafNodes[0].Outputs[0].Value)
	})

	t.Run("multi-leaf tree", func(t *testing.T) {
		leaves := []Leaf{
			{PkScript: []byte("alice"), Amount: 5000, SignerKey: user1PubKey},
			{PkScript: []byte("bob"), Amount: 3000, SignerKey: user2PubKey},
			{PkScript: []byte("charlie"), Amount: 2000, SignerKey: user3PubKey},
		}

		tree, err := BuildVTXOTree(input, leaves, 144, operatorSweepPubKey, operatorSigningPubKey, 2, prevOut)
		require.NoError(t, err)

		leafNodes, err := tree.GetLeafNodes()
		require.NoError(t, err)
		require.Len(t, leafNodes, 3) // Should return exactly 3 leaf nodes

		// Verify all returned nodes are actually leaf nodes
		for i, leaf := range leafNodes {
			require.Len(t, leaf.Children, 0, "Node %d should be a leaf (no children)", i)
			require.Greater(t, leaf.Outputs[0].Value, int64(0), "Leaf %d should have positive VTXO value", i)
		}

		// Verify total amounts match expected
		var totalAmount int64
		expectedScripts := map[string]int64{
			string([]byte("alice")):   5000,
			string([]byte("bob")):     3000,
			string([]byte("charlie")): 2000,
		}

		for _, leaf := range leafNodes {
			vtxoOutput := leaf.Outputs[0] // First output should be the VTXO
			totalAmount += vtxoOutput.Value

			// Check that this is one of our expected scripts/amounts
			scriptStr := string(vtxoOutput.PkScript)
			expectedAmount, found := expectedScripts[scriptStr]
			require.True(t, found, "Unexpected script in leaf: %s", scriptStr)
			require.Equal(t, expectedAmount, vtxoOutput.Value)
		}

		require.Equal(t, int64(10000), totalAmount) // 5000 + 3000 + 2000
	})

	t.Run("tree method delegates to root", func(t *testing.T) {
		leaves := []Leaf{
			{PkScript: []byte("test1"), Amount: 2000, SignerKey: user1PubKey},
			{PkScript: []byte("test2"), Amount: 1000, SignerKey: user2PubKey},
		}

		tree, err := BuildVTXOTree(input, leaves, 144, operatorSweepPubKey, operatorSigningPubKey, 2, prevOut)
		require.NoError(t, err)

		// Test both Tree and TreeNode methods return same results
		treeLeaves, err := tree.GetLeafNodes()
		require.NoError(t, err)

		rootLeaves, err := tree.Root.GetLeafNodes()
		require.NoError(t, err)

		require.Equal(t, len(treeLeaves), len(rootLeaves))
		require.Equal(t, treeLeaves, rootLeaves) // Should be identical
	})

	t.Run("compare with ForEachLeaf", func(t *testing.T) {
		leaves := []Leaf{
			{PkScript: []byte("leaf1"), Amount: 4000, SignerKey: user1PubKey},
			{PkScript: []byte("leaf2"), Amount: 3000, SignerKey: user2PubKey},
			{PkScript: []byte("leaf3"), Amount: 2000, SignerKey: user3PubKey},
			{PkScript: []byte("leaf4"), Amount: 1000, SignerKey: user1PubKey},
		}

		tree, err := BuildVTXOTree(input, leaves, 144, operatorSweepPubKey, operatorSigningPubKey, 2, prevOut)
		require.NoError(t, err)

		// Get leaves using GetLeafNodes
		leafNodes, err := tree.GetLeafNodes()
		require.NoError(t, err)

		// Get leaves using ForEachLeaf
		var forEachLeaves []*TreeNode
		err = tree.ForEachLeaf(func(node *TreeNode) error {
			forEachLeaves = append(forEachLeaves, node)
			return nil
		})
		require.NoError(t, err)

		// Should return the same leaves
		require.Equal(t, len(leafNodes), len(forEachLeaves))
		require.Equal(t, 4, len(leafNodes))

		// Convert to maps for comparison (order might differ)
		getLeafMap := make(map[string]*TreeNode)
		forEachMap := make(map[string]*TreeNode)

		for _, leaf := range leafNodes {
			txHash, err := leaf.TXID()
			require.NoError(t, err)
			getLeafMap[txHash.String()] = leaf
		}

		for _, leaf := range forEachLeaves {
			txHash, err := leaf.TXID()
			require.NoError(t, err)
			forEachMap[txHash.String()] = leaf
		}

		// Maps should be identical
		require.Equal(t, getLeafMap, forEachMap)
	})

	t.Run("extracted path leaf nodes", func(t *testing.T) {
		leaves := []Leaf{
			{PkScript: []byte("alice"), Amount: 3000, SignerKey: user1PubKey},
			{PkScript: []byte("bob"), Amount: 2000, SignerKey: user2PubKey},
			{PkScript: []byte("charlie"), Amount: 1000, SignerKey: user3PubKey},
		}

		fullTree, err := BuildVTXOTree(input, leaves, 144, operatorSweepPubKey, operatorSigningPubKey, 2, prevOut)
		require.NoError(t, err)

		// Extract Alice's path
		alicePath := fullTree.ExtractPathForCosigner(user1PubKey)
		require.NotNil(t, alicePath)

		// Get leaf nodes from Alice's path
		aliceLeaves, err := alicePath.GetLeafNodes()
		require.NoError(t, err)
		require.Len(t, aliceLeaves, 1) // Alice should have exactly one leaf

		// Verify it's Alice's leaf
		aliceLeaf := aliceLeaves[0]
		require.True(t, ContainsCosigner(aliceLeaf.CoSigners, user1PubKey))
		require.Equal(t, int64(3000), aliceLeaf.Outputs[0].Value)
		require.Equal(t, []byte("alice"), aliceLeaf.Outputs[0].PkScript)

		// Extract operator's path (should see all leaves)
		operatorPath := fullTree.ExtractPathForCosigner(operatorSigningPubKey)
		require.NotNil(t, operatorPath)

		operatorLeaves, err := operatorPath.GetLeafNodes()
		require.NoError(t, err)
		require.Len(t, operatorLeaves, 3) // Operator should see all 3 leaves
	})
}

func TestGetLeafForCosigner(t *testing.T) {
	// Generate test keys
	operatorSweepKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorSweepPubKey := operatorSweepKey.PubKey()

	operatorSigningKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorSigningPubKey := operatorSigningKey.PubKey()

	user1Key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	user1PubKey := user1Key.PubKey()

	user2Key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	user2PubKey := user2Key.PubKey()

	user3Key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	user3PubKey := user3Key.PubKey()

	input := &wire.OutPoint{
		Hash:  [32]byte{1, 2, 3, 4, 5},
		Index: 0,
	}

	prevOut := &wire.TxOut{
		Value:    12000,
		PkScript: []byte("mock_script"),
	}

	t.Run("nil tree GetLeafForCosigner", func(t *testing.T) {
		var nilTree *TreeNode
		leaf, err := nilTree.GetLeafForCosigner(user1PubKey)
		require.NoError(t, err)
		require.Nil(t, leaf)
	})

	t.Run("single leaf tree", func(t *testing.T) {
		leaves := []Leaf{
			{
				PkScript:  []byte("alice_script"),
				Amount:    5000,
				SignerKey: user1PubKey,
			},
		}

		tree, err := BuildVTXOTree(input, leaves, 144, operatorSweepPubKey, operatorSigningPubKey, 2, prevOut)
		require.NoError(t, err)

		// Find Alice's leaf
		aliceLeaf, err := tree.GetLeafForCosigner(user1PubKey)
		require.NoError(t, err)
		require.NotNil(t, aliceLeaf)
		require.Equal(t, tree.Root, aliceLeaf) // Root is the leaf in single-leaf tree
		require.True(t, ContainsCosigner(aliceLeaf.CoSigners, user1PubKey))
		require.Equal(t, int64(5000), aliceLeaf.Outputs[0].Value)

		// Operator should also find the same leaf
		operatorLeaf, err := tree.GetLeafForCosigner(operatorSigningPubKey)
		require.NoError(t, err)
		require.Equal(t, aliceLeaf, operatorLeaf)

		// Non-existent cosigner should return nil
		nonExistentKey, err := btcec.NewPrivateKey()
		require.NoError(t, err)
		notFoundLeaf, err := tree.GetLeafForCosigner(nonExistentKey.PubKey())
		require.NoError(t, err)
		require.Nil(t, notFoundLeaf)
	})

	t.Run("multi-leaf tree", func(t *testing.T) {
		leaves := []Leaf{
			{PkScript: []byte("alice_script"), Amount: 5000, SignerKey: user1PubKey},
			{PkScript: []byte("bob_script"), Amount: 3000, SignerKey: user2PubKey},
			{PkScript: []byte("charlie_script"), Amount: 2000, SignerKey: user3PubKey},
		}

		tree, err := BuildVTXOTree(input, leaves, 144, operatorSweepPubKey, operatorSigningPubKey, 2, prevOut)
		require.NoError(t, err)

		// Find Alice's leaf
		aliceLeaf, err := tree.GetLeafForCosigner(user1PubKey)
		require.NoError(t, err)
		require.NotNil(t, aliceLeaf)
		require.Len(t, aliceLeaf.Children, 0) // Should be a leaf
		require.True(t, ContainsCosigner(aliceLeaf.CoSigners, user1PubKey))
		require.Equal(t, int64(5000), aliceLeaf.Outputs[0].Value)
		require.Equal(t, []byte("alice_script"), aliceLeaf.Outputs[0].PkScript)

		// Find Bob's leaf
		bobLeaf, err := tree.GetLeafForCosigner(user2PubKey)
		require.NoError(t, err)
		require.NotNil(t, bobLeaf)
		require.Len(t, bobLeaf.Children, 0) // Should be a leaf
		require.True(t, ContainsCosigner(bobLeaf.CoSigners, user2PubKey))
		require.Equal(t, int64(3000), bobLeaf.Outputs[0].Value)
		require.Equal(t, []byte("bob_script"), bobLeaf.Outputs[0].PkScript)

		// Find Charlie's leaf
		charlieLeaf, err := tree.GetLeafForCosigner(user3PubKey)
		require.NoError(t, err)
		require.NotNil(t, charlieLeaf)
		require.True(t, ContainsCosigner(charlieLeaf.CoSigners, user3PubKey))
		require.Equal(t, int64(2000), charlieLeaf.Outputs[0].Value)
		require.Equal(t, []byte("charlie_script"), charlieLeaf.Outputs[0].PkScript)

		// Verify they're all different leaves
		require.NotEqual(t, aliceLeaf, bobLeaf)
		require.NotEqual(t, aliceLeaf, charlieLeaf)
		require.NotEqual(t, bobLeaf, charlieLeaf)

		// Operator should find the first leaf (implementation dependent)
		operatorLeaf, err := tree.GetLeafForCosigner(operatorSigningPubKey)
		require.NoError(t, err)
		require.NotNil(t, operatorLeaf)
		require.True(t, ContainsCosigner(operatorLeaf.CoSigners, operatorSigningPubKey))
	})

	t.Run("tree method delegates to root", func(t *testing.T) {
		leaves := []Leaf{
			{PkScript: []byte("test_script"), Amount: 2000, SignerKey: user1PubKey},
		}

		tree, err := BuildVTXOTree(input, leaves, 144, operatorSweepPubKey, operatorSigningPubKey, 2, prevOut)
		require.NoError(t, err)

		// Both methods should return the same result
		treeLeaf, err := tree.GetLeafForCosigner(user1PubKey)
		require.NoError(t, err)

		rootLeaf, err := tree.Root.GetLeafForCosigner(user1PubKey)
		require.NoError(t, err)

		require.Equal(t, treeLeaf, rootLeaf)
	})
}

func TestGetNonAnchorOutpoint(t *testing.T) {
	// Generate test keys
	operatorSweepKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorSweepPubKey := operatorSweepKey.PubKey()

	operatorSigningKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorSigningPubKey := operatorSigningKey.PubKey()

	user1Key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	user1PubKey := user1Key.PubKey()

	user2Key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	user2PubKey := user2Key.PubKey()

	input := &wire.OutPoint{
		Hash:  [32]byte{1, 2, 3, 4, 5},
		Index: 0,
	}

	prevOut := &wire.TxOut{
		Value:    8000,
		PkScript: []byte("mock_script"),
	}

	t.Run("nil node GetNonAnchorOutpoint", func(t *testing.T) {
		var nilNode *TreeNode
		outpoint, err := nilNode.GetNonAnchorOutpoint()
		require.Error(t, err)
		require.Nil(t, outpoint)
		require.Contains(t, err.Error(), "cannot get outpoint from nil node")
	})

	t.Run("single leaf tree", func(t *testing.T) {
		leaves := []Leaf{
			{
				PkScript:  []byte("alice_vtxo"),
				Amount:    4000,
				SignerKey: user1PubKey,
			},
		}

		tree, err := BuildVTXOTree(input, leaves, 144, operatorSweepPubKey, operatorSigningPubKey, 2, prevOut)
		require.NoError(t, err)

		// Get the leaf and its non-anchor outpoint
		leaf, err := tree.GetLeafForCosigner(user1PubKey)
		require.NoError(t, err)
		require.NotNil(t, leaf)

		outpoint, err := leaf.GetNonAnchorOutpoint()
		require.NoError(t, err)
		require.NotNil(t, outpoint)

		// Verify the outpoint structure
		txHash, err := leaf.TXID()
		require.NoError(t, err)
		require.Equal(t, txHash, outpoint.Hash)
		require.Equal(t, uint32(0), outpoint.Index) // First output should be the VTXO

		// Verify the output exists and has the right value
		require.Len(t, leaf.Outputs, 2) // VTXO + anchor
		require.Equal(t, int64(4000), leaf.Outputs[0].Value)
		require.Equal(t, int64(0), leaf.Outputs[1].Value) // Anchor
	})

	t.Run("multi-leaf tree", func(t *testing.T) {
		leaves := []Leaf{
			{PkScript: []byte("alice_vtxo"), Amount: 3000, SignerKey: user1PubKey},
			{PkScript: []byte("bob_vtxo"), Amount: 2000, SignerKey: user2PubKey},
		}

		tree, err := BuildVTXOTree(input, leaves, 144, operatorSweepPubKey, operatorSigningPubKey, 2, prevOut)
		require.NoError(t, err)

		// Test Alice's leaf
		aliceLeaf, err := tree.GetLeafForCosigner(user1PubKey)
		require.NoError(t, err)
		require.NotNil(t, aliceLeaf)

		aliceOutpoint, err := aliceLeaf.GetNonAnchorOutpoint()
		require.NoError(t, err)
		require.NotNil(t, aliceOutpoint)

		aliceTxHash, err := aliceLeaf.TXID()
		require.NoError(t, err)
		require.Equal(t, aliceTxHash, aliceOutpoint.Hash)
		require.Equal(t, uint32(0), aliceOutpoint.Index)

		// Test Bob's leaf
		bobLeaf, err := tree.GetLeafForCosigner(user2PubKey)
		require.NoError(t, err)
		require.NotNil(t, bobLeaf)

		bobOutpoint, err := bobLeaf.GetNonAnchorOutpoint()
		require.NoError(t, err)
		require.NotNil(t, bobOutpoint)

		bobTxHash, err := bobLeaf.TXID()
		require.NoError(t, err)
		require.Equal(t, bobTxHash, bobOutpoint.Hash)
		require.Equal(t, uint32(0), bobOutpoint.Index)

		// Outpoints should be different
		require.NotEqual(t, aliceOutpoint, bobOutpoint)
		require.NotEqual(t, aliceOutpoint.Hash, bobOutpoint.Hash)
	})

	t.Run("non-leaf node error", func(t *testing.T) {
		leaves := []Leaf{
			{PkScript: []byte("leaf1"), Amount: 2000, SignerKey: user1PubKey},
			{PkScript: []byte("leaf2"), Amount: 1000, SignerKey: user2PubKey},
		}

		tree, err := BuildVTXOTree(input, leaves, 144, operatorSweepPubKey, operatorSigningPubKey, 2, prevOut)
		require.NoError(t, err)

		// Try to get outpoint from root (non-leaf) node
		outpoint, err := tree.Root.GetNonAnchorOutpoint()
		require.Error(t, err)
		require.Nil(t, outpoint)
		require.Contains(t, err.Error(), "node is not a leaf")
	})

	t.Run("integration test - complete flow", func(t *testing.T) {
		// Test the complete flow: build tree -> find leaf for cosigner -> get outpoint
		leaves := []Leaf{
			{PkScript: []byte("alice_vtxo"), Amount: 5000, SignerKey: user1PubKey},
			{PkScript: []byte("bob_vtxo"), Amount: 3000, SignerKey: user2PubKey},
		}

		tree, err := BuildVTXOTree(input, leaves, 144, operatorSweepPubKey, operatorSigningPubKey, 2, prevOut)
		require.NoError(t, err)

		// Complete flow for Alice
		aliceLeaf, err := tree.GetLeafForCosigner(user1PubKey)
		require.NoError(t, err)
		require.NotNil(t, aliceLeaf)

		aliceOutpoint, err := aliceLeaf.GetNonAnchorOutpoint()
		require.NoError(t, err)
		require.NotNil(t, aliceOutpoint)

		// Verify we can use the outpoint to identify the correct output
		aliceTxHash, err := aliceLeaf.TXID()
		require.NoError(t, err)
		require.Equal(t, aliceTxHash, aliceOutpoint.Hash)

		// The outpoint should reference the VTXO output
		vtxoOutput := aliceLeaf.Outputs[aliceOutpoint.Index]
		require.Equal(t, int64(5000), vtxoOutput.Value)
		require.Equal(t, []byte("alice_vtxo"), vtxoOutput.PkScript)

		// Complete flow for Bob
		bobLeaf, err := tree.GetLeafForCosigner(user2PubKey)
		require.NoError(t, err)
		require.NotNil(t, bobLeaf)

		bobOutpoint, err := bobLeaf.GetNonAnchorOutpoint()
		require.NoError(t, err)
		require.NotNil(t, bobOutpoint)

		// Verify Bob's outpoint
		bobTxHash, err := bobLeaf.TXID()
		require.NoError(t, err)
		require.Equal(t, bobTxHash, bobOutpoint.Hash)

		bobVtxoOutput := bobLeaf.Outputs[bobOutpoint.Index]
		require.Equal(t, int64(3000), bobVtxoOutput.Value)
		require.Equal(t, []byte("bob_vtxo"), bobVtxoOutput.PkScript)
	})
}

func TestExtractPathForIndex(t *testing.T) {
	// Generate test keys
	connectorKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	connectorPubKey := connectorKey.PubKey()

	input := &wire.OutPoint{
		Hash:  [32]byte{1, 2, 3, 4, 5},
		Index: 0,
	}

	prevOut := &wire.TxOut{
		Value:    10000,
		PkScript: []byte("mock_script"),
	}

	t.Run("nil tree ExtractPathForIndex", func(t *testing.T) {
		var nilTree *TreeNode
		extracted, err := nilTree.ExtractPathForIndex(0)
		require.NoError(t, err)
		require.Nil(t, extracted)
	})

	t.Run("negative index error", func(t *testing.T) {
		tree, err := BuildConnectorTree(input, 3, 1000, connectorPubKey, 2, prevOut)
		require.NoError(t, err)

		extracted, err := tree.ExtractPathForIndex(-1)
		require.Error(t, err)
		require.Nil(t, extracted)
		require.Contains(t, err.Error(), "leaf index must be non-negative")
	})

	t.Run("single leaf connector tree", func(t *testing.T) {
		tree, err := BuildConnectorTree(input, 1, 1000, connectorPubKey, 2, prevOut)
		require.NoError(t, err)

		// Extract index 0 (the only leaf)
		extracted, err := tree.ExtractPathForIndex(0)
		require.NoError(t, err)
		require.NotNil(t, extracted)
		require.Equal(t, tree.Root, extracted.Root) // Should be identical for single leaf

		// Extract out-of-bounds index
		outOfBounds, err := tree.ExtractPathForIndex(1)
		require.NoError(t, err)
		require.Nil(t, outOfBounds)
	})

	t.Run("two leaf connector tree", func(t *testing.T) {
		tree, err := BuildConnectorTree(input, 2, 1000, connectorPubKey, 2, prevOut)
		require.NoError(t, err)

		// Extract index 0
		path0, err := tree.ExtractPathForIndex(0)
		require.NoError(t, err)
		require.NotNil(t, path0)

		// Extract index 1
		path1, err := tree.ExtractPathForIndex(1)
		require.NoError(t, err)
		require.NotNil(t, path1)

		// Verify both paths have the same root structure but different leaves
		require.Equal(t, tree.Root.Input, path0.Root.Input)
		require.Equal(t, tree.Root.Input, path1.Root.Input)

		// Each path should have exactly one leaf
		leaves0, err := path0.GetLeafNodes()
		require.NoError(t, err)
		require.Len(t, leaves0, 1)

		leaves1, err := path1.GetLeafNodes()
		require.NoError(t, err)
		require.Len(t, leaves1, 1)

		// The leaves should be different
		leaf0Hash, err := leaves0[0].TXID()
		require.NoError(t, err)
		leaf1Hash, err := leaves1[0].TXID()
		require.NoError(t, err)
		require.NotEqual(t, leaf0Hash, leaf1Hash)

		// Extract out-of-bounds index
		outOfBounds, err := tree.ExtractPathForIndex(2)
		require.NoError(t, err)
		require.Nil(t, outOfBounds)
	})

	t.Run("five leaf connector tree", func(t *testing.T) {
		tree, err := BuildConnectorTree(input, 5, 1000, connectorPubKey, 2, prevOut)
		require.NoError(t, err)

		// Test all valid indices
		for i := 0; i < 5; i++ {
			path, err := tree.ExtractPathForIndex(i)
			require.NoError(t, err)
			require.NotNil(t, path, "Failed to extract path for index %d", i)

			// Verify path has exactly one leaf
			leaves, err := path.GetLeafNodes()
			require.NoError(t, err)
			require.Len(t, leaves, 1, "Path %d should have exactly one leaf", i)

			// Verify the leaf is a connector leaf with the right properties
			leaf := leaves[0]
			require.Len(t, leaf.Children, 0) // Should be a leaf
			require.True(t, ContainsCosigner(leaf.CoSigners, connectorPubKey))
			require.Equal(t, int64(1000), leaf.Outputs[0].Value)
		}

		// Test out-of-bounds indices
		outOfBounds, err := tree.ExtractPathForIndex(5)
		require.NoError(t, err)
		require.Nil(t, outOfBounds)

		outOfBounds, err = tree.ExtractPathForIndex(10)
		require.NoError(t, err)
		require.Nil(t, outOfBounds)
	})

	t.Run("consistent leaf ordering", func(t *testing.T) {
		tree, err := BuildConnectorTree(input, 4, 1000, connectorPubKey, 2, prevOut)
		require.NoError(t, err)

		// Extract all paths and collect their leaf transaction IDs
		var leafTxIDs []string
		for i := 0; i < 4; i++ {
			path, err := tree.ExtractPathForIndex(i)
			require.NoError(t, err)
			require.NotNil(t, path)

			leaves, err := path.GetLeafNodes()
			require.NoError(t, err)
			require.Len(t, leaves, 1)

			txID, err := leaves[0].TXID()
			require.NoError(t, err)
			leafTxIDs = append(leafTxIDs, txID.String())
		}

		// All leaf transaction IDs should be unique
		uniqueIDs := make(map[string]bool)
		for _, id := range leafTxIDs {
			require.False(t, uniqueIDs[id], "Duplicate leaf transaction ID found: %s", id)
			uniqueIDs[id] = true
		}

		// Extract the same indices again and verify consistency
		for i := 0; i < 4; i++ {
			path, err := tree.ExtractPathForIndex(i)
			require.NoError(t, err)
			leaves, err := path.GetLeafNodes()
			require.NoError(t, err)
			txID, err := leaves[0].TXID()
			require.NoError(t, err)
			require.Equal(t, leafTxIDs[i], txID.String(), "Leaf ordering changed for index %d", i)
		}
	})

	t.Run("tree method delegates to root", func(t *testing.T) {
		tree, err := BuildConnectorTree(input, 3, 1000, connectorPubKey, 2, prevOut)
		require.NoError(t, err)

		// Both methods should return the same result
		treePath, err := tree.ExtractPathForIndex(1)
		require.NoError(t, err)

		rootPath, err := tree.Root.ExtractPathForIndex(1)
		require.NoError(t, err)

		// Compare the roots
		require.Equal(t, treePath.Root.Input, rootPath.Input)
		require.Equal(t, treePath.Root.CoSigners, rootPath.CoSigners)
		require.Equal(t, treePath.Root.Outputs, rootPath.Outputs)
	})

	t.Run("integration with GetNonAnchorOutpoint", func(t *testing.T) {
		// Test the complete flow for connector trees: extract path by index -> get outpoint
		tree, err := BuildConnectorTree(input, 3, 1000, connectorPubKey, 2, prevOut)
		require.NoError(t, err)

		for i := 0; i < 3; i++ {
			// Extract path for index i
			path, err := tree.ExtractPathForIndex(i)
			require.NoError(t, err)
			require.NotNil(t, path)

			// Get the leaf from the path
			leaves, err := path.GetLeafNodes()
			require.NoError(t, err)
			require.Len(t, leaves, 1)
			leaf := leaves[0]

			// Get the non-anchor outpoint
			outpoint, err := leaf.GetNonAnchorOutpoint()
			require.NoError(t, err)
			require.NotNil(t, outpoint)

			// Verify the outpoint references the connector output
			require.Equal(t, uint32(0), outpoint.Index) // Should be first output
			connectorOutput := leaf.Outputs[outpoint.Index]
			require.Equal(t, int64(1000), connectorOutput.Value)

			txHash, err := leaf.TXID()
			require.NoError(t, err)
			require.Equal(t, txHash, outpoint.Hash)
		}
	})

	t.Run("compare with ForEachLeaf indexing", func(t *testing.T) {
		tree, err := BuildConnectorTree(input, 4, 1000, connectorPubKey, 2, prevOut)
		require.NoError(t, err)

		// Collect leaves using ForEachLeaf (in traversal order)
		var forEachLeaves []*TreeNode
		err = tree.ForEachLeaf(func(node *TreeNode) error {
			forEachLeaves = append(forEachLeaves, node)
			return nil
		})
		require.NoError(t, err)
		require.Len(t, forEachLeaves, 4)

		// Compare with ExtractPathForIndex results
		for i := 0; i < 4; i++ {
			path, err := tree.ExtractPathForIndex(i)
			require.NoError(t, err)
			leaves, err := path.GetLeafNodes()
			require.NoError(t, err)
			require.Len(t, leaves, 1)

			// The leaf transaction IDs should match
			forEachTxID, err := forEachLeaves[i].TXID()
			require.NoError(t, err)
			extractedTxID, err := leaves[0].TXID()
			require.NoError(t, err)
			require.Equal(t, forEachTxID, extractedTxID, "Leaf index %d mismatch", i)
		}
	})
}

func TestSubmitTreeSigsAndVerify(t *testing.T) {
	// Generate test keys
	operatorSweepKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorSweepPubKey := operatorSweepKey.PubKey()

	operatorSigningKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	operatorSigningPubKey := operatorSigningKey.PubKey()

	user1Key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	user1PubKey := user1Key.PubKey()

	// Create initial previous output
	initialPrevOut := &wire.TxOut{
		Value:    10000000, // 0.1 BTC
		PkScript: []byte("initial_commitment_script"),
	}

	input := &wire.OutPoint{
		Hash:  [32]byte{1, 2, 3, 4, 5},
		Index: 0,
	}

	leaves := []Leaf{
		{
			PkScript:  []byte("user1_vtxo_script"),
			Amount:    5000000, // 0.05 BTC
			SignerKey: user1PubKey,
		},
	}

	tree, err := BuildVTXOTree(input, leaves, 144, operatorSweepPubKey, operatorSigningPubKey, 2, initialPrevOut)
	require.NoError(t, err)

	// Test SubmitTreeSigs with verification
	t.Run("submit and verify tree signatures", func(t *testing.T) {
		// Create mock signatures
		txHash, err := tree.Root.TXID()
		require.NoError(t, err)

		// Create a mock signature (this won't be cryptographically valid)
		mockSig := &schnorr.Signature{}
		sigs := map[string]*schnorr.Signature{
			txHash.String(): mockSig,
		}

		// First, just submit signatures without verification
		err = tree.SubmitTreeSigs(sigs)
		require.NoError(t, err)

		// Verify the signature was stored
		require.NotNil(t, tree.Root.Signature)
		require.Equal(t, mockSig, tree.Root.Signature)

		// Test verification separately (this will fail with mock signature)
		err = tree.VerifySigned()
		require.Error(t, err) // Expected to fail with mock signature
		require.Contains(t, err.Error(), "signature verification failed")
	})

	t.Run("to signed transaction", func(t *testing.T) {
		// Test ToSignedTx method
		signedTx, err := tree.Root.ToSignedTx()
		require.NoError(t, err)
		require.NotNil(t, signedTx)

		// Should have witness data
		require.Len(t, signedTx.TxIn, 1)
		require.Len(t, signedTx.TxIn[0].Witness, 1) // Just the signature for keyspend
	})

	t.Run("error cases", func(t *testing.T) {
		// Test nil tree
		var nilTree *TreeNode
		err := nilTree.SubmitTreeSigs(nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), "cannot submit signatures to nil tree")

		// Test missing signature
		newTree, err := BuildVTXOTree(input, leaves, 144, operatorSweepPubKey, operatorSigningPubKey, 2, initialPrevOut)
		require.NoError(t, err)

		txHash, err := newTree.Root.TXID()
		require.NoError(t, err)

		// Submit empty signatures map
		emptySigs := make(map[string]*schnorr.Signature)
		err = newTree.SubmitTreeSigs(emptySigs)
		require.Error(t, err)
		require.Contains(t, err.Error(), fmt.Sprintf("signature not found for transaction %s", txHash))
	})
}
