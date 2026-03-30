package arkscript

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/darepo-client/internal/testutils"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

// TestContainsKey verifies the AST-level key lookup across all node types.
func TestContainsKey(t *testing.T) {
	t.Parallel()

	key1, _ := testutils.CreateKey(1)
	key2, _ := testutils.CreateKey(2)
	key3, _ := testutils.CreateKey(3)

	t.Run("checksig contains key", func(t *testing.T) {
		t.Parallel()

		node := &Multisig{Keys: []*btcec.PublicKey{key1}}
		require.True(t, ContainsKey(node, key1))
		require.False(t, ContainsKey(node, key2))
	})

	t.Run("multisig contains key", func(t *testing.T) {
		t.Parallel()

		node := &Multisig{
			Keys: []*btcec.PublicKey{key1, key2},
		}
		require.True(t, ContainsKey(node, key1))
		require.True(t, ContainsKey(node, key2))
		require.False(t, ContainsKey(node, key3))
	})

	t.Run("csv wrapping checksig", func(t *testing.T) {
		t.Parallel()

		node := &CSV{
			Lock:  100,
			Inner: &Multisig{Keys: []*btcec.PublicKey{key1}},
		}
		require.True(t, ContainsKey(node, key1))
		require.False(t, ContainsKey(node, key2))
	})

	t.Run("condition wrapping multisig", func(t *testing.T) {
		t.Parallel()

		predicate, err := Hash160Condition(make([]byte, 20))
		require.NoError(t, err)

		node := &Condition{
			Predicate: predicate,
			Inner: &Multisig{
				Keys: []*btcec.PublicKey{key1, key2},
			},
		}
		require.True(t, ContainsKey(node, key1))
		require.True(t, ContainsKey(node, key2))
		require.False(t, ContainsKey(node, key3))
	})

	t.Run("nil inputs", func(t *testing.T) {
		t.Parallel()

		require.False(t, ContainsKey(nil, key1))
		require.False(t, ContainsKey(
			&Multisig{Keys: []*btcec.PublicKey{key1}}, nil,
		))
	})
}

// TestExtractCSVDelay verifies CSV delay extraction from nested AST structures.
func TestExtractCSVDelay(t *testing.T) {
	t.Parallel()

	key, _ := testutils.CreateKey(1)

	t.Run("csv node returns lock", func(t *testing.T) {
		t.Parallel()

		node := &CSV{
			Lock:  144,
			Inner: &Multisig{Keys: []*btcec.PublicKey{key}},
		}
		require.Equal(t, uint32(144), ExtractCSVDelay(node))
	})

	t.Run("no csv returns zero", func(t *testing.T) {
		t.Parallel()

		node := &Multisig{Keys: []*btcec.PublicKey{key}}
		require.Equal(t, uint32(0), ExtractCSVDelay(node))
	})

	t.Run("absolute locktime condition without csv returns zero",
		func(t *testing.T) {
			t.Parallel()

			lockPrefix, err := AbsoluteLockTimeCondition(500000)
			require.NoError(t, err)

			node := &Condition{
				Predicate: lockPrefix,
				Inner: &Multisig{
					Keys: []*btcec.PublicKey{key},
				},
			}
			require.Equal(t, uint32(0), ExtractCSVDelay(node))
		},
	)

	t.Run("csv nested in condition", func(t *testing.T) {
		t.Parallel()

		predicate, err := Hash160Condition(make([]byte, 20))
		require.NoError(t, err)

		node := &CSV{
			Lock: 288,
			Inner: &Condition{
				Predicate: predicate,
				Inner: &Multisig{
					Keys: []*btcec.PublicKey{key},
				},
			},
		}
		require.Equal(t, uint32(288), ExtractCSVDelay(node))
	})
}

// TestValidatePolicy checks Ark policy invariant enforcement for various
// valid and invalid policy configurations.
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
		}
		exitNode := &CSV{
			Lock:  100,
			Inner: &Multisig{Keys: []*btcec.PublicKey{owner}},
		}

		nodes := []Node{collabNode, exitNode}

		err := ValidatePolicy(nodes, opts)
		require.NoError(t, err)
	})

	t.Run("collab leaf missing operator key", func(t *testing.T) {
		t.Parallel()

		collabNode := &Multisig{
			Keys: []*btcec.PublicKey{owner, other},
		}
		exitNode := &CSV{
			Lock:  100,
			Inner: &Multisig{Keys: []*btcec.PublicKey{owner}},
		}

		nodes := []Node{collabNode, exitNode}

		err := ValidatePolicy(nodes, opts)
		require.Error(t, err)
		require.ErrorIs(t, err, ErrMissingCollab)
	})

	t.Run("exit leaf not csv-gated", func(t *testing.T) {
		t.Parallel()

		collabNode := &Multisig{
			Keys: []*btcec.PublicKey{owner, operator},
		}
		exitNode := &Multisig{Keys: []*btcec.PublicKey{owner}}

		nodes := []Node{collabNode, exitNode}

		err := ValidatePolicy(nodes, opts)
		require.Error(t, err)
		require.Contains(t, err.Error(), "not CSV-gated")
	})

	t.Run("exit delay below minimum", func(t *testing.T) {
		t.Parallel()

		collabNode := &Multisig{
			Keys: []*btcec.PublicKey{owner, operator},
		}
		exitNode := &CSV{
			Lock:  5, // below MinExitDelay=10
			Inner: &Multisig{Keys: []*btcec.PublicKey{owner}},
		}

		nodes := []Node{collabNode, exitNode}

		err := ValidatePolicy(nodes, opts)
		require.Error(t, err)
		require.Contains(t, err.Error(), "below minimum")
	})

	t.Run("valid vhtlc policy", func(t *testing.T) {
		t.Parallel()

		preimage, err := lntypes.MakePreimage(
			[]byte("validate-test-preimage-32-bytes!"),
		)
		require.NoError(t, err)
		paymentHash := preimage.Hash()

		policy, err := NewVHTLCPolicy(VHTLCOpts{
			Sender:                               owner,
			Receiver:                             other,
			Server:                               operator,
			PreimageHash:                         paymentHash[:],
			RefundLocktime:                       500000,
			UnilateralClaimDelay:                 144,
			UnilateralRefundDelay:                288,
			UnilateralRefundWithoutReceiverDelay: 1008,
		})
		require.NoError(t, err)

		err = policy.Template.ValidateArkPolicy(
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
			Inner: &Multisig{Keys: []*btcec.PublicKey{owner}},
		}
		nodes := []Node{exitNode}

		err := ValidatePolicy(nodes, opts)
		require.ErrorIs(t, err, ErrMissingCollab)
	})
}

// TestScriptContainsKey tests the byte-level script key substring scan.
func TestScriptContainsKey(t *testing.T) {
	t.Parallel()

	key1, _ := testutils.CreateKey(1)
	key2, _ := testutils.CreateKey(2)

	node := &Multisig{
		Keys: []*btcec.PublicKey{key1, key2},
	}
	script, err := node.Script()
	require.NoError(t, err)

	require.True(t, ScriptContainsKey(script, key1))
	require.True(t, ScriptContainsKey(script, key2))

	key3, _ := testutils.CreateKey(3)
	require.False(t, ScriptContainsKey(script, key3))
}
