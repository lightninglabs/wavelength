package arkscript

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
)

// Node is the interface that all AST nodes must implement. Each node
// represents a spending condition that can be compiled to a tapscript.
type Node interface {
	// Script compiles the node to its canonical tapscript encoding.
	Script() ([]byte, error)

	// nodeSealed is a marker method to ensure only types defined in this
	// package can implement the Node interface.
	nodeSealed()
}

// Multisig represents an N-of-N multi-signature check.
// All keys must sign for the script to succeed.
type Multisig struct {
	// Keys are the public keys that must all sign. Order is significant.
	Keys []*btcec.PublicKey
}

// Script compiles the Multisig node to its canonical tapscript encoding.
func (m *Multisig) Script() ([]byte, error) {
	if len(m.Keys) == 0 {
		return nil, fmt.Errorf("multisig: no keys provided")
	}

	for i, key := range m.Keys {
		if key == nil {
			return nil, fmt.Errorf("multisig: key at index "+
				"%d is nil", i)
		}
	}

	return m.scriptChecksig()
}

// scriptChecksig builds the CHECKSIGVERIFY chain encoding.
// Format: <k0> CHECKSIGVERIFY <k1> CHECKSIGVERIFY ... <kn-1> CHECKSIG.
func (m *Multisig) scriptChecksig() ([]byte, error) {
	builder := txscript.NewScriptBuilder()

	for i, key := range m.Keys {
		builder.AddData(schnorr.SerializePubKey(key))

		// Use CHECKSIGVERIFY for all keys except the last one.
		if i < len(m.Keys)-1 {
			builder.AddOp(txscript.OP_CHECKSIGVERIFY)
		} else {
			builder.AddOp(txscript.OP_CHECKSIG)
		}
	}

	return builder.Script()
}

// nodeSealed implements the Node interface.
func (m *Multisig) nodeSealed() {}

// CSV represents a relative timelock gate using OP_CHECKSEQUENCEVERIFY.
// The inner expression is evaluated first, then the CSV check is performed.
// Canonical encoding: <inner> <lock> OP_CHECKSEQUENCEVERIFY OP_DROP.
type CSV struct {
	// Lock is a canonical block-mode BIP-68 delay in the range 1..65535.
	Lock uint32

	// Inner is the expression to gate with the timelock.
	Inner Node
}

// Script compiles the CSV node to its canonical tapscript encoding.
func (c *CSV) Script() ([]byte, error) {
	if c.Inner == nil {
		return nil, fmt.Errorf("csv: inner node is nil")
	}
	if err := validateCSVLock(c.Lock); err != nil {
		return nil, err
	}

	// Compile the inner expression first.
	innerScript, err := c.Inner.Script()
	if err != nil {
		return nil, fmt.Errorf("csv: failed to compile inner: %w", err)
	}

	// Build the full script: <inner> <lock> CSV DROP
	builder := txscript.NewScriptBuilder()
	builder.AddOps(innerScript)
	builder.AddInt64(int64(c.Lock))
	builder.AddOp(txscript.OP_CHECKSEQUENCEVERIFY)
	builder.AddOp(txscript.OP_DROP)

	return builder.Script()
}

// validateCSVLock ensures a CSV operand is the canonical BIP-68 encoding of a
// non-zero block delay. Ark policies do not support time-mode CSV values, and
// accepting any high bit would let the raw validator comparison diverge from
// consensus. In particular, bit 31 makes OP_CHECKSEQUENCEVERIFY a NOP, while
// reserved bits are masked away during script execution.
func validateCSVLock(lock uint32) error {
	const maxBlockDelay = uint32(wire.SequenceLockTimeMask)

	if lock == 0 || lock > maxBlockDelay {
		return fmt.Errorf("csv: lock must be a canonical block delay "+
			"in range [1, %d], got 0x%08x", maxBlockDelay, lock)
	}

	return nil
}

