package arkscript

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/txscript"
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

// MultisigType specifies the encoding format for multisig scripts.
type MultisigType int

const (
	// MultisigTypeChecksig uses a CHECKSIGVERIFY chain for N-of-N multisig.
	// This is the canonical encoding for Ark VTXO collab leaves.
	// Format: <k0> CHECKSIGVERIFY <k1> CHECKSIGVERIFY ... <kn-1> CHECKSIG
	MultisigTypeChecksig MultisigType = iota

	// MultisigTypeChecksigAdd uses CHECKSIGADD with NUMEQUAL for N-of-N.
	// Format: <k0> CHECKSIG <k1> CHECKSIGADD ... <kn-1> CHECKSIGADD <n> NUMEQUAL
	MultisigTypeChecksigAdd
)

// HashAlgorithm specifies the hash algorithm for hashlock conditions.
type HashAlgorithm int

const (
	// HashAlgoHash160 uses RIPEMD160(SHA256(preimage)).
	HashAlgoHash160 HashAlgorithm = iota

	// HashAlgoSHA256 uses SHA256(preimage).
	HashAlgoSHA256
)

// Checksig represents a single-key signature check.
// Canonical encoding: <xonly_pubkey> OP_CHECKSIG
type Checksig struct {
	// Key is the public key that must sign.
	Key *btcec.PublicKey
}

// Script compiles the Checksig node to its canonical tapscript encoding.
func (c *Checksig) Script() ([]byte, error) {
	if c.Key == nil {
		return nil, fmt.Errorf("checksig: key is nil")
	}

	builder := txscript.NewScriptBuilder()
	builder.AddData(schnorr.SerializePubKey(c.Key))
	builder.AddOp(txscript.OP_CHECKSIG)

	return builder.Script()
}

// nodeSealed implements the Node interface.
func (c *Checksig) nodeSealed() {}

// Multisig represents an N-of-N multi-signature check.
// All keys must sign for the script to succeed.
type Multisig struct {
	// Keys are the public keys that must all sign. Order is significant.
	Keys []*btcec.PublicKey

	// Type specifies the encoding format (checksig chain or checksigadd).
	Type MultisigType
}

// Script compiles the Multisig node to its canonical tapscript encoding.
func (m *Multisig) Script() ([]byte, error) {
	if len(m.Keys) == 0 {
		return nil, fmt.Errorf("multisig: no keys provided")
	}

	for i, key := range m.Keys {
		if key == nil {
			return nil, fmt.Errorf("multisig: key at index %d is nil", i)
		}
	}

	switch m.Type {
	case MultisigTypeChecksig:
		return m.scriptChecksig()

	case MultisigTypeChecksigAdd:
		return m.scriptChecksigAdd()

	default:
		return nil, fmt.Errorf("multisig: unknown type %d", m.Type)
	}
}

// scriptChecksig builds the CHECKSIGVERIFY chain encoding.
// Format: <k0> CHECKSIGVERIFY <k1> CHECKSIGVERIFY ... <kn-1> CHECKSIG
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

// scriptChecksigAdd builds the CHECKSIGADD + NUMEQUAL encoding.
// Format: <k0> CHECKSIG <k1> CHECKSIGADD ... <kn-1> CHECKSIGADD <n> NUMEQUAL
func (m *Multisig) scriptChecksigAdd() ([]byte, error) {
	builder := txscript.NewScriptBuilder()

	for i, key := range m.Keys {
		builder.AddData(schnorr.SerializePubKey(key))

		if i == 0 {
			builder.AddOp(txscript.OP_CHECKSIG)
		} else {
			builder.AddOp(txscript.OP_CHECKSIGADD)
		}
	}

	builder.AddInt64(int64(len(m.Keys)))
	builder.AddOp(txscript.OP_NUMEQUAL)

	return builder.Script()
}

// nodeSealed implements the Node interface.
func (m *Multisig) nodeSealed() {}

