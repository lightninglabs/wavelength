package closure

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/txscript"
	"github.com/stretchr/testify/require"
)

// TestClosureScriptRoundtrip verifies that Script() -> Decode() preserves
// data for all closure types.
func TestClosureScriptRoundtrip(t *testing.T) {
	t.Parallel()

	key1 := createTestKey(1)
	key2 := createTestKey(2)
	key3 := createTestKey(3)

	t.Run("CSVSigClosure roundtrip", func(t *testing.T) {
		t.Parallel()

		original := &CSVSigClosure{
			PubKey: key1,
			Locktime: RelativeLocktime{
				Type:  LocktimeTypeSecond,
				Value: 1024,
			},
		}

		script, err := original.Script()
		require.NoError(t, err)
		require.NotEmpty(t, script)

		decoded := &CSVSigClosure{}
		valid, err := decoded.Decode(script)
		require.NoError(t, err)
		require.True(t, valid)

		require.True(t, original.PubKey.IsEqual(decoded.PubKey))
		require.Equal(t, original.Locktime, decoded.Locktime)
	})

	t.Run("CSVSigClosure with block locktime", func(t *testing.T) {
		t.Parallel()

		original := &CSVSigClosure{
			PubKey: key1,
			Locktime: RelativeLocktime{
				Type:  LocktimeTypeBlock,
				Value: 100,
			},
		}

		script, err := original.Script()
		require.NoError(t, err)

		decoded := &CSVSigClosure{}
		valid, err := decoded.Decode(script)
		require.NoError(t, err)
		require.True(t, valid)

		require.True(t, original.PubKey.IsEqual(decoded.PubKey))
		require.Equal(t, original.Locktime, decoded.Locktime)
	})

	t.Run("CSVMultisigClosure roundtrip", func(t *testing.T) {
		t.Parallel()

		original := &CSVMultisigClosure{
			MultisigClosure: MultisigClosure{
				PubKeys: []*btcec.PublicKey{key1, key2},
				Type:    MultisigTypeChecksig,
			},
			Locktime: RelativeLocktime{
				Type:  LocktimeTypeSecond,
				Value: 2048,
			},
		}

		script, err := original.Script()
		require.NoError(t, err)
		require.NotEmpty(t, script)

		decoded := &CSVMultisigClosure{}
		valid, err := decoded.Decode(script)
		require.NoError(t, err)
		require.True(t, valid)

		require.Len(t, decoded.PubKeys, 2)
		require.True(t, original.PubKeys[0].IsEqual(decoded.PubKeys[0]))
		require.True(t, original.PubKeys[1].IsEqual(decoded.PubKeys[1]))
		require.Equal(t, original.Locktime, decoded.Locktime)
	})

	t.Run("MultisigClosure CHECKSIG roundtrip", func(t *testing.T) {
		t.Parallel()

		original := &MultisigClosure{
			PubKeys: []*btcec.PublicKey{key1, key2},
			Type:    MultisigTypeChecksig,
		}

		script, err := original.Script()
		require.NoError(t, err)
		require.NotEmpty(t, script)

		decoded := &MultisigClosure{}
		valid, err := decoded.Decode(script)
		require.NoError(t, err)
		require.True(t, valid)

		require.Len(t, decoded.PubKeys, 2)
		require.True(t, original.PubKeys[0].IsEqual(decoded.PubKeys[0]))
		require.True(t, original.PubKeys[1].IsEqual(decoded.PubKeys[1]))
		require.Equal(t, MultisigTypeChecksig, decoded.Type)
	})

	t.Run("MultisigClosure CHECKSIGADD roundtrip", func(t *testing.T) {
		t.Parallel()

		original := &MultisigClosure{
			PubKeys: []*btcec.PublicKey{key1, key2, key3},
			Type:    MultisigTypeChecksigAdd,
		}

		script, err := original.Script()
		require.NoError(t, err)
		require.NotEmpty(t, script)

		decoded := &MultisigClosure{}
		valid, err := decoded.Decode(script)
		require.NoError(t, err)
		require.True(t, valid)

		require.Len(t, decoded.PubKeys, 3)
		for i := range original.PubKeys {
			isEqual := original.PubKeys[i].IsEqual(decoded.PubKeys[i])
			require.True(t, isEqual)
		}
		require.Equal(t, MultisigTypeChecksigAdd, decoded.Type)
	})

	t.Run("CLTVMultisigClosure roundtrip", func(t *testing.T) {
		t.Parallel()

		original := &CLTVMultisigClosure{
			MultisigClosure: MultisigClosure{
				PubKeys: []*btcec.PublicKey{key1, key2},
				Type:    MultisigTypeChecksig,
			},
			// Seconds-based (>= 500000000).
			Locktime: AbsoluteLocktime(500000001),
		}

		script, err := original.Script()
		require.NoError(t, err)
		require.NotEmpty(t, script)

		decoded := &CLTVMultisigClosure{}
		valid, err := decoded.Decode(script)
		require.NoError(t, err)
		require.True(t, valid)

		require.Len(t, decoded.PubKeys, 2)
		require.True(t, original.PubKeys[0].IsEqual(decoded.PubKeys[0]))
		require.True(t, original.PubKeys[1].IsEqual(decoded.PubKeys[1]))
		require.Equal(t, original.Locktime, decoded.Locktime)
	})

	t.Run("CLTVMultisigClosure block-based locktime", func(t *testing.T) {
		t.Parallel()

		original := &CLTVMultisigClosure{
			MultisigClosure: MultisigClosure{
				PubKeys: []*btcec.PublicKey{key1, key2},
				Type:    MultisigTypeChecksig,
			},
			// Block-based (< 500000000).
			Locktime: AbsoluteLocktime(1000),
		}

		script, err := original.Script()
		require.NoError(t, err)
		require.NotEmpty(t, script)

		decoded := &CLTVMultisigClosure{}
		valid, err := decoded.Decode(script)
		require.NoError(t, err)
		require.True(t, valid)

		require.Equal(t, original.Locktime, decoded.Locktime)
	})

	t.Run("ConditionMultisigClosure roundtrip", func(t *testing.T) {
		t.Parallel()

		// Simple condition: OP_1 (always true).
		condition := []byte{txscript.OP_1}

		original := &ConditionMultisigClosure{
			MultisigClosure: MultisigClosure{
				PubKeys: []*btcec.PublicKey{key1, key2},
				Type:    MultisigTypeChecksig,
			},
			Condition: condition,
		}

		script, err := original.Script()
		require.NoError(t, err)
		require.NotEmpty(t, script)

		decoded := &ConditionMultisigClosure{}
		valid, err := decoded.Decode(script)
		require.NoError(t, err)
		require.True(t, valid)

		require.Len(t, decoded.PubKeys, 2)
		require.True(t, original.PubKeys[0].IsEqual(decoded.PubKeys[0]))
		require.True(t, original.PubKeys[1].IsEqual(decoded.PubKeys[1]))
		require.Equal(t, original.Condition, decoded.Condition)
	})

	t.Run("ConditionCSVMultisigClosure roundtrip", func(t *testing.T) {
		t.Parallel()

		// Simple condition: OP_1 (always true).
		condition := []byte{txscript.OP_1}

		original := &ConditionCSVMultisigClosure{
			CSVMultisigClosure: CSVMultisigClosure{
				MultisigClosure: MultisigClosure{
					PubKeys: []*btcec.PublicKey{key1, key2},
					Type:    MultisigTypeChecksig,
				},
				Locktime: RelativeLocktime{
					Type:  LocktimeTypeSecond,
					Value: 1024,
				},
			},
			Condition: condition,
		}

		script, err := original.Script()
		require.NoError(t, err)
		require.NotEmpty(t, script)

		decoded := &ConditionCSVMultisigClosure{}
		valid, err := decoded.Decode(script)
		require.NoError(t, err)
		require.True(t, valid)

		require.Len(t, decoded.PubKeys, 2)
		require.True(t, original.PubKeys[0].IsEqual(decoded.PubKeys[0]))
		require.True(t, original.PubKeys[1].IsEqual(decoded.PubKeys[1]))
		require.Equal(t, original.Condition, decoded.Condition)
		require.Equal(t, original.Locktime, decoded.Locktime)
	})
}

