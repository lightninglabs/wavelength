package arkscript

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightninglabs/darepo-client/internal/testutils"
	"github.com/stretchr/testify/require"
)

func TestContainsKey(t *testing.T) {
	t.Parallel()

	key1, _ := testutils.CreateKey(1)
	key2, _ := testutils.CreateKey(2)
	key3, _ := testutils.CreateKey(3)

	t.Run("checksig contains key", func(t *testing.T) {
		t.Parallel()

		node := &Checksig{Key: key1}
		require.True(t, ContainsKey(node, key1))
		require.False(t, ContainsKey(node, key2))
	})

	t.Run("multisig contains key", func(t *testing.T) {
		t.Parallel()

		node := &Multisig{
			Keys: []*btcec.PublicKey{key1, key2},
			Type: MultisigTypeChecksig,
		}
		require.True(t, ContainsKey(node, key1))
		require.True(t, ContainsKey(node, key2))
		require.False(t, ContainsKey(node, key3))
	})

	t.Run("csv wrapping checksig", func(t *testing.T) {
		t.Parallel()

		node := &CSV{
			Lock:  100,
			Inner: &Checksig{Key: key1},
		}
		require.True(t, ContainsKey(node, key1))
		require.False(t, ContainsKey(node, key2))
	})

	t.Run("hashlock wrapping multisig", func(t *testing.T) {
		t.Parallel()

		node := &HashLock{
			Algorithm: HashAlgoHash160,
			Hash:      make([]byte, 20),
			Inner: &Multisig{
				Keys: []*btcec.PublicKey{key1, key2},
				Type: MultisigTypeChecksig,
			},
		}
		require.True(t, ContainsKey(node, key1))
		require.True(t, ContainsKey(node, key2))
		require.False(t, ContainsKey(node, key3))
	})

	t.Run("nil inputs", func(t *testing.T) {
		t.Parallel()

		require.False(t, ContainsKey(nil, key1))
		require.False(t, ContainsKey(&Checksig{Key: key1}, nil))
	})
}

func TestExtractCSVDelay(t *testing.T) {
	t.Parallel()

	key, _ := testutils.CreateKey(1)

	t.Run("csv node returns lock", func(t *testing.T) {
		t.Parallel()

		node := &CSV{
			Lock:  144,
			Inner: &Checksig{Key: key},
		}
		require.Equal(t, uint32(144), ExtractCSVDelay(node))
	})

	t.Run("no csv returns zero", func(t *testing.T) {
		t.Parallel()

		node := &Checksig{Key: key}
		require.Equal(t, uint32(0), ExtractCSVDelay(node))
	})

	t.Run("cltv without csv returns zero", func(t *testing.T) {
		t.Parallel()

		node := &CLTV{
			Lock:  500000,
			Inner: &Checksig{Key: key},
		}
		require.Equal(t, uint32(0), ExtractCSVDelay(node))
	})

	t.Run("csv nested in hashlock", func(t *testing.T) {
		t.Parallel()

		node := &CSV{
			Lock: 288,
			Inner: &HashLock{
				Algorithm: HashAlgoHash160,
				Hash:      make([]byte, 20),
				Inner:     &Checksig{Key: key},
			},
		}
		require.Equal(t, uint32(288), ExtractCSVDelay(node))
	})
}

