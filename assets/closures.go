package assets

import (
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

type Closure interface {
	Script() ([]byte, error)
	Decode(script []byte) (bool, error)
	Witness(controlBlock []byte, opts map[string][]byte) (wire.TxWitness,
		error)
}

// CSVClosure is a simple CSV (CheckSequenceVerify) timeout closure. This allows
// a key holder to spend after a relative timelock delay.
type CSVClosure struct {
	// Key is the public key that can spend after the CSV delay.
	Key *btcec.PublicKey

	// Delay is the CSV delay in blocks.
	Delay uint32
}

// Script returns the CSV timeout script.
//
// Script structure:
//
//	<Key> OP_CHECKSIGVERIFY <Delay> OP_CHECKSEQUENCEVERIFY
func (c *CSVClosure) Script() ([]byte, error) {
	builder := txscript.NewScriptBuilder()
	builder.AddData(schnorr.SerializePubKey(c.Key))
	builder.AddOp(txscript.OP_CHECKSIGVERIFY)
	builder.AddInt64(int64(c.Delay))
	builder.AddOp(txscript.OP_CHECKSEQUENCEVERIFY)

	return builder.Script()
}

// Leaf returns the taproot leaf for this closure.
func (c *CSVClosure) Leaf() txscript.TapLeaf {
	script, _ := c.Script()

	return txscript.TapLeaf{
		LeafVersion: txscript.BaseLeafVersion,
		Script:      script,
	}
}

// ScriptClosure returns a ScriptClosure with witness function for this CSV
// closure.
func (c *CSVClosure) ScriptClosure() ScriptClosure {
	keyHex := hex.EncodeToString(schnorr.SerializePubKey(c.Key))
	return ScriptClosure{
		ID:     "csv",
		Script: c.Script,
		WitnessFunc: func(controlBlock []byte, sigs map[string][]byte) (
			wire.TxWitness, error) {

			sig, ok := sigs[keyHex]
			if !ok {
				return nil, fmt.Errorf("missing csv signature")
			}

			scriptBytes, err := c.Script()
			if err != nil {
				return nil, err
			}

			return wire.TxWitness{
				sig,
				scriptBytes,
				controlBlock,
			}, nil
		},
	}
}

// CheckSigAddClosure is a 2-of-2 multisig closure using CHECKSIGADD. This
// requires both parties to sign cooperatively.
type CheckSigAddClosure struct {
	// Key1 is the first signer's public key.
	Key1 *btcec.PublicKey

	// Key2 is the second signer's public key.
	Key2 *btcec.PublicKey
}

// Script returns the CHECKSIGADD 2-of-2 multisig script.
//
// Script structure:
//
//	<Key1> OP_CHECKSIG <Key2> OP_CHECKSIGADD <2> OP_EQUAL
func (c *CheckSigAddClosure) Script() ([]byte, error) {
	builder := txscript.NewScriptBuilder()

	// Add first key + CHECKSIG
	builder.AddData(schnorr.SerializePubKey(c.Key1))
	builder.AddOp(txscript.OP_CHECKSIG)

	// Add second key + CHECKSIGADD
	builder.AddData(schnorr.SerializePubKey(c.Key2))
	builder.AddOp(txscript.OP_CHECKSIGADD)

	// Require exactly 2 valid signatures
	builder.AddInt64(2)
	builder.AddOp(txscript.OP_EQUAL)

	return builder.Script()
}

// Leaf returns the taproot leaf for this closure.
func (c *CheckSigAddClosure) Leaf() txscript.TapLeaf {
	script, _ := c.Script()

	return txscript.TapLeaf{
		LeafVersion: txscript.BaseLeafVersion,
		Script:      script,
	}
}

// ScriptClosure returns a ScriptClosure with witness function for this
// CHECKSIGADD closure.
func (c *CheckSigAddClosure) ScriptClosure() ScriptClosure {
	key1Hex := hex.EncodeToString(schnorr.SerializePubKey(c.Key1))
	key2Hex := hex.EncodeToString(schnorr.SerializePubKey(c.Key2))

	return ScriptClosure{
		ID:     "coop_multisig",
		Script: c.Script,
		WitnessFunc: func(controlBlock []byte, sigs map[string][]byte) (
			wire.TxWitness, error) {

			sig1, ok := sigs[key1Hex]
			if !ok {
				return nil, fmt.Errorf("missing key1 signature")
			}

			sig2, ok := sigs[key2Hex]
			if !ok {
				return nil, fmt.Errorf("missing key2 signature")
			}

			scriptBytes, err := c.Script()
			if err != nil {
				return nil, err
			}

			return wire.TxWitness{
				sig2,
				sig1,
				scriptBytes,
				controlBlock,
			}, nil
		},
	}
}
