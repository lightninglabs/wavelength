package arkscript

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/wavelength/internal/testutils"
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
			Keys: []*btcec.PublicKey{
				key1,
				key2,
			},
		}
		require.True(t, ContainsKey(node, key1))
		require.True(t, ContainsKey(node, key2))
		require.False(t, ContainsKey(node, key3))
	})

	t.Run("csv wrapping checksig", func(t *testing.T) {
		t.Parallel()

		node := &CSV{
			Lock: 100,
			Inner: &Multisig{
				Keys: []*btcec.PublicKey{
					key1,
				},
			},
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
				Keys: []*btcec.PublicKey{
					key1,
					key2,
				},
			},
		}
		require.True(t, ContainsKey(node, key1))
		require.True(t, ContainsKey(node, key2))
		require.False(t, ContainsKey(node, key3))
	})

	t.Run("nil inputs", func(t *testing.T) {
		t.Parallel()

		require.False(t, ContainsKey(nil, key1))
		require.False(
			t,
			ContainsKey(
				&Multisig{
					Keys: []*btcec.PublicKey{key1},
				},
				nil,
			),
		)
	})
}

// TestSigningKeysForSpendPathReturnsScriptOrder verifies that callers can
// recover the exact CHECKSIG key order from a semantic policy leaf selected by
// a spend path. Round forfeit validation uses this order to assemble
// multi-participant custom-policy witnesses.
func TestSigningKeysForSpendPathReturnsScriptOrder(t *testing.T) {
	t.Parallel()

	sender, _ := testutils.CreateKey(1)
	receiver, _ := testutils.CreateKey(2)
	operator, _ := testutils.CreateKey(3)

	var hash lntypes.Hash
	copy(hash[:], []byte("test payment hash for vhtlc key"))

	policy, err := NewVHTLCPolicy(VHTLCOpts{
		Sender:                               sender,
		Receiver:                             receiver,
		Server:                               operator,
		PreimageHash:                         hash,
		RefundLocktime:                       700_000,
		UnilateralClaimDelay:                 144,
		UnilateralRefundDelay:                144,
		UnilateralRefundWithoutReceiverDelay: 144,
	})
	require.NoError(t, err)

	refundPath, err := policy.RefundPath()
	require.NoError(t, err)

	keys, err := SigningKeysForSpendPath(policy.Template, refundPath)
	require.NoError(t, err)
	require.Len(t, keys, 3)
	require.True(t, keys[0].IsEqual(sender))
	require.True(t, keys[1].IsEqual(receiver))
	require.True(t, keys[2].IsEqual(operator))
}

