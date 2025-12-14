package assets

import (
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/txscript"
	"github.com/stretchr/testify/require"
)

// testKey generates a deterministic test key.
func testKey(t *testing.T, seed byte) *btcec.PublicKey {
	t.Helper()

	var privKeyBytes [32]byte
	for i := range privKeyBytes {
		privKeyBytes[i] = seed
	}

	privKey, _ := btcec.PrivKeyFromBytes(privKeyBytes[:])

	return privKey.PubKey()
}

// TestCSVClosureScript verifies the CSVClosure generates the expected script.
func TestCSVClosureScript(t *testing.T) {
	key := testKey(t, 0x01)
	delay := uint32(144) // ~1 day in blocks

	closure := &CSVClosure{
		Key:   key,
		Delay: delay,
	}

	script, err := closure.Script()
	require.NoError(t, err)
	require.NotEmpty(t, script)

	// Disassemble and verify structure.
	disasm, err := txscript.DisasmString(script)
	require.NoError(t, err)

	// Should contain: <pubkey> OP_CHECKSIGVERIFY <delay>
	// OP_CHECKSEQUENCEVERIFY
	require.Contains(t, disasm, "OP_CHECKSIGVERIFY")
	require.Contains(t, disasm, "OP_CHECKSEQUENCEVERIFY")

	// Verify the key is in the script.
	keyHex := hex.EncodeToString(schnorr.SerializePubKey(key))
	require.Contains(t, disasm, keyHex)

	// Verify delay value is present (144 = 0x90 in minimally encoded form).
	require.Contains(t, disasm, "9000")
}

// TestCSVClosureLeaf verifies CSVClosure creates a valid taproot leaf.
func TestCSVClosureLeaf(t *testing.T) {
	key := testKey(t, 0x02)

	closure := &CSVClosure{
		Key:   key,
		Delay: 10,
	}

	leaf := closure.Leaf()

	require.Equal(t, txscript.BaseLeafVersion, leaf.LeafVersion)
	require.NotEmpty(t, leaf.Script)

	// The leaf script should match what Script() returns.
	expectedScript, err := closure.Script()
	require.NoError(t, err)
	require.Equal(t, expectedScript, leaf.Script)
}