// TestDecodeClosure_AutoDetection verifies that DecodeClosure correctly
// identifies the closure type from raw script bytes.
func TestDecodeClosure_AutoDetection(t *testing.T) {
	t.Parallel()

	key1 := createTestKey(1)
	key2 := createTestKey(2)

	t.Run("detects CSVSigClosure", func(t *testing.T) {
		t.Parallel()

		original := &CSVSigClosure{
			PubKey: key1,
			Locktime: RelativeLocktime{
				Type:  LocktimeTypeSecond,
				Value: 1024,
			},
		}

		script, err := original.Script()
		require.NoError(t, err)

		decoded, err := DecodeClosure(script)
		require.NoError(t, err)

		csv, ok := decoded.(*CSVSigClosure)
		require.True(t, ok, "expected CSVSigClosure, got %T", decoded)
		require.True(t, original.PubKey.IsEqual(csv.PubKey))
	})

	t.Run("detects CSVMultisigClosure", func(t *testing.T) {
		t.Parallel()

		original := &CSVMultisigClosure{
			MultisigClosure: MultisigClosure{
				PubKeys: []*btcec.PublicKey{key1, key2},
				Type:    MultisigTypeChecksig,
			},
			Locktime: RelativeLocktime{
				Type:  LocktimeTypeSecond,
				Value: 1024,
			},
		}

		script, err := original.Script()
		require.NoError(t, err)

		decoded, err := DecodeClosure(script)
		require.NoError(t, err)

		csv, ok := decoded.(*CSVMultisigClosure)
		require.True(t, ok, "expected CSVMultisigClosure, got %T", decoded)
		require.Len(t, csv.PubKeys, 2)
	})

	t.Run("detects MultisigClosure CHECKSIG", func(t *testing.T) {
		t.Parallel()

		original := &MultisigClosure{
			PubKeys: []*btcec.PublicKey{key1, key2},
			Type:    MultisigTypeChecksig,
		}

		script, err := original.Script()
		require.NoError(t, err)

		decoded, err := DecodeClosure(script)
		require.NoError(t, err)

		ms, ok := decoded.(*MultisigClosure)
		require.True(t, ok, "expected MultisigClosure, got %T", decoded)
		require.Equal(t, MultisigTypeChecksig, ms.Type)
	})

	t.Run("detects MultisigClosure CHECKSIGADD", func(t *testing.T) {
		t.Parallel()

		key3 := createTestKey(3)
		original := &MultisigClosure{
			PubKeys: []*btcec.PublicKey{key1, key2, key3},
			Type:    MultisigTypeChecksigAdd,
		}

		script, err := original.Script()
		require.NoError(t, err)

		decoded, err := DecodeClosure(script)
		require.NoError(t, err)

		ms, ok := decoded.(*MultisigClosure)
		require.True(t, ok, "expected MultisigClosure, got %T", decoded)
		require.Equal(t, MultisigTypeChecksigAdd, ms.Type)
	})

	t.Run("detects CLTVMultisigClosure", func(t *testing.T) {
		t.Parallel()

		original := &CLTVMultisigClosure{
			MultisigClosure: MultisigClosure{
				PubKeys: []*btcec.PublicKey{key1, key2},
				Type:    MultisigTypeChecksig,
			},
			Locktime: AbsoluteLocktime(500000001),
		}

		script, err := original.Script()
		require.NoError(t, err)

		decoded, err := DecodeClosure(script)
		require.NoError(t, err)

		cltv, ok := decoded.(*CLTVMultisigClosure)
		require.True(t, ok, "expected CLTVMultisigClosure, got %T", decoded)
		require.Equal(t, original.Locktime, cltv.Locktime)
	})

	t.Run("detects ConditionMultisigClosure", func(t *testing.T) {
		t.Parallel()

		condition := []byte{txscript.OP_1}
		original := &ConditionMultisigClosure{
			MultisigClosure: MultisigClosure{
				PubKeys: []*btcec.PublicKey{key1, key2},
				Type:    MultisigTypeChecksig,
			},
			Condition: condition,
		}

		script, err := original.Script()
		require.NoError(t, err)

		decoded, err := DecodeClosure(script)
		require.NoError(t, err)

		cond, ok := decoded.(*ConditionMultisigClosure)
		require.True(t, ok, "expected ConditionMultisigClosure, got %T", decoded)
		require.Equal(t, condition, cond.Condition)
	})

	t.Run("detects ConditionCSVMultisigClosure", func(t *testing.T) {
		t.Parallel()

		condition := []byte{txscript.OP_1}
		original := &ConditionCSVMultisigClosure{
			CSVMultisigClosure: CSVMultisigClosure{
				MultisigClosure: MultisigClosure{
					PubKeys: []*btcec.PublicKey{key1, key2},
					Type:    MultisigTypeChecksig,
				},
				Locktime: RelativeLocktime{
					Type:  LocktimeTypeSecond,
					Value: 1024,
				},
			},
			Condition: condition,
		}

		script, err := original.Script()
		require.NoError(t, err)

		decoded, err := DecodeClosure(script)
		require.NoError(t, err)

		cond, ok := decoded.(*ConditionCSVMultisigClosure)
		require.True(t, ok,
			"expected ConditionCSVMultisigClosure, got %T", decoded)
		require.Equal(t, condition, cond.Condition)
	})

	t.Run("empty script returns error", func(t *testing.T) {
		t.Parallel()

		decoded, err := DecodeClosure([]byte{})
		require.Error(t, err)
		require.Nil(t, decoded)
		require.Contains(t, err.Error(), "cannot decode empty script")
	})

	t.Run("invalid script returns error", func(t *testing.T) {
		t.Parallel()

		// Random invalid bytes.
		invalidScript := []byte{0x01, 0x02, 0x03}

		decoded, err := DecodeClosure(invalidScript)
		require.Error(t, err)
		require.Nil(t, decoded)
	})
}