// TestExtractCSVDelay verifies CSV delay extraction from nested AST structures.
func TestExtractCSVDelay(t *testing.T) {
	t.Parallel()

	key, _ := testutils.CreateKey(1)

	t.Run("csv node returns lock", func(t *testing.T) {
		t.Parallel()

		node := &CSV{
			Lock: 144,
			Inner: &Multisig{
				Keys: []*btcec.PublicKey{
					key,
				},
			},
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
					Keys: []*btcec.PublicKey{
						key,
					},
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
					Keys: []*btcec.PublicKey{
						key,
					},
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
			Keys: []*btcec.PublicKey{
				owner,
				operator,
			},
		}
		exitNode := &CSV{
			Lock: 100,
			Inner: &Multisig{
				Keys: []*btcec.PublicKey{
					owner,
				},
			},
		}

		nodes := []Node{collabNode, exitNode}

		err := ValidatePolicy(nodes, opts)
		require.NoError(t, err)
	})

	t.Run("collab leaf missing operator key", func(t *testing.T) {
		t.Parallel()

		collabNode := &Multisig{
			Keys: []*btcec.PublicKey{
				owner,
				other,
			},
		}
		exitNode := &CSV{
			Lock: 100,
			Inner: &Multisig{
				Keys: []*btcec.PublicKey{
					owner,
				},
			},
		}

		nodes := []Node{collabNode, exitNode}

		err := ValidatePolicy(nodes, opts)
		require.Error(t, err)
		require.ErrorIs(t, err, ErrMissingCollab)
	})

	t.Run("exit leaf not csv-gated", func(t *testing.T) {
		t.Parallel()

		collabNode := &Multisig{
			Keys: []*btcec.PublicKey{
				owner,
				operator,
			},
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
			Keys: []*btcec.PublicKey{
				owner,
				operator,
			},
		}
		exitNode := &CSV{
			Lock: 5, // below MinExitDelay=10
			Inner: &Multisig{
				Keys: []*btcec.PublicKey{
					owner,
				},
			},
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
			PreimageHash:                         paymentHash,
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
			Lock: 100,
			Inner: &Multisig{
				Keys: []*btcec.PublicKey{
					owner,
				},
			},
		}
		nodes := []Node{exitNode}

		err := ValidatePolicy(nodes, opts)
		require.ErrorIs(t, err, ErrMissingCollab)
	})

	// ValidateStandardVTXOPolicy must reject a zero MinExitDelay
	// fail-closed so no caller can accidentally bypass the
	// forfeit-incentive check on the standard-VTXO admission surface.
	t.Run("standard policy zero MinExitDelay rejected", func(t *testing.T) {
		t.Parallel()

		collabNode := &Multisig{
			Keys: []*btcec.PublicKey{
				owner,
				operator,
			},
		}
		exitNode := &CSV{
			Lock: 144,
			Inner: &Multisig{
				Keys: []*btcec.PublicKey{
					owner,
				},
			},
		}
		nodes := []Node{collabNode, exitNode}

		err := ValidateStandardVTXOPolicy(nodes, operator, 0)
		require.ErrorContains(t, err, "MinExitDelay must be non-zero")
	})

	// ValidatePolicy itself permits a zero MinExitDelay because custom
	// policies (e.g. vHTLC) carry protocol-specific unilateral delays
	// independent of the operator's standard VTXO exit delay.
	structuralName := "structural ValidatePolicy allows zero MinExitDelay"
	t.Run(structuralName, func(t *testing.T) {
		t.Parallel()

		collabNode := &Multisig{
			Keys: []*btcec.PublicKey{
				owner,
				operator,
			},
		}
		exitNode := &CSV{
			Lock: 10,
			Inner: &Multisig{
				Keys: []*btcec.PublicKey{
					owner,
				},
			},
		}
		nodes := []Node{collabNode, exitNode}

		err := ValidatePolicy(nodes, PolicyValidationOpts{
			OperatorKey: operator,
		})
		require.NoError(t, err)
	})

	// A leaf that is a plain Multisig containing only the operator key
	// would grant the operator a unilateral spend path. ValidatePolicy
	// must reject it regardless of whether the policy otherwise looks
	// well-formed.
	t.Run("operator-unilateral leaf rejected", func(t *testing.T) {
		t.Parallel()

		collabNode := &Multisig{
			Keys: []*btcec.PublicKey{
				owner,
				operator,
			},
		}
		exitNode := &CSV{
			Lock: 144,
			Inner: &Multisig{
				Keys: []*btcec.PublicKey{
					owner,
				},
			},
		}
		operatorOnly := &Multisig{
			Keys: []*btcec.PublicKey{
				operator,
			},
		}

		nodes := []Node{collabNode, exitNode, operatorOnly}

		err := ValidatePolicy(nodes, opts)
		require.ErrorContains(t, err, "operator unilateral")
	})

	// The same invariant applies when the operator-only leaf is hidden
	// behind a CSV or Condition wrapper: the recursive walk must catch
	// the offending Multisig wherever it appears.
	t.Run("operator-unilateral under CSV rejected", func(t *testing.T) {
		t.Parallel()

		collabNode := &Multisig{
			Keys: []*btcec.PublicKey{
				owner,
				operator,
			},
		}
		exitNode := &CSV{
			Lock: 144,
			Inner: &Multisig{
				Keys: []*btcec.PublicKey{
					owner,
				},
			},
		}
		operatorOnlyCSV := &CSV{
			Lock: 10,
			Inner: &Multisig{
				Keys: []*btcec.PublicKey{
					operator,
				},
			},
		}

		nodes := []Node{collabNode, exitNode, operatorOnlyCSV}

		err := ValidatePolicy(nodes, opts)
		require.ErrorContains(t, err, "operator unilateral")
	})

	// Same invariant, but now the operator-only Multisig is hidden
	// behind a Condition predicate rather than a CSV. The explicit
	// `case *Condition` branch in walkRejectOperatorUnilateral must
	// descend through the predicate wrapper and reject the inner
	// Multisig; a regression that dropped the Condition branch would
	// silently accept this shape.
	conditionName := "operator-unilateral under Condition rejected"
	t.Run(conditionName, func(t *testing.T) {
		t.Parallel()

		collabNode := &Multisig{
			Keys: []*btcec.PublicKey{
				owner,
				operator,
			},
		}
		exitNode := &CSV{
			Lock: 144,
			Inner: &Multisig{
				Keys: []*btcec.PublicKey{
					owner,
				},
			},
		}
		operatorOnlyCondition := &Condition{
			Predicate: []byte{
				0x01,
			},
			Inner: &Multisig{
				Keys: []*btcec.PublicKey{
					operator,
				},
			},
		}

		nodes := []Node{
			collabNode, exitNode, operatorOnlyCondition,
		}

		err := ValidatePolicy(nodes, opts)
		require.ErrorContains(t, err, "operator unilateral")
	})

	// A Multisig with the operator key duplicated — Multisig{op, op} —
	// has len(Keys) == 2 yet is still unilaterally spendable by the
	// operator, since every required signer resolves to the same key.
	// The pre-fix length check treated this as safe; the distinctness
	// check must reject it.
	t.Run("duplicate-operator-key multisig rejected", func(t *testing.T) {
		t.Parallel()

		collabNode := &Multisig{
			Keys: []*btcec.PublicKey{
				owner,
				operator,
			},
		}
		exitNode := &CSV{
			Lock: 144,
			Inner: &Multisig{
				Keys: []*btcec.PublicKey{
					owner,
				},
			},
		}
		duplicateOperator := &Multisig{
			Keys: []*btcec.PublicKey{
				operator,
				operator,
			},
		}

		nodes := []Node{collabNode, exitNode, duplicateOperator}

		err := ValidatePolicy(nodes, opts)
		require.ErrorContains(t, err, "operator-only")
	})

	// The same distinctness check applies when the duplicate-operator
	// Multisig is wrapped in a CSV: unilateral spend after the delay is
	// still unilateral.
	name := "duplicate-operator-key multisig under CSV rejected"
	t.Run(name, func(t *testing.T) {
		t.Parallel()

		collabNode := &Multisig{
			Keys: []*btcec.PublicKey{
				owner,
				operator,
			},
		}
		exitNode := &CSV{
			Lock: 144,
			Inner: &Multisig{
				Keys: []*btcec.PublicKey{
					owner,
				},
			},
		}
		duplicateUnderCSV := &CSV{
			Lock: 10,
			Inner: &Multisig{
				Keys: []*btcec.PublicKey{
					operator,
					operator,
				},
			},
		}

		nodes := []Node{collabNode, exitNode, duplicateUnderCSV}

		err := ValidatePolicy(nodes, opts)
		require.ErrorContains(t, err, "operator-only")
	})
}

// TestScriptContainsKey tests the byte-level script key substring scan.
func TestScriptContainsKey(t *testing.T) {
	t.Parallel()

	key1, _ := testutils.CreateKey(1)
	key2, _ := testutils.CreateKey(2)

	node := &Multisig{
		Keys: []*btcec.PublicKey{
			key1,
			key2,
		},
	}
	script, err := node.Script()
	require.NoError(t, err)

	require.True(t, ScriptContainsKey(script, key1))
	require.True(t, ScriptContainsKey(script, key2))

	key3, _ := testutils.CreateKey(3)
	require.False(t, ScriptContainsKey(script, key3))
}