// TestCSVClosureWitness verifies CSVClosure builds correct witness stack.
func TestCSVClosureWitness(t *testing.T) {
	key := testKey(t, 0x03)

	closure := &CSVClosure{
		Key:   key,
		Delay: 10,
	}

	sc := closure.ScriptClosure()
	require.Equal(t, "csv", sc.ID)

	// Create mock signature and control block.
	mockSig := []byte("mock_signature_64_bytes_padded_for_schnorr_xxxxxxx")
	mockControlBlock := []byte("control_block")

	keyHex := hex.EncodeToString(schnorr.SerializePubKey(key))
	sigs := map[string][]byte{
		keyHex: mockSig,
	}

	witness, err := sc.WitnessFunc(mockControlBlock, sigs)
	require.NoError(t, err)

	// Witness should be: [sig, script, control_block]
	require.Len(t, witness, 3)
	require.Equal(t, mockSig, witness[0])
	require.Equal(t, mockControlBlock, witness[2])

	// Missing signature should error.
	_, err = sc.WitnessFunc(mockControlBlock, map[string][]byte{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing csv signature")
}

// TestVTXOTimeoutClosureScript verifies VTXOTimeoutClosure script structure.
func TestVTXOTimeoutClosureScript(t *testing.T) {
	key := testKey(t, 0x04)
	delay := uint32(288)

	closure := &VTXOTimeoutClosure{
		Key:   key,
		Delay: delay,
	}

	script, err := closure.Script()
	require.NoError(t, err)

	disasm, err := txscript.DisasmString(script)
	require.NoError(t, err)

	// Should contain: <key> OP_CHECKSIG <delay> OP_CHECKSEQUENCEVERIFY
	// OP_DROP
	require.Contains(t, disasm, "OP_CHECKSIG")
	require.Contains(t, disasm, "OP_CHECKSEQUENCEVERIFY")
	require.Contains(t, disasm, "OP_DROP")

	// Note: Unlike CSVClosure, this uses CHECKSIG not CHECKSIGVERIFY.
	require.NotContains(t, disasm, "OP_CHECKSIGVERIFY")
}

// TestCollabMultisigClosureScript verifies collaborative multisig script.
func TestCollabMultisigClosureScript(t *testing.T) {
	ownerKey := testKey(t, 0x05)
	cosignerKey := testKey(t, 0x06)

	closure := &CollabMultisigClosure{
		OwnerKey:    ownerKey,
		CosignerKey: cosignerKey,
	}

	script, err := closure.Script()
	require.NoError(t, err)

	disasm, err := txscript.DisasmString(script)
	require.NoError(t, err)

	// Should contain: <owner> OP_CHECKSIGVERIFY <cosigner> OP_CHECKSIG
	require.Contains(t, disasm, "OP_CHECKSIGVERIFY")
	require.Contains(t, disasm, "OP_CHECKSIG")

	// Both keys should be in the script.
	ownerKeyHex := hex.EncodeToString(
		schnorr.SerializePubKey(ownerKey),
	)
	cosignerKeyHex := hex.EncodeToString(
		schnorr.SerializePubKey(cosignerKey),
	)
	require.Contains(t, disasm, ownerKeyHex)
	require.Contains(t, disasm, cosignerKeyHex)
}

// TestCollabMultisigClosureWitness verifies witness stack order.
func TestCollabMultisigClosureWitness(t *testing.T) {
	ownerKey := testKey(t, 0x07)
	cosignerKey := testKey(t, 0x08)

	closure := &CollabMultisigClosure{
		OwnerKey:    ownerKey,
		CosignerKey: cosignerKey,
	}

	sc := closure.ScriptClosure()
	require.Equal(t, "collab_multisig", sc.ID)

	ownerSig := []byte("owner_signature")
	cosignerSig := []byte("cosigner_signature")
	controlBlock := []byte("control")

	ownerKeyHex := hex.EncodeToString(
		schnorr.SerializePubKey(ownerKey),
	)
	cosignerKeyHex := hex.EncodeToString(
		schnorr.SerializePubKey(cosignerKey),
	)

	sigs := map[string][]byte{
		ownerKeyHex:    ownerSig,
		cosignerKeyHex: cosignerSig,
	}

	witness, err := sc.WitnessFunc(controlBlock, sigs)
	require.NoError(t, err)

	// Witness order: [cosigner_sig, owner_sig, script, control_block]
	// Stack is LIFO, so owner_sig is checked first by CHECKSIGVERIFY.
	require.Len(t, witness, 4)
	require.Equal(t, cosignerSig, witness[0])
	require.Equal(t, ownerSig, witness[1])
	require.Equal(t, controlBlock, witness[3])
}

// TestCheckSigAddClosureScript verifies CHECKSIGADD 2-of-2 script.
func TestCheckSigAddClosureScript(t *testing.T) {
	key1 := testKey(t, 0x09)
	key2 := testKey(t, 0x0a)

	closure := &CheckSigAddClosure{
		Key1: key1,
		Key2: key2,
	}

	script, err := closure.Script()
	require.NoError(t, err)

	disasm, err := txscript.DisasmString(script)
	require.NoError(t, err)

	// Should contain: <key1> OP_CHECKSIG <key2> OP_CHECKSIGADD 2 OP_EQUAL
	require.Contains(t, disasm, "OP_CHECKSIG")
	require.Contains(t, disasm, "OP_CHECKSIGADD")
	require.Contains(t, disasm, "OP_EQUAL")
	require.Contains(t, disasm, "2") // threshold
}

// TestCheckSigAddClosureWitness verifies CHECKSIGADD witness order.
func TestCheckSigAddClosureWitness(t *testing.T) {
	key1 := testKey(t, 0x0b)
	key2 := testKey(t, 0x0c)

	closure := &CheckSigAddClosure{
		Key1: key1,
		Key2: key2,
	}

	sc := closure.ScriptClosure()
	require.Equal(t, "coop_multisig", sc.ID)

	sig1 := []byte("sig1")
	sig2 := []byte("sig2")
	controlBlock := []byte("ctrl")

	key1Hex := hex.EncodeToString(schnorr.SerializePubKey(key1))
	key2Hex := hex.EncodeToString(schnorr.SerializePubKey(key2))

	sigs := map[string][]byte{
		key1Hex: sig1,
		key2Hex: sig2,
	}

	witness, err := sc.WitnessFunc(controlBlock, sigs)
	require.NoError(t, err)

	// Witness order: [sig2, sig1, script, control_block]
	// CHECKSIGADD processes in reverse stack order.
	require.Len(t, witness, 4)
	require.Equal(t, sig2, witness[0])
	require.Equal(t, sig1, witness[1])
}

// TestClosureScriptDeterminism verifies scripts are deterministic.
func TestClosureScriptDeterminism(t *testing.T) {
	key := testKey(t, 0x0d)

	closure := &CSVClosure{
		Key:   key,
		Delay: 100,
	}

	script1, err := closure.Script()
	require.NoError(t, err)

	script2, err := closure.Script()
	require.NoError(t, err)

	require.Equal(t, script1, script2)
}
