package arkscript

import (
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightninglabs/darepo-client/internal/testutils"
	"github.com/stretchr/testify/require"
)

// TestSingleKeyMultisigScript tests the 1-of-1 multisig encoding.
func TestSingleKeyMultisigScript(t *testing.T) {
	t.Parallel()

	key, _ := testutils.CreateKey(1)

	node := &Multisig{Keys: []*btcec.PublicKey{key}}
	script, err := node.Script()
	require.NoError(t, err)

	// Disassemble and verify structure.
	dis, err := txscript.DisasmString(script)
	require.NoError(t, err)
	t.Logf("single-key multisig script: %s", dis)

	// Should be: <32-byte-key> OP_CHECKSIG
	// Script length: 1 (push) + 32 (key) + 1 (OP_CHECKSIG) = 34 bytes
	require.Len(t, script, 34)
	require.Equal(t, byte(txscript.OP_CHECKSIG), script[len(script)-1])
}

// TestSingleKeyMultisigNilKey tests that 1-of-1 multisig validates keys.
func TestSingleKeyMultisigNilKey(t *testing.T) {
	t.Parallel()

	node := &Multisig{Keys: []*btcec.PublicKey{nil}}
	_, err := node.Script()
	require.Error(t, err)
	require.Contains(t, err.Error(), "key at index 0 is nil")
}

// TestMultisigChecksigScript tests the Multisig CHECKSIGVERIFY chain encoding.
func TestMultisigChecksigScript(t *testing.T) {
	t.Parallel()

	key1, _ := testutils.CreateKey(1)
	key2, _ := testutils.CreateKey(2)

	node := &Multisig{
		Keys: []*btcec.PublicKey{key1, key2},
	}
	script, err := node.Script()
	require.NoError(t, err)

	// Disassemble and verify structure.
	dis, err := txscript.DisasmString(script)
	require.NoError(t, err)
	t.Logf("Multisig (checksig) script: %s", dis)

	// Should be: <k1> CHECKSIGVERIFY <k2> CHECKSIG
	// Script length: (1 + 32) + 1 + (1 + 32) + 1 = 68 bytes
	require.Len(t, script, 68)

	// Last opcode should be OP_CHECKSIG.
	require.Equal(t, byte(txscript.OP_CHECKSIG), script[len(script)-1])

	// There should be one CHECKSIGVERIFY in the middle.
	require.Contains(t, dis, "OP_CHECKSIGVERIFY")
}

// TestMultisigEmptyKeys tests that Multisig returns an error for empty keys.
func TestMultisigEmptyKeys(t *testing.T) {
	t.Parallel()

	node := &Multisig{
		Keys: []*btcec.PublicKey{},
	}
	_, err := node.Script()
	require.Error(t, err)
	require.Contains(t, err.Error(), "no keys provided")
}

// TestCSVScript tests the CSV node script encoding.
func TestCSVScript(t *testing.T) {
	t.Parallel()

	key, _ := testutils.CreateKey(1)
	delay := uint32(100)

	node := &CSV{
		Lock:  delay,
		Inner: &Multisig{Keys: []*btcec.PublicKey{key}},
	}
	script, err := node.Script()
	require.NoError(t, err)

	dis, err := txscript.DisasmString(script)
	require.NoError(t, err)
	t.Logf("CSV script: %s", dis)

	// Should be: <key> CHECKSIG <delay> CSV DROP
	require.Contains(t, dis, "OP_CHECKSIG")
	require.Contains(t, dis, "OP_CHECKSEQUENCEVERIFY")
	require.Contains(t, dis, "OP_DROP")

	// Last opcode should be OP_DROP.
	require.Equal(t, byte(txscript.OP_DROP), script[len(script)-1])
}

// TestCSVNilInner tests that CSV returns an error for nil inner node.
func TestCSVNilInner(t *testing.T) {
	t.Parallel()

	node := &CSV{Lock: 100, Inner: nil}
	_, err := node.Script()
	require.Error(t, err)
	require.Contains(t, err.Error(), "inner node is nil")
}

// TestAbsoluteLockTimeCondition tests the opaque absolute locktime prefix.
func TestAbsoluteLockTimeCondition(t *testing.T) {
	t.Parallel()

	locktime := uint32(500000)

	script, err := AbsoluteLockTimeCondition(locktime)
	require.NoError(t, err)

	dis, err := txscript.DisasmString(script)
	require.NoError(t, err)
	t.Logf("absolute locktime condition: %s", dis)

	require.Contains(t, dis, "OP_CHECKLOCKTIMEVERIFY")
	require.Contains(t, dis, "OP_DROP")
	require.Equal(t, byte(txscript.OP_DROP), script[len(script)-1])
}

// TestConditionScript tests the Condition node script encoding.
func TestConditionScript(t *testing.T) {
	t.Parallel()

	key, _ := testutils.CreateKey(1)
	hash := make([]byte, 20)
	hash[0] = 0xde
	hash[19] = 0xad
	predicate, err := Hash160Condition(hash)
	require.NoError(t, err)

	node := &Condition{
		Predicate: predicate,
		Inner:     &Multisig{Keys: []*btcec.PublicKey{key}},
	}
	script, err := node.Script()
	require.NoError(t, err)

	dis, err := txscript.DisasmString(script)
	require.NoError(t, err)
	t.Logf("Condition script: %s", dis)

	// Should be: HASH160 <hash> EQUALVERIFY <key> CHECKSIG
	require.Contains(t, dis, "OP_HASH160")
	require.Contains(t, dis, "OP_EQUALVERIFY")
	require.Contains(t, dis, "OP_CHECKSIG")
}