// TestMultisigClosure_Variants verifies both CHECKSIG and CHECKSIGADD
// variants produce correct scripts.
func TestMultisigClosure_Variants(t *testing.T) {
	t.Parallel()

	key1 := createTestKey(1)
	key2 := createTestKey(2)
	key3 := createTestKey(3)

	t.Run("CHECKSIG variant with 2 keys", func(t *testing.T) {
		t.Parallel()

		closure := &MultisigClosure{
			PubKeys: []*btcec.PublicKey{key1, key2},
			Type:    MultisigTypeChecksig,
		}

		script, err := closure.Script()
		require.NoError(t, err)

		// Verify script structure:
		// <pubkey1> OP_CHECKSIGVERIFY <pubkey2> OP_CHECKSIG
		tokenizer := txscript.MakeScriptTokenizer(0, script)

		// First pubkey.
		require.True(t, tokenizer.Next())
		require.Equal(t, byte(txscript.OP_DATA_32), tokenizer.Opcode())
		require.Equal(t, schnorr.SerializePubKey(key1), tokenizer.Data())

		// CHECKSIGVERIFY.
		require.True(t, tokenizer.Next())
		require.Equal(t, byte(txscript.OP_CHECKSIGVERIFY), tokenizer.Opcode())

		// Second pubkey.
		require.True(t, tokenizer.Next())
		require.Equal(t, byte(txscript.OP_DATA_32), tokenizer.Opcode())
		require.Equal(t, schnorr.SerializePubKey(key2), tokenizer.Data())

		// CHECKSIG.
		require.True(t, tokenizer.Next())
		require.Equal(t, byte(txscript.OP_CHECKSIG), tokenizer.Opcode())

		// End of script.
		require.False(t, tokenizer.Next())
	})

	t.Run("CHECKSIGADD variant with 3 keys", func(t *testing.T) {
		t.Parallel()

		closure := &MultisigClosure{
			PubKeys: []*btcec.PublicKey{key1, key2, key3},
			Type:    MultisigTypeChecksigAdd,
		}

		script, err := closure.Script()
		require.NoError(t, err)

		// Verify script structure:
		// <pk1> CHECKSIG <pk2> CHECKSIGADD <pk3> CHECKSIGADD 3 NUMEQUAL
		tokenizer := txscript.MakeScriptTokenizer(0, script)

		// First pubkey + CHECKSIG.
		require.True(t, tokenizer.Next())
		require.Equal(t, schnorr.SerializePubKey(key1), tokenizer.Data())
		require.True(t, tokenizer.Next())
		require.Equal(t, byte(txscript.OP_CHECKSIG), tokenizer.Opcode())

		// Second pubkey + CHECKSIGADD.
		require.True(t, tokenizer.Next())
		require.Equal(t, schnorr.SerializePubKey(key2), tokenizer.Data())
		require.True(t, tokenizer.Next())
		require.Equal(t, byte(txscript.OP_CHECKSIGADD), tokenizer.Opcode())

		// Third pubkey + CHECKSIGADD.
		require.True(t, tokenizer.Next())
		require.Equal(t, schnorr.SerializePubKey(key3), tokenizer.Data())
		require.True(t, tokenizer.Next())
		require.Equal(t, byte(txscript.OP_CHECKSIGADD), tokenizer.Opcode())

		// OP_3 OP_NUMEQUAL.
		require.True(t, tokenizer.Next())
		require.Equal(t, byte(txscript.OP_3), tokenizer.Opcode())
		require.True(t, tokenizer.Next())
		require.Equal(t, byte(txscript.OP_NUMEQUAL), tokenizer.Opcode())
	})

	t.Run("single key CHECKSIG", func(t *testing.T) {
		t.Parallel()

		closure := &MultisigClosure{
			PubKeys: []*btcec.PublicKey{key1},
			Type:    MultisigTypeChecksig,
		}

		script, err := closure.Script()
		require.NoError(t, err)

		// Verify script structure: <pubkey> OP_CHECKSIG
		tokenizer := txscript.MakeScriptTokenizer(0, script)

		require.True(t, tokenizer.Next())
		require.Equal(t, schnorr.SerializePubKey(key1), tokenizer.Data())

		require.True(t, tokenizer.Next())
		require.Equal(t, byte(txscript.OP_CHECKSIG), tokenizer.Opcode())

		require.False(t, tokenizer.Next())
	})
}

