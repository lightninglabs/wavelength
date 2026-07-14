package arkscript

import (
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/wavelength/internal/testutils"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// TestNewVTXOPolicyMatchesGoldenVectors verifies that NewVTXOPolicy produces
// byte-identical output to the golden test vectors.
func TestNewVTXOPolicyMatchesGoldenVectors(t *testing.T) {
	t.Parallel()

	for _, vec := range goldenVTXOVectors {
		t.Run(vec.Name, func(t *testing.T) {
			t.Parallel()

			ownerKey, _ := testutils.CreateKey(vec.OwnerKeyIndex)
			operatorKey, _ := testutils.CreateKey(
				vec.OperatorKeyIndex,
			)

			policy, err := NewVTXOPolicy(
				ownerKey, operatorKey, vec.ExitDelay,
			)
			require.NoError(t, err)

			// Verify output key matches.
			outputKey := policy.OutputKey()
			outputKeyHex := hex.EncodeToString(
				outputKey.SerializeCompressed(),
			)
			require.Equal(
				t, vec.OutputKeyHex, outputKeyHex,
				"output key mismatch",
			)

			// Verify root hash matches.
			rootHashHex := hex.EncodeToString(policy.RootHash)
			require.Equal(
				t, vec.RootHashHex, rootHashHex,
				"root hash mismatch",
			)

			// Verify collab spend info matches.
			collabInfo, err := policy.CollabSpendInfo()
			require.NoError(t, err)

			collabScriptHex := hex.EncodeToString(
				collabInfo.WitnessScript,
			)
			require.Equal(
				t, vec.CollabScriptHex, collabScriptHex,
				"collab script mismatch",
			)

			collabControlHex := hex.EncodeToString(
				collabInfo.ControlBlock,
			)
			require.Equal(
				t, vec.CollabControlHex, collabControlHex,
				"collab control block mismatch",
			)

			// Verify timeout control block matches.
			exitInfo, err := policy.ExitSpendInfo()
			require.NoError(t, err)
			timeoutScriptHex := hex.EncodeToString(
				exitInfo.WitnessScript,
			)
			require.Equal(
				t, vec.TimeoutScriptHex, timeoutScriptHex,
				"timeout script mismatch",
			)

			exitControlHex := hex.EncodeToString(
				exitInfo.ControlBlock,
			)
			require.Equal(
				t, vec.TimeoutControlHex, exitControlHex,
				"timeout control block mismatch",
			)
		})
	}
}

// TestNewVTXOPolicyMatchesVTXOTapScript verifies that NewVTXOPolicy produces
// the same output as VTXOTapScript.
func TestNewVTXOPolicyMatchesVTXOTapScript(t *testing.T) {
	t.Parallel()

	ownerKey, _ := testutils.CreateKey(1)
	operatorKey, _ := testutils.CreateKey(2)
	exitDelay := uint32(100)

	// Build using policy API.
	policy, err := NewVTXOPolicy(ownerKey, operatorKey, exitDelay)
	require.NoError(t, err)

	// Build using tapscript API.
	tapscript, err := VTXOTapScript(ownerKey, operatorKey, exitDelay)
	require.NoError(t, err)

	// Verify output keys match.
	taprootKey, err := tapscript.TaprootKey()
	require.NoError(t, err)
	require.Equal(
		t,
		hex.EncodeToString(
			taprootKey.SerializeCompressed(),
		),
		hex.EncodeToString(
			policy.OutputKey().SerializeCompressed(),
		),
		"output keys should match",
	)

	// Verify root hashes match.
	require.Equal(
		t, tapscript.RootHash, policy.RootHash,
		"root hashes should match",
	)

	// Verify leaf scripts match via SpendInfo.
	collabInfo, err := policy.CollabSpendInfo()
	require.NoError(t, err)
	require.Equal(
		t, tapscript.Leaves[0].Script, collabInfo.WitnessScript,
		"collab scripts should match",
	)

	exitInfo, err := policy.ExitSpendInfo()
	require.NoError(t, err)
	require.Equal(
		t, tapscript.Leaves[1].Script, exitInfo.WitnessScript,
		"exit scripts should match",
	)

	// Verify VTXOTapKey matches policy OutputKey.
	tapKey, err := VTXOTapKey(ownerKey, operatorKey, exitDelay)
	require.NoError(t, err)
	require.Equal(
		t,
		hex.EncodeToString(
			tapKey.SerializeCompressed(),
		),
		hex.EncodeToString(
			policy.OutputKey().SerializeCompressed(),
		),
		"VTXOTapKey should match policy OutputKey",
	)
}

// TestNewVTXOPolicyValidation tests parameter validation.
func TestNewVTXOPolicyValidation(t *testing.T) {
	t.Parallel()

	ownerKey, _ := testutils.CreateKey(1)
	operatorKey, _ := testutils.CreateKey(2)

	t.Run("nil owner key", func(t *testing.T) {
		t.Parallel()

		_, err := NewVTXOPolicy(nil, operatorKey, 100)
		require.Error(t, err)
		require.Contains(t, err.Error(), "owner key is nil")
	})

	t.Run("nil operator key", func(t *testing.T) {
		t.Parallel()

		_, err := NewVTXOPolicy(ownerKey, nil, 100)
		require.Error(t, err)
		require.Contains(t, err.Error(), "operator key is nil")
	})

	t.Run("zero exit delay", func(t *testing.T) {
		t.Parallel()

		_, err := NewVTXOPolicy(ownerKey, operatorKey, 0)
		require.Error(t, err)
		require.Contains(t, err.Error(), "exit delay must be non-zero")
	})
}

// TestVTXOSpendInfoTxContext tests tx-context derivation for VTXO leaves.
func TestVTXOSpendInfoTxContext(t *testing.T) {
	t.Parallel()

	ownerKey, _ := testutils.CreateKey(1)
	operatorKey, _ := testutils.CreateKey(2)
	exitDelay := uint32(144)

	policy, err := NewVTXOPolicy(ownerKey, operatorKey, exitDelay)
	require.NoError(t, err)

	t.Run("collab path has no timelock requirements", func(t *testing.T) {
		t.Parallel()

		path, err := policy.CompiledPolicy.SpendPathForNode(
			policy.collabNode, nil,
		)
		require.NoError(t, err)
		require.Equal(t, uint32(0xffffffff),
			path.RequiredSequence)
		require.Equal(t, uint32(0), path.RequiredLockTime)
	})

	t.Run("exit path requires CSV delay", func(t *testing.T) {
		t.Parallel()

		path, err := policy.CompiledPolicy.SpendPathForNode(
			policy.exitNode, nil,
		)
		require.NoError(t, err)
		require.Equal(t, exitDelay, path.RequiredSequence)
		require.Equal(t, uint32(0), path.RequiredLockTime)
	})
}

// TestDeriveSequence tests the sequence derivation function.
func TestDeriveSequence(t *testing.T) {
	t.Parallel()

	key, _ := testutils.CreateKey(1)

	t.Run("simple checksig returns max sequence", func(t *testing.T) {
		t.Parallel()

		node := &Multisig{Keys: []*btcec.PublicKey{key}}
		require.Equal(t, uint32(0xffffffff), DeriveSequence(node))
	})

	t.Run("CSV returns lock value", func(t *testing.T) {
		t.Parallel()

		node := &CSV{
			Lock: 100,
			Inner: &Multisig{
				Keys: []*btcec.PublicKey{
					key,
				},
			},
		}
		require.Equal(t, uint32(100), DeriveSequence(node))
	})

	t.Run("absolute locktime condition returns non-final sequence",
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
			// CLTV requires non-final nSequence (0xfffffffe) so
			// the locktime check is not bypassed.
			require.Equal(
				t, uint32(0xfffffffe), DeriveSequence(node),
			)
		},
	)

	t.Run("CSV nested in absolute locktime condition returns CSV lock",
		func(t *testing.T) {
			t.Parallel()

			lockPrefix, err := AbsoluteLockTimeCondition(500000)
			require.NoError(t, err)

			node := &Condition{
				Predicate: lockPrefix,
				Inner: &CSV{
					Lock: 100,
					Inner: &Multisig{
						Keys: []*btcec.PublicKey{
							key,
						},
					},
				},
			}
			require.Equal(t, uint32(100), DeriveSequence(node))
		})

	t.Run("condition preserves inner timelock", func(t *testing.T) {
		t.Parallel()

		predicate, err := Hash160Condition(make([]byte, 20))
		require.NoError(t, err)

		node := &Condition{
			Predicate: predicate,
			Inner: &CSV{
				Lock: 200,
				Inner: &Multisig{
					Keys: []*btcec.PublicKey{
						key,
					},
				},
			},
		}
		require.Equal(t, uint32(200), DeriveSequence(node))
	})
}