func TestValidatePolicy(t *testing.T) {
	t.Parallel()

	owner, _ := testutils.CreateKey(1)
	operator, _ := testutils.CreateKey(2)
	other, _ := testutils.CreateKey(3)

	opts := PolicyValidationOpts{
		OperatorKey:  operator,
		MinExitDelay: 10,
	}

	t.Run("valid standard vtxo", func(t *testing.T) {
		t.Parallel()

		collabNode := &Multisig{
			Keys: []*btcec.PublicKey{owner, operator},
			Type: MultisigTypeChecksig,
		}
		exitNode := &CSV{
			Lock:  100,
			Inner: &Checksig{Key: owner},
		}

		collabScript, _ := collabNode.Script()
		exitScript, _ := exitNode.Script()

		leaves := []PolicyLeaf{
			{Role: LeafRoleCollab, Leaf: txscript.NewBaseTapLeaf(collabScript)},
			{Role: LeafRoleExit, Leaf: txscript.NewBaseTapLeaf(exitScript)},
		}
		nodes := []Node{collabNode, exitNode}

		err := ValidatePolicy(leaves, nodes, opts)
		require.NoError(t, err)
	})

	t.Run("collab leaf missing operator key", func(t *testing.T) {
		t.Parallel()

		collabNode := &Multisig{
			Keys: []*btcec.PublicKey{owner, other},
			Type: MultisigTypeChecksig,
		}
		exitNode := &CSV{
			Lock:  100,
			Inner: &Checksig{Key: owner},
		}

		collabScript, _ := collabNode.Script()
		exitScript, _ := exitNode.Script()

		leaves := []PolicyLeaf{
			{Role: LeafRoleCollab, Leaf: txscript.NewBaseTapLeaf(collabScript)},
			{Role: LeafRoleExit, Leaf: txscript.NewBaseTapLeaf(exitScript)},
		}
		nodes := []Node{collabNode, exitNode}

		err := ValidatePolicy(leaves, nodes, opts)
		require.Error(t, err)
		require.Contains(t, err.Error(), "operator key")
	})

	t.Run("exit leaf not csv-gated", func(t *testing.T) {
		t.Parallel()

		collabNode := &Multisig{
			Keys: []*btcec.PublicKey{owner, operator},
			Type: MultisigTypeChecksig,
		}
		exitNode := &Checksig{Key: owner}

		collabScript, _ := collabNode.Script()
		exitScript, _ := exitNode.Script()

		leaves := []PolicyLeaf{
			{Role: LeafRoleCollab, Leaf: txscript.NewBaseTapLeaf(collabScript)},
			{Role: LeafRoleExit, Leaf: txscript.NewBaseTapLeaf(exitScript)},
		}
		nodes := []Node{collabNode, exitNode}

		err := ValidatePolicy(leaves, nodes, opts)
		require.Error(t, err)
		require.Contains(t, err.Error(), "not CSV-gated")
	})

	t.Run("exit delay below minimum", func(t *testing.T) {
		t.Parallel()

		collabNode := &Multisig{
			Keys: []*btcec.PublicKey{owner, operator},
			Type: MultisigTypeChecksig,
		}
		exitNode := &CSV{
			Lock:  5, // below MinExitDelay=10
			Inner: &Checksig{Key: owner},
		}

		collabScript, _ := collabNode.Script()
		exitScript, _ := exitNode.Script()

		leaves := []PolicyLeaf{
			{Role: LeafRoleCollab, Leaf: txscript.NewBaseTapLeaf(collabScript)},
			{Role: LeafRoleExit, Leaf: txscript.NewBaseTapLeaf(exitScript)},
		}
		nodes := []Node{collabNode, exitNode}

		err := ValidatePolicy(leaves, nodes, opts)
		require.Error(t, err)
		require.Contains(t, err.Error(), "below minimum")
	})

	t.Run("valid vhtlc policy", func(t *testing.T) {
		t.Parallel()

		preimageHash := make([]byte, 20)

		policy, err := NewVHTLCPolicy(VHTLCOpts{
			Sender:       owner,
			Receiver:     other,
			Server:       operator,
			PreimageHash: preimageHash,
			RefundLocktime:                       500000,
			UnilateralClaimDelay:                 144,
			UnilateralRefundDelay:                288,
			UnilateralRefundWithoutReceiverDelay: 1008,
		})
		require.NoError(t, err)

		// Collect nodes in leaf order.
		nodes := policy.OrderedNodes()

		err = ValidatePolicy(
			policy.Leaves, nodes,
			PolicyValidationOpts{
				OperatorKey:  operator,
				MinExitDelay: 100,
			},
		)
		require.NoError(t, err)
	})

	t.Run("missing collab leaf", func(t *testing.T) {
		t.Parallel()

		exitNode := &CSV{
			Lock:  100,
			Inner: &Checksig{Key: owner},
		}
		exitScript, _ := exitNode.Script()

		leaves := []PolicyLeaf{
			{Role: LeafRoleExit, Leaf: txscript.NewBaseTapLeaf(exitScript)},
		}
		nodes := []Node{exitNode}

		err := ValidatePolicy(leaves, nodes, opts)
		require.ErrorIs(t, err, ErrMissingCollab)
	})
}

func TestScriptContainsKey(t *testing.T) {
	t.Parallel()

	key1, _ := testutils.CreateKey(1)
	key2, _ := testutils.CreateKey(2)

	node := &Multisig{
		Keys: []*btcec.PublicKey{key1, key2},
		Type: MultisigTypeChecksig,
	}
	script, err := node.Script()
	require.NoError(t, err)

	require.True(t, ScriptContainsKey(script, key1))
	require.True(t, ScriptContainsKey(script, key2))

	key3, _ := testutils.CreateKey(3)
	require.False(t, ScriptContainsKey(script, key3))
}