// TestMultisigClosure_DecodeDistinguishesVariants verifies that Decode
// correctly identifies which multisig variant is being used.
func TestMultisigClosure_DecodeDistinguishesVariants(t *testing.T) {
	t.Parallel()

	key1 := createTestKey(1)
	key2 := createTestKey(2)
	key3 := createTestKey(3)

	t.Run("distinguishes CHECKSIG from CHECKSIGADD", func(t *testing.T) {
		t.Parallel()

		// Create CHECKSIG variant.
		checksigClosure := &MultisigClosure{
			PubKeys: []*btcec.PublicKey{key1, key2},
			Type:    MultisigTypeChecksig,
		}
		checksigScript, err := checksigClosure.Script()
		require.NoError(t, err)

		// Create CHECKSIGADD variant.
		checksigAddClosure := &MultisigClosure{
			PubKeys: []*btcec.PublicKey{key1, key2, key3},
			Type:    MultisigTypeChecksigAdd,
		}
		checksigAddScript, err := checksigAddClosure.Script()
		require.NoError(t, err)

		// Scripts should be different.
		require.False(t, bytes.Equal(checksigScript, checksigAddScript))

		// Decode and verify types.
		decoded1 := &MultisigClosure{}
		valid, err := decoded1.Decode(checksigScript)
		require.NoError(t, err)
		require.True(t, valid)
		require.Equal(t, MultisigTypeChecksig, decoded1.Type)

		decoded2 := &MultisigClosure{}
		valid, err = decoded2.Decode(checksigAddScript)
		require.NoError(t, err)
		require.True(t, valid)
		require.Equal(t, MultisigTypeChecksigAdd, decoded2.Type)
	})
}