// nodeSealed implements the Node interface.
func (c *CSV) nodeSealed() {}

// Condition represents a generic opaque script prefix that must execute
// before the inner spending clause is evaluated.
// Canonical encoding: <prefix> <inner>.
type Condition struct {
	// Predicate is a canonical script fragment prepended ahead of the
	// inner spending clause. The fragment is expected to enforce its
	// own VERIFY/DROP semantics as needed.
	Predicate []byte

	// Inner is the expression gated by the predicate.
	Inner Node
}

// Script compiles the Condition node to its canonical tapscript encoding.
func (c *Condition) Script() ([]byte, error) {
	if c.Inner == nil {
		return nil, fmt.Errorf("condition: inner node is nil")
	}

	if len(c.Predicate) == 0 {
		return nil, fmt.Errorf("condition: predicate script is empty")
	}
	if err := validateConditionPredicate(c.Predicate); err != nil {
		return nil, err
	}

	innerScript, err := c.Inner.Script()
	if err != nil {
		return nil, fmt.Errorf("condition: failed to compile inner: %w",
			err)
	}

	builder := txscript.NewScriptBuilder()
	builder.AddOps(c.Predicate)
	builder.AddOps(innerScript)

	return builder.Script()
}

// validateConditionPredicate ensures a raw predicate cannot change how the
// typed inner clause is parsed or bypass its execution. In particular, an
// incomplete data push could consume the inner script as pushed bytes, while
// OP_SUCCESS would make the entire tapscript succeed before the inner clause
// executes.
func validateConditionPredicate(predicate []byte) error {
	tokenizer := txscript.MakeScriptTokenizer(0, predicate)
	for tokenizer.Next() {
	}
	if err := tokenizer.Err(); err != nil {
		return fmt.Errorf("condition: predicate is not a complete "+
			"script fragment: %w", err)
	}
	if txscript.ScriptHasOpSuccess(predicate) {
		return fmt.Errorf("condition: predicate contains OP_SUCCESS " +
			"opcode")
	}

	return nil
}

// nodeSealed implements the Node interface.
func (c *Condition) nodeSealed() {}

// Hash160Condition builds the canonical script prefix for
// HASH160(<witness_item>) == hash.
func Hash160Condition(hash []byte) ([]byte, error) {
	if len(hash) != 20 {
		return nil, fmt.Errorf("hash160 condition requires 20-byte "+
			"hash, got %d", len(hash))
	}

	builder := txscript.NewScriptBuilder()
	builder.AddOp(txscript.OP_HASH160)
	builder.AddData(hash)
	builder.AddOp(txscript.OP_EQUALVERIFY)

	return builder.Script()
}

// AbsoluteLockTimeCondition builds the canonical script prefix for
// nLockTime >= lock enforced via OP_CHECKLOCKTIMEVERIFY.
func AbsoluteLockTimeCondition(lock uint32) ([]byte, error) {
	builder := txscript.NewScriptBuilder()
	builder.AddInt64(int64(lock))
	builder.AddOp(txscript.OP_CHECKLOCKTIMEVERIFY)
	builder.AddOp(txscript.OP_DROP)

	return builder.Script()
}

// PaymentHash160Condition builds the canonical vHTLC success predicate
// script for a 32-byte Lightning payment preimage:
// OP_SIZE 32 EQUALVERIFY HASH160 <HASH160(payment_hash)> EQUALVERIFY.
func PaymentHash160Condition(paymentHash lntypes.Hash) ([]byte, error) {
	builder := txscript.NewScriptBuilder()
	builder.AddOp(txscript.OP_SIZE)
	builder.AddInt64(32)
	builder.AddOp(txscript.OP_EQUALVERIFY)
	builder.AddOp(txscript.OP_HASH160)
	builder.AddData(input.Ripemd160H(paymentHash[:]))
	builder.AddOp(txscript.OP_EQUALVERIFY)

	return builder.Script()
}