// TestDeriveSequenceProperties uses property-based testing to verify
// invariants of DeriveSequence across random AST trees.
func TestDeriveSequenceProperties(t *testing.T) {
	t.Parallel()

	key, _ := testutils.CreateKey(1)

	t.Run("CSV always returns its lock value", func(t *testing.T) {
		t.Parallel()

		rapid.Check(t, func(t *rapid.T) {
			lock := rapid.Uint32Range(1, 0xffff).Draw(t, "lock")

			node := &CSV{
				Lock: lock,
				Inner: &Multisig{
					Keys: []*btcec.PublicKey{
						key,
					},
				},
			}

			seq := DeriveSequence(node)
			if seq != lock {
				t.Fatalf("CSV(%d) returned seq %d", lock, seq)
			}
		})
	})

	t.Run("CLTV always returns non-final sequence", func(t *testing.T) {
		t.Parallel()

		rapid.Check(t, func(t *rapid.T) {
			locktime := rapid.Uint32Range(1, 0xffffffff-1).
				Draw(t, "locktime")

			pred, err := AbsoluteLockTimeCondition(locktime)
			if err != nil {
				t.Fatalf("AbsoluteLockTimeCondition: %v", err)
			}

			node := &Condition{
				Predicate: pred,
				Inner: &Multisig{
					Keys: []*btcec.PublicKey{
						key,
					},
				},
			}

			seq := DeriveSequence(node)
			if seq != 0xfffffffe {
				t.Fatalf("CLTV(%d) returned sequence %x, want "+
					"0xfffffffe", locktime, seq)
			}
		})
	})

	t.Run("CSV takes priority over CLTV", func(t *testing.T) {
		t.Parallel()

		rapid.Check(t, func(t *rapid.T) {
			csvLock := rapid.Uint32Range(1, 0xffff).
				Draw(t, "csvLock")
			cltvLock := rapid.Uint32Range(1, 0xffffffff-1).
				Draw(t, "cltvLock")

			pred, err := AbsoluteLockTimeCondition(cltvLock)
			if err != nil {
				t.Fatalf("AbsoluteLockTimeCondition: %v", err)
			}

			node := &Condition{
				Predicate: pred,
				Inner: &CSV{
					Lock: csvLock,
					Inner: &Multisig{
						Keys: []*btcec.PublicKey{
							key,
						},
					},
				},
			}

			seq := DeriveSequence(node)
			if seq != csvLock {
				t.Fatalf("CSV(%d)+CLTV(%d) returned sequence "+
					"%d, want CSV lock", csvLock, cltvLock,
					seq)
			}
		})
	})
}