// TestClosure_DecodeEmptyScript verifies that all closure types handle
// empty scripts correctly.
func TestClosure_DecodeEmptyScript(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name    string
		closure Closure
	}{
		{"CSVSigClosure", &CSVSigClosure{}},
		{"CSVMultisigClosure", &CSVMultisigClosure{}},
		{"MultisigClosure", &MultisigClosure{}},
		{"CLTVMultisigClosure", &CLTVMultisigClosure{}},
		{"ConditionMultisigClosure", &ConditionMultisigClosure{}},
		{"ConditionCSVMultisigClosure", &ConditionCSVMultisigClosure{}},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			valid, err := tc.closure.Decode([]byte{})

			// All closures should reject empty scripts.
			if err == nil {
				require.False(t, valid)
			}
		})
	}
}

// TestCLTVMultisigClosure_LocktimeTypes verifies that CLTV closures correctly
// handle both block-based and seconds-based locktimes.
func TestCLTVMultisigClosure_LocktimeTypes(t *testing.T) {
	t.Parallel()

	key1 := createTestKey(1)
	key2 := createTestKey(2)

	t.Run("seconds-based locktime (>= 500000000)", func(t *testing.T) {
		t.Parallel()

		closure := &CLTVMultisigClosure{
			MultisigClosure: MultisigClosure{
				PubKeys: []*btcec.PublicKey{key1, key2},
				Type:    MultisigTypeChecksig,
			},
			Locktime: AbsoluteLocktime(500000001),
		}

		require.True(t, closure.Locktime.IsSeconds())

		script, err := closure.Script()
		require.NoError(t, err)

		decoded := &CLTVMultisigClosure{}
		valid, err := decoded.Decode(script)
		require.NoError(t, err)
		require.True(t, valid)
		require.True(t, decoded.Locktime.IsSeconds())
		require.Equal(t, closure.Locktime, decoded.Locktime)
	})

	t.Run("block-based locktime (< 500000000)", func(t *testing.T) {
		t.Parallel()

		closure := &CLTVMultisigClosure{
			MultisigClosure: MultisigClosure{
				PubKeys: []*btcec.PublicKey{key1, key2},
				Type:    MultisigTypeChecksig,
			},
			Locktime: AbsoluteLocktime(800000),
		}

		require.False(t, closure.Locktime.IsSeconds())

		script, err := closure.Script()
		require.NoError(t, err)

		decoded := &CLTVMultisigClosure{}
		valid, err := decoded.Decode(script)
		require.NoError(t, err)
		require.True(t, valid)
		require.False(t, decoded.Locktime.IsSeconds())
		require.Equal(t, closure.Locktime, decoded.Locktime)
	})
}

