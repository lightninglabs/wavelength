package arkscript

import (
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightninglabs/darepo-client/internal/testutils"
	"github.com/stretchr/testify/require"
)

// TestChecksigScript tests the Checksig node script encoding.
func TestChecksigScript(t *testing.T) {
	t.Parallel()

	key, _ := testutils.CreateKey(1)

	node := &Checksig{Key: key}
	script, err := node.Script()
	require.NoError(t, err)

	// Disassemble and verify structure.
	dis, err := txscript.DisasmString(script)
	require.NoError(t, err)
	t.Logf("Checksig script: %s", dis)

	// Should be: <32-byte-key> OP_CHECKSIG
	// Script length: 1 (push) + 32 (key) + 1 (OP_CHECKSIG) = 34 bytes
	require.Len(t, script, 34)
	require.Equal(t, byte(txscript.OP_CHECKSIG), script[len(script)-1])
}

// TestChecksigNilKey tests that Checksig returns an error for nil key.
func TestChecksigNilKey(t *testing.T) {
	t.Parallel()

	node := &Checksig{Key: nil}
	_, err := node.Script()
	require.Error(t, err)
	require.Contains(t, err.Error(), "key is nil")
}

// TestMultisigChecksigScript tests the Multisig CHECKSIGVERIFY chain encoding.
func TestMultisigChecksigScript(t *testing.T) {
	t.Parallel()

	key1, _ := testutils.CreateKey(1)
	key2, _ := testutils.CreateKey(2)

	node := &Multisig{
		Keys: []*btcec.PublicKey{key1, key2},
		Type: MultisigTypeChecksig,
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

// TestMultisigChecksigAddScript tests the Multisig CHECKSIGADD encoding.
func TestMultisigChecksigAddScript(t *testing.T) {
	t.Parallel()

	key1, _ := testutils.CreateKey(1)
	key2, _ := testutils.CreateKey(2)
	key3, _ := testutils.CreateKey(3)

	node := &Multisig{
		Keys: []*btcec.PublicKey{key1, key2, key3},
		Type: MultisigTypeChecksigAdd,
	}
	script, err := node.Script()
	require.NoError(t, err)

	dis, err := txscript.DisasmString(script)
	require.NoError(t, err)
	t.Logf("Multisig (checksigadd) script: %s", dis)

	// Should contain CHECKSIG, CHECKSIGADD (twice), and NUMEQUAL.
	require.Contains(t, dis, "OP_CHECKSIG")
	require.Contains(t, dis, "OP_CHECKSIGADD")
	require.Contains(t, dis, "OP_NUMEQUAL")

	// Should end with: 3 NUMEQUAL
	require.Equal(t, byte(txscript.OP_NUMEQUAL), script[len(script)-1])
}

// TestMultisigEmptyKeys tests that Multisig returns an error for empty keys.
func TestMultisigEmptyKeys(t *testing.T) {
	t.Parallel()

	node := &Multisig{
		Keys: []*btcec.PublicKey{},
		Type: MultisigTypeChecksig,
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
		Inner: &Checksig{Key: key},
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

// TestCLTVScript tests the CLTV node script encoding.
func TestCLTVScript(t *testing.T) {
	t.Parallel()

	key, _ := testutils.CreateKey(1)
	locktime := uint32(500000)

	node := &CLTV{
		Lock:  locktime,
		Inner: &Checksig{Key: key},
	}
	script, err := node.Script()
	require.NoError(t, err)

	dis, err := txscript.DisasmString(script)
	require.NoError(t, err)
	t.Logf("CLTV script: %s", dis)

	// Should be: <lock> CLTV DROP <key> CHECKSIG
	require.Contains(t, dis, "OP_CHECKLOCKTIMEVERIFY")
	require.Contains(t, dis, "OP_DROP")
	require.Contains(t, dis, "OP_CHECKSIG")

	// Last opcode should be OP_CHECKSIG (from inner).
	require.Equal(t, byte(txscript.OP_CHECKSIG), script[len(script)-1])
}

// TestCLTVNilInner tests that CLTV returns an error for nil inner node.
func TestCLTVNilInner(t *testing.T) {
	t.Parallel()

	node := &CLTV{Lock: 500000, Inner: nil}
	_, err := node.Script()
	require.Error(t, err)
	require.Contains(t, err.Error(), "inner node is nil")
}

// TestHashLockHash160Script tests the HashLock node with HASH160.
func TestHashLockHash160Script(t *testing.T) {
	t.Parallel()

	key, _ := testutils.CreateKey(1)
	hash := make([]byte, 20) // 20-byte hash for HASH160
	hash[0] = 0xde
	hash[19] = 0xad

	node := &HashLock{
		Algorithm: HashAlgoHash160,
		Hash:      hash,
		Inner:     &Checksig{Key: key},
	}
	script, err := node.Script()
	require.NoError(t, err)

	dis, err := txscript.DisasmString(script)
	require.NoError(t, err)
	t.Logf("HashLock (HASH160) script: %s", dis)

	// Should be: HASH160 <hash> EQUALVERIFY <key> CHECKSIG
	require.Contains(t, dis, "OP_HASH160")
	require.Contains(t, dis, "OP_EQUALVERIFY")
	require.Contains(t, dis, "OP_CHECKSIG")
}

// TestHashLockSHA256Script tests the HashLock node with SHA256.
func TestHashLockSHA256Script(t *testing.T) {
	t.Parallel()

	key, _ := testutils.CreateKey(1)
	hash := make([]byte, 32) // 32-byte hash for SHA256
	hash[0] = 0xca
	hash[31] = 0xfe

	node := &HashLock{
		Algorithm: HashAlgoSHA256,
		Hash:      hash,
		Inner:     &Checksig{Key: key},
	}
	script, err := node.Script()
	require.NoError(t, err)

	dis, err := txscript.DisasmString(script)
	require.NoError(t, err)
	t.Logf("HashLock (SHA256) script: %s", dis)

	// Should be: SHA256 <hash> EQUALVERIFY <key> CHECKSIG
	require.Contains(t, dis, "OP_SHA256")
	require.Contains(t, dis, "OP_EQUALVERIFY")
	require.Contains(t, dis, "OP_CHECKSIG")
}

// TestHashLockInvalidHashLength tests that HashLock validates hash length.
func TestHashLockInvalidHashLength(t *testing.T) {
	t.Parallel()

	key, _ := testutils.CreateKey(1)

	// Wrong length for HASH160.
	node := &HashLock{
		Algorithm: HashAlgoHash160,
		Hash:      make([]byte, 32), // Should be 20
		Inner:     &Checksig{Key: key},
	}
	_, err := node.Script()
	require.Error(t, err)
	require.Contains(t, err.Error(), "requires 20-byte hash")

	// Wrong length for SHA256.
	node = &HashLock{
		Algorithm: HashAlgoSHA256,
		Hash:      make([]byte, 20), // Should be 32
		Inner:     &Checksig{Key: key},
	}
	_, err = node.Script()
	require.Error(t, err)
	require.Contains(t, err.Error(), "requires 32-byte hash")
}

// TestHashLockNilInner tests that HashLock returns an error for nil inner.
func TestHashLockNilInner(t *testing.T) {
	t.Parallel()

	node := &HashLock{
		Algorithm: HashAlgoHash160,
		Hash:      make([]byte, 20),
		Inner:     nil,
	}
	_, err := node.Script()
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
			operatorKey, _ := testutils.CreateKey(vec.OperatorKeyIndex)

			// Build the collab leaf using AST: Multisig([owner, operator])
			collabNode := &Multisig{
				Keys: []*btcec.PublicKey{ownerKey, operatorKey},
				Type: MultisigTypeChecksig,
			}
			collabScript, err := collabNode.Script()
			require.NoError(t, err)

			// Verify collab script matches golden vector.
			collabHex := hex.EncodeToString(collabScript)
			require.Equal(t, vec.CollabScriptHex, collabHex,
				"AST collab script does not match golden vector")

			// Build the timeout/exit leaf using AST: CSV(delay, Checksig(owner))
			timeoutNode := &CSV{
				Lock:  vec.ExitDelay,
				Inner: &Checksig{Key: ownerKey},
			}
			timeoutScript, err := timeoutNode.Script()
			require.NoError(t, err)

			// Verify timeout script matches golden vector.
			timeoutHex := hex.EncodeToString(timeoutScript)
			require.Equal(t, vec.TimeoutScriptHex, timeoutHex,
				"AST timeout script does not match golden vector")
		})
	}
}

// TestComposedCSVChecksig specifically tests the CSV(lock, Checksig(key))
// composition as documented in the RFC.
func TestComposedCSVChecksig(t *testing.T) {
	t.Parallel()

	key, _ := testutils.CreateKey(1)
	delay := uint32(100)

	// Build: CSV(100, Checksig(key))
	node := &CSV{
		Lock:  delay,
		Inner: &Checksig{Key: key},
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
		"CSV(Checksig) should produce <key> CHECKSIG <lock> CSV DROP")
}

// TestNestedComposition tests deeply nested AST compositions.
func TestNestedComposition(t *testing.T) {
	t.Parallel()

	key, _ := testutils.CreateKey(1)
	hash := make([]byte, 32)

	// Build: CLTV(500000, HashLock(SHA256, hash, CSV(100, Checksig(key))))
	node := &CLTV{
		Lock: 500000,
		Inner: &HashLock{
			Algorithm: HashAlgoSHA256,
			Hash:      hash,
			Inner: &CSV{
				Lock:  100,
				Inner: &Checksig{Key: key},
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
	require.Contains(t, dis, "OP_SHA256")
	require.Contains(t, dis, "OP_EQUALVERIFY")
	require.Contains(t, dis, "OP_CHECKSIG")
	require.Contains(t, dis, "OP_CHECKSEQUENCEVERIFY")
}