// TestDeriveLockTimeProperties uses property-based testing to verify
// invariants of DeriveLockTime across random AST trees.
func TestDeriveLockTimeProperties(t *testing.T) {
	t.Parallel()

	key, _ := testutils.CreateKey(1)

	t.Run("CLTV round-trips locktime value", func(t *testing.T) {
		t.Parallel()

		rapid.Check(t, func(t *rapid.T) {
			locktime := rapid.Uint32Range(1, 0xffffffff-1).
				Draw(t, "locktime")

			pred, err := AbsoluteLockTimeCondition(locktime)
			if err != nil {
				t.Fatalf("AbsoluteLockTimeCondition: %v", err)
			}

			node := &Condition{
				Predicate: pred,
				Inner: &Multisig{
					Keys: []*btcec.PublicKey{
						key,
					},
				},
			}

			got := DeriveLockTime(node)
			if got != locktime {
				t.Fatalf("DeriveLockTime(CLTV(%d)) = %d",
					locktime, got)
			}
		})
	})

	t.Run("non-CLTV nodes return zero locktime", func(t *testing.T) {
		t.Parallel()

		rapid.Check(t, func(t *rapid.T) {
			lock := rapid.Uint32Range(1, 0xffff).Draw(t, "lock")

			node := &CSV{
				Lock: lock,
				Inner: &Multisig{
					Keys: []*btcec.PublicKey{
						key,
					},
				},
			}

			got := DeriveLockTime(node)
			if got != 0 {
				t.Fatalf("CSV node returned locktime "+
					"%d, want 0", got)
			}
		})
	})
}