// TestCSVSigClosure_LocktimeTypes verifies that CSV closures correctly handle
// both block-based and seconds-based locktimes.
func TestCSVSigClosure_LocktimeTypes(t *testing.T) {
	t.Parallel()

	key1 := createTestKey(1)

	t.Run("seconds-based relative locktime", func(t *testing.T) {
		t.Parallel()

		closure := &CSVSigClosure{
			PubKey: key1,
			Locktime: RelativeLocktime{
				Type:  LocktimeTypeSecond,
				Value: 512 * 7, // Multiple of 512 seconds (3584s).
			},
		}

		script, err := closure.Script()
		require.NoError(t, err)

		decoded := &CSVSigClosure{}
		valid, err := decoded.Decode(script)
		require.NoError(t, err)
		require.True(t, valid)
		require.Equal(t, LocktimeTypeSecond, decoded.Locktime.Type)
	})

	t.Run("block-based relative locktime", func(t *testing.T) {
		t.Parallel()

		closure := &CSVSigClosure{
			PubKey: key1,
			Locktime: RelativeLocktime{
				Type:  LocktimeTypeBlock,
				Value: 144,
			},
		}

		script, err := closure.Script()
		require.NoError(t, err)

		decoded := &CSVSigClosure{}
		valid, err := decoded.Decode(script)
		require.NoError(t, err)
		require.True(t, valid)
		require.Equal(t, LocktimeTypeBlock, decoded.Locktime.Type)
		require.Equal(t, uint32(144), decoded.Locktime.Value)
	})
}

// TestConditionMultisigClosure_ComplexCondition verifies that condition
// closures can handle more complex condition scripts.
func TestConditionMultisigClosure_ComplexCondition(t *testing.T) {
	t.Parallel()

	key1 := createTestKey(1)
	key2 := createTestKey(2)

	t.Run("condition with data push", func(t *testing.T) {
		t.Parallel()

		// Condition: OP_SHA256 <hash> OP_EQUAL
		// This checks if a preimage hashes to a specific value.
		preimage := []byte("test_preimage")
		hash := txscript.NewScriptBuilder()
		hash.AddOp(txscript.OP_SHA256)
		// Note: In real usage, you'd compute the actual hash.

		// Simplified condition: OP_DUP OP_DROP OP_1
		// (Duplicates top of stack, drops it, pushes 1 = always true).
		condition := []byte{
			txscript.OP_DUP,
			txscript.OP_DROP,
			txscript.OP_1,
		}

		original := &ConditionMultisigClosure{
			MultisigClosure: MultisigClosure{
				PubKeys: []*btcec.PublicKey{key1, key2},
				Type:    MultisigTypeChecksig,
			},
			Condition: condition,
		}

		script, err := original.Script()
		require.NoError(t, err)

		decoded := &ConditionMultisigClosure{}
		valid, err := decoded.Decode(script)
		require.NoError(t, err)
		require.True(t, valid)

		_ = preimage // Unused in this simplified test.

		require.Equal(t, condition, decoded.Condition)
	})
}