// CSV represents a relative timelock gate using OP_CHECKSEQUENCEVERIFY.
// The inner expression is evaluated first, then the CSV check is performed.
// Canonical encoding: <inner> <lock> OP_CHECKSEQUENCEVERIFY OP_DROP
type CSV struct {
	// Lock is the BIP-68 encoded relative locktime value.
	Lock uint32

	// Inner is the expression to gate with the timelock.
	Inner Node
}

// Script compiles the CSV node to its canonical tapscript encoding.
func (c *CSV) Script() ([]byte, error) {
	if c.Inner == nil {
		return nil, fmt.Errorf("csv: inner node is nil")
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

// nodeSealed implements the Node interface.
func (c *CSV) nodeSealed() {}

// CLTV represents an absolute timelock gate using OP_CHECKLOCKTIMEVERIFY.
// Canonical encoding: <lock> OP_CHECKLOCKTIMEVERIFY OP_DROP <inner>
type CLTV struct {
	// Lock is the absolute locktime value (block height or unix timestamp).
	Lock uint32

	// Inner is the expression to gate with the timelock.
	Inner Node
}

// Script compiles the CLTV node to its canonical tapscript encoding.
func (c *CLTV) Script() ([]byte, error) {
	if c.Inner == nil {
		return nil, fmt.Errorf("cltv: inner node is nil")
	}

	// Compile the inner expression.
	innerScript, err := c.Inner.Script()
	if err != nil {
		return nil, fmt.Errorf("cltv: failed to compile inner: %w", err)
	}

	// Build the full script: <lock> CLTV DROP <inner>
	builder := txscript.NewScriptBuilder()
	builder.AddInt64(int64(c.Lock))
	builder.AddOp(txscript.OP_CHECKLOCKTIMEVERIFY)
	builder.AddOp(txscript.OP_DROP)
	builder.AddOps(innerScript)

	return builder.Script()
}

// nodeSealed implements the Node interface.
func (c *CLTV) nodeSealed() {}

// HashLock represents a preimage gate using a hash verification.
// Canonical encoding depends on algorithm:
//   - HASH160: OP_HASH160 <20-byte-hash> OP_EQUALVERIFY <inner>
//   - SHA256:  OP_SHA256 <32-byte-hash> OP_EQUALVERIFY <inner>
type HashLock struct {
	// Algorithm specifies which hash function to use.
	Algorithm HashAlgorithm

	// Hash is the expected hash value (20 bytes for HASH160, 32 for SHA256).
	Hash []byte

	// Inner is the expression to gate with the hashlock.
	Inner Node
}

// Script compiles the HashLock node to its canonical tapscript encoding.
func (h *HashLock) Script() ([]byte, error) {
	if h.Inner == nil {
		return nil, fmt.Errorf("hashlock: inner node is nil")
	}

	// Validate hash length based on algorithm.
	switch h.Algorithm {
	case HashAlgoHash160:
		if len(h.Hash) != 20 {
			return nil, fmt.Errorf(
				"hashlock: HASH160 requires 20-byte hash, got %d",
				len(h.Hash),
			)
		}

	case HashAlgoSHA256:
		if len(h.Hash) != 32 {
			return nil, fmt.Errorf(
				"hashlock: SHA256 requires 32-byte hash, got %d",
				len(h.Hash),
			)
		}

	default:
		return nil, fmt.Errorf("hashlock: unknown algorithm %d", h.Algorithm)
	}

	// Compile the inner expression.
	innerScript, err := h.Inner.Script()
	if err != nil {
		return nil, fmt.Errorf("hashlock: failed to compile inner: %w", err)
	}

	// Build the full script: OP_HASH* <hash> EQUALVERIFY <inner>
	builder := txscript.NewScriptBuilder()

	switch h.Algorithm {
	case HashAlgoHash160:
		builder.AddOp(txscript.OP_HASH160)

	case HashAlgoSHA256:
		builder.AddOp(txscript.OP_SHA256)
	}

	builder.AddData(h.Hash)
	builder.AddOp(txscript.OP_EQUALVERIFY)
	builder.AddOps(innerScript)

	return builder.Script()
}

// nodeSealed implements the Node interface.
func (h *HashLock) nodeSealed() {}