// TestVTXOValidationErrorFormat tests the error formatting.
func TestVTXOValidationErrorFormat(t *testing.T) {
	t.Parallel()

	err := ErrMissingCollab
	require.Contains(t, err.Error(), "MISSING_COLLAB")
	require.Contains(t, err.Error(), "collab leaf")
}

// TestVTXOPolicyDeterminism verifies that NewVTXOPolicy is deterministic.
func TestVTXOPolicyDeterminism(t *testing.T) {
	t.Parallel()

	ownerKey, _ := testutils.CreateKey(1)
	operatorKey, _ := testutils.CreateKey(2)
	exitDelay := uint32(100)

	policy1, err := NewVTXOPolicy(ownerKey, operatorKey, exitDelay)
	require.NoError(t, err)

	policy2, err := NewVTXOPolicy(ownerKey, operatorKey, exitDelay)
	require.NoError(t, err)

	// Output keys should be identical.
	require.Equal(
		t, policy1.OutputKey().SerializeCompressed(),
		policy2.OutputKey().SerializeCompressed(),
	)

	// Root hashes should be identical.
	require.Equal(t, policy1.RootHash, policy2.RootHash)

	// All leaf scripts should be identical.
	for i := range policy1.Leaves {
		require.Equal(
			t, policy1.Leaves[i].Leaf.Script,
			policy2.Leaves[i].Leaf.Script,
		)
	}
}

// TestVTXOPolicyDifferentInputsDifferentOutputs verifies that different inputs
// produce different outputs.
func TestVTXOPolicyDifferentInputsDifferentOutputs(t *testing.T) {
	t.Parallel()

	key1, _ := testutils.CreateKey(1)
	key2, _ := testutils.CreateKey(2)
	key3, _ := testutils.CreateKey(3)

	policy1, err := NewVTXOPolicy(key1, key2, 100)
	require.NoError(t, err)

	t.Run("different owner key", func(t *testing.T) {
		t.Parallel()

		policy2, err := NewVTXOPolicy(key3, key2, 100)
		require.NoError(t, err)
		require.NotEqual(
			t, policy1.OutputKey().SerializeCompressed(),
			policy2.OutputKey().SerializeCompressed(),
		)
	})

	t.Run("different operator key", func(t *testing.T) {
		t.Parallel()

		policy2, err := NewVTXOPolicy(key1, key3, 100)
		require.NoError(t, err)
		require.NotEqual(
			t, policy1.OutputKey().SerializeCompressed(),
			policy2.OutputKey().SerializeCompressed(),
		)
	})

	t.Run("different exit delay", func(t *testing.T) {
		t.Parallel()

		policy2, err := NewVTXOPolicy(key1, key2, 200)
		require.NoError(t, err)
		require.NotEqual(
			t, policy1.OutputKey().SerializeCompressed(),
			policy2.OutputKey().SerializeCompressed(),
		)
	})
}

// TestMultisigNilKey tests that Multisig with nil keys returns error.
func TestMultisigNilKey(t *testing.T) {
	t.Parallel()

	key, _ := testutils.CreateKey(1)

	node := &Multisig{
		Keys: []*btcec.PublicKey{
			key,
			nil,
		},
	}

	_, err := node.Script()
	require.Error(t, err)
	require.Contains(t, err.Error(), "key at index 1 is nil")
}
