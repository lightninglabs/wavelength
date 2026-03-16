package arkscript

import (
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightninglabs/darepo-client/internal/testutils"
	"github.com/stretchr/testify/require"
)

// TestNewVTXOPolicyMatchesGoldenVectors verifies that NewVTXOPolicy produces
// byte-identical output to the golden test vectors.
func TestNewVTXOPolicyMatchesGoldenVectors(t *testing.T) {
	t.Parallel()

	for _, vec := range goldenVTXOVectors {
		t.Run(vec.Name, func(t *testing.T) {
			t.Parallel()

			ownerKey, _ := testutils.CreateKey(vec.OwnerKeyIndex)
			operatorKey, _ := testutils.CreateKey(vec.OperatorKeyIndex)

			policy, err := NewVTXOPolicy(
				ownerKey, operatorKey, vec.ExitDelay,
			)
			require.NoError(t, err)

			// Verify output key matches.
			outputKey := policy.OutputKey()
			outputKeyHex := hex.EncodeToString(
				outputKey.SerializeCompressed(),
			)
			require.Equal(t, vec.OutputKeyHex, outputKeyHex,
				"output key mismatch")

			// Verify root hash matches.
			rootHashHex := hex.EncodeToString(policy.RootHash)
			require.Equal(t, vec.RootHashHex, rootHashHex,
				"root hash mismatch")

			// Verify collab script matches.
			collabScriptHex := hex.EncodeToString(
				policy.Leaves[0].Leaf.Script,
			)
			require.Equal(t, vec.CollabScriptHex, collabScriptHex,
				"collab script mismatch")

			// Verify timeout script matches.
			timeoutScriptHex := hex.EncodeToString(
				policy.Leaves[1].Leaf.Script,
			)
			require.Equal(t, vec.TimeoutScriptHex, timeoutScriptHex,
				"timeout script mismatch")

			// Verify collab control block matches.
			collabInfo, err := policy.CollabSpendInfo()
			require.NoError(t, err)
			collabControlHex := hex.EncodeToString(
				collabInfo.ControlBlock,
			)
			require.Equal(t, vec.CollabControlHex, collabControlHex,
				"collab control block mismatch")

			// Verify timeout control block matches.
			exitInfo, err := policy.ExitSpendInfo()
			require.NoError(t, err)
			exitControlHex := hex.EncodeToString(
				exitInfo.ControlBlock,
			)
			require.Equal(t, vec.TimeoutControlHex, exitControlHex,
				"timeout control block mismatch")
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
	require.Equal(t,
		hex.EncodeToString(taprootKey.SerializeCompressed()),
		hex.EncodeToString(policy.OutputKey().SerializeCompressed()),
		"output keys should match")

	// Verify root hashes match.
	require.Equal(t, tapscript.RootHash, policy.RootHash,
		"root hashes should match")

	// Verify leaf scripts match.
	require.Equal(t,
		tapscript.Leaves[0].Script, policy.Leaves[0].Leaf.Script,
		"collab scripts should match")
	require.Equal(t,
		tapscript.Leaves[1].Script, policy.Leaves[1].Leaf.Script,
		"exit scripts should match")

	// Verify VTXOTapKey matches policy OutputKey.
	tapKey, err := VTXOTapKey(ownerKey, operatorKey, exitDelay)
	require.NoError(t, err)
	require.Equal(t,
		hex.EncodeToString(tapKey.SerializeCompressed()),
		hex.EncodeToString(policy.OutputKey().SerializeCompressed()),
		"VTXOTapKey should match policy OutputKey")
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

		info, err := policy.CollabSpendInfo()
		require.NoError(t, err)
		require.Equal(t, uint32(0xffffffff), info.RequiredSequence)
		require.Equal(t, uint32(0), info.RequiredLockTime)
	})

	t.Run("exit path requires CSV delay", func(t *testing.T) {
		t.Parallel()

		info, err := policy.ExitSpendInfo()
		require.NoError(t, err)
		require.Equal(t, exitDelay, info.RequiredSequence)
		require.Equal(t, uint32(0), info.RequiredLockTime)
	})
}

// TestDeriveSequence tests the sequence derivation function.
func TestDeriveSequence(t *testing.T) {
	t.Parallel()

	key, _ := testutils.CreateKey(1)

	t.Run("simple checksig returns max sequence", func(t *testing.T) {
		t.Parallel()

		node := &Checksig{Key: key}
		require.Equal(t, uint32(0xffffffff), DeriveSequence(node))
	})

	t.Run("CSV returns lock value", func(t *testing.T) {
		t.Parallel()

		node := &CSV{Lock: 100, Inner: &Checksig{Key: key}}
		require.Equal(t, uint32(100), DeriveSequence(node))
	})

	t.Run("CLTV returns non-final sequence", func(t *testing.T) {
		t.Parallel()

		node := &CLTV{Lock: 500000, Inner: &Checksig{Key: key}}
		require.Equal(t, uint32(0xfffffffe), DeriveSequence(node))
	})

	t.Run("CSV nested in CLTV returns CSV lock", func(t *testing.T) {
		t.Parallel()

		node := &CLTV{
			Lock: 500000,
			Inner: &CSV{
				Lock:  100,
				Inner: &Checksig{Key: key},
			},
		}
		require.Equal(t, uint32(100), DeriveSequence(node))
	})

	t.Run("hashlock preserves inner timelock", func(t *testing.T) {
		t.Parallel()

		node := &HashLock{
			Algorithm: HashAlgoSHA256,
			Hash:      make([]byte, 32),
			Inner: &CSV{
				Lock:  200,
				Inner: &Checksig{Key: key},
			},
		}
		require.Equal(t, uint32(200), DeriveSequence(node))
	})
}

// TestDeriveLockTime tests the locktime derivation function.
func TestDeriveLockTime(t *testing.T) {
	t.Parallel()

	key, _ := testutils.CreateKey(1)

	t.Run("simple checksig returns zero", func(t *testing.T) {
		t.Parallel()

		node := &Checksig{Key: key}
		require.Equal(t, uint32(0), DeriveLockTime(node))
	})

	t.Run("CSV returns zero", func(t *testing.T) {
		t.Parallel()

		node := &CSV{Lock: 100, Inner: &Checksig{Key: key}}
		require.Equal(t, uint32(0), DeriveLockTime(node))
	})

	t.Run("CLTV returns lock value", func(t *testing.T) {
		t.Parallel()

		node := &CLTV{Lock: 500000, Inner: &Checksig{Key: key}}
		require.Equal(t, uint32(500000), DeriveLockTime(node))
	})

	t.Run("nested CLTV returns lock value", func(t *testing.T) {
		t.Parallel()

		node := &HashLock{
			Algorithm: HashAlgoSHA256,
			Hash:      make([]byte, 32),
			Inner: &CLTV{
				Lock:  600000,
				Inner: &Checksig{Key: key},
			},
		}
		require.Equal(t, uint32(600000), DeriveLockTime(node))
	})
}

// TestValidateVTXOLeaves tests VTXO policy invariant validation.
func TestValidateVTXOLeaves(t *testing.T) {
	t.Parallel()

	collabScript := []byte{0x01}
	exitScript := []byte{0x02}

	t.Run("valid VTXO policy", func(t *testing.T) {
		t.Parallel()

		leaves := []PolicyLeaf{
			{
				Role: LeafRoleCollab,
				Leaf: txscript.NewBaseTapLeaf(collabScript),
			},
			{
				Role: LeafRoleExit,
				Leaf: txscript.NewBaseTapLeaf(exitScript),
			},
		}

		err := ValidateVTXOLeaves(leaves)
		require.NoError(t, err)
	})

	t.Run("missing collab leaf", func(t *testing.T) {
		t.Parallel()

		leaves := []PolicyLeaf{
			{
				Role: LeafRoleExit,
				Leaf: txscript.NewBaseTapLeaf(exitScript),
			},
		}

		err := ValidateVTXOLeaves(leaves)
		require.ErrorIs(t, err, ErrMissingCollab)
	})

	t.Run("missing exit leaf", func(t *testing.T) {
		t.Parallel()

		leaves := []PolicyLeaf{
			{
				Role: LeafRoleCollab,
				Leaf: txscript.NewBaseTapLeaf(collabScript),
			},
		}

		err := ValidateVTXOLeaves(leaves)
		require.ErrorIs(t, err, ErrMissingExit)
	})

	t.Run("multiple collab leaves is valid", func(t *testing.T) {
		t.Parallel()

		leaves := []PolicyLeaf{
			{
				Role: LeafRoleCollab,
				Leaf: txscript.NewBaseTapLeaf(collabScript),
			},
			{
				Role: LeafRoleCollab,
				Leaf: txscript.NewBaseTapLeaf([]byte{0x03}),
			},
			{
				Role: LeafRoleExit,
				Leaf: txscript.NewBaseTapLeaf(exitScript),
			},
		}

		err := ValidateVTXOLeaves(leaves)
		require.NoError(t, err)
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
	require.Equal(t,
		policy1.OutputKey().SerializeCompressed(),
		policy2.OutputKey().SerializeCompressed())

	// Root hashes should be identical.
	require.Equal(t, policy1.RootHash, policy2.RootHash)

	// All leaf scripts should be identical.
	for i := range policy1.Leaves {
		require.Equal(t,
			policy1.Leaves[i].Leaf.Script,
			policy2.Leaves[i].Leaf.Script)
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
		require.NotEqual(t,
			policy1.OutputKey().SerializeCompressed(),
			policy2.OutputKey().SerializeCompressed())
	})

	t.Run("different operator key", func(t *testing.T) {
		t.Parallel()

		policy2, err := NewVTXOPolicy(key1, key3, 100)
		require.NoError(t, err)
		require.NotEqual(t,
			policy1.OutputKey().SerializeCompressed(),
			policy2.OutputKey().SerializeCompressed())
	})

	t.Run("different exit delay", func(t *testing.T) {
		t.Parallel()

		policy2, err := NewVTXOPolicy(key1, key2, 200)
		require.NoError(t, err)
		require.NotEqual(t,
			policy1.OutputKey().SerializeCompressed(),
			policy2.OutputKey().SerializeCompressed())
	})
}

// TestMultisigNilKey tests that Multisig with nil keys returns error.
func TestMultisigNilKey(t *testing.T) {
	t.Parallel()

	key, _ := testutils.CreateKey(1)

	node := &Multisig{
		Keys: []*btcec.PublicKey{key, nil},
		Type: MultisigTypeChecksig,
	}

	_, err := node.Script()
	require.Error(t, err)
	require.Contains(t, err.Error(), "key at index 1 is nil")
}