// TestHash160ConditionInvalidHashLength tests that Hash160Condition validates
// hash length.
func TestHash160ConditionInvalidHashLength(t *testing.T) {
	t.Parallel()

	_, err := Hash160Condition(make([]byte, 32))
	require.Error(t, err)
	require.Contains(t, err.Error(), "requires 20-byte hash")
}

// TestConditionNilInner tests that Condition returns an error for nil inner.
func TestConditionNilInner(t *testing.T) {
	t.Parallel()

	predicate, err := Hash160Condition(make([]byte, 20))
	require.NoError(t, err)

	node := &Condition{
		Predicate: predicate,
		Inner:     nil,
	}
	_, err = node.Script()
	require.Error(t, err)
	require.Contains(t, err.Error(), "inner node is nil")
}

// TestASTMatchesGoldenVectors verifies that the AST produces byte-identical
// scripts to the golden test vectors from the current implementation.
func TestASTMatchesGoldenVectors(t *testing.T) {
	t.Parallel()

	for _, vec := range goldenVTXOVectors {
		t.Run(vec.Name, func(t *testing.T) {
			t.Parallel()

			// Create keys using the same deterministic method.
			ownerKey, _ := testutils.CreateKey(vec.OwnerKeyIndex)
			operatorKey, _ := testutils.CreateKey(
				vec.OperatorKeyIndex,
			)

			// Build the collab leaf using AST:
			// Multisig([owner, operator]).
			collabNode := &Multisig{
				Keys: []*btcec.PublicKey{ownerKey, operatorKey},
			}
			collabScript, err := collabNode.Script()
			require.NoError(t, err)

			// Verify collab script matches golden vector.
			collabHex := hex.EncodeToString(collabScript)
			require.Equal(t, vec.CollabScriptHex, collabHex,
				"AST collab script does not match golden "+
					"vector")

			// Build the timeout/exit leaf using AST:
			// CSV(delay, Multisig([owner])).
			timeoutNode := &CSV{
				Lock: vec.ExitDelay,
				Inner: &Multisig{
					Keys: []*btcec.PublicKey{ownerKey},
				},
			}
			timeoutScript, err := timeoutNode.Script()
			require.NoError(t, err)

			// Verify timeout script matches golden vector.
			timeoutHex := hex.EncodeToString(timeoutScript)
			timeoutMsg := "AST timeout script does not " +
				"match golden vector"
			require.Equal(
				t, vec.TimeoutScriptHex, timeoutHex, timeoutMsg,
			)
		})
	}
}

// TestComposedCSVChecksig specifically tests the CSV(lock, Multisig([key]))
// composition as documented in the RFC.
func TestComposedCSVChecksig(t *testing.T) {
	t.Parallel()

	key, _ := testutils.CreateKey(1)
	delay := uint32(100)

	// Build: CSV(100, Multisig([key]))
	node := &CSV{
		Lock:  delay,
		Inner: &Multisig{Keys: []*btcec.PublicKey{key}},
	}
	script, err := node.Script()
	require.NoError(t, err)

	dis, err := txscript.DisasmString(script)
	require.NoError(t, err)
	t.Logf("CSV(Checksig) composition: %s", dis)

	// Per RFC: <xonly_key> OP_CHECKSIG <lock> OP_CSV OP_DROP
	// Verify the structure matches.
	expectedStructure := "OP_CHECKSIG 64 OP_CHECKSEQUENCEVERIFY OP_DROP"
	require.Contains(t, dis, expectedStructure,
		"CSV(Multisig1) should produce <key> CHECKSIG <lock> CSV DROP")
}

// TestNestedComposition tests deeply nested AST compositions.
func TestNestedComposition(t *testing.T) {
	t.Parallel()

	key, _ := testutils.CreateKey(1)
	hash := make([]byte, 20)

	predicate, err := Hash160Condition(hash)
	require.NoError(t, err)

	locktimePrefix, err := AbsoluteLockTimeCondition(500000)
	require.NoError(t, err)

	// Build: Condition(cltv, Condition(hash160, CSV(100, Multisig([key]))))
	node := &Condition{
		Predicate: locktimePrefix,
		Inner: &Condition{
			Predicate: predicate,
			Inner: &CSV{
				Lock:  100,
				Inner: &Multisig{Keys: []*btcec.PublicKey{key}},
			},
		},
	}

	script, err := node.Script()
	require.NoError(t, err)

	dis, err := txscript.DisasmString(script)
	require.NoError(t, err)
	t.Logf("Nested composition: %s", dis)

	// Should contain all the expected opcodes.
	require.Contains(t, dis, "OP_CHECKLOCKTIMEVERIFY")
	require.Contains(t, dis, "OP_HASH160")
	require.Contains(t, dis, "OP_EQUALVERIFY")
	require.Contains(t, dis, "OP_CHECKSIG")
	require.Contains(t, dis, "OP_CHECKSEQUENCEVERIFY")
}
