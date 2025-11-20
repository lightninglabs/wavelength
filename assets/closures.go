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

// CollabMultisigClosure is a 2-of-2 multisig closure using CHECKSIGVERIFY +
// CHECKSIG. This matches the collaborative path in scripts.MultiSigCollabTapLeaf
// used for VTXOs.
type CollabMultisigClosure struct {
	// OwnerKey is the owner's public key (checked with CHECKSIGVERIFY).
	OwnerKey *btcec.PublicKey

	// CosignerKey is the cosigner's public key (checked with CHECKSIG).
	CosignerKey *btcec.PublicKey
}

// VTXOTimeoutClosure is a CSV timeout closure matching the timeout path in
// scripts.UnilateralCSVTimeoutTapLeaf used for VTXOs.
type VTXOTimeoutClosure struct {
	// Key is the public key that can spend after the CSV delay.
	Key *btcec.PublicKey

	// Delay is the CSV delay in blocks.
	Delay uint32
}

// Script returns the VTXO timeout script matching
// scripts.UnilateralCSVTimeoutTapLeaf.
//
// Script structure:
//
//	<key> OP_CHECKSIG
//	<delay> OP_CHECKSEQUENCEVERIFY OP_DROP
func (c *VTXOTimeoutClosure) Script() ([]byte, error) {
	builder := txscript.NewScriptBuilder()
	builder.AddData(schnorr.SerializePubKey(c.Key))
	builder.AddOp(txscript.OP_CHECKSIG)
	builder.AddInt64(int64(c.Delay))
	builder.AddOp(txscript.OP_CHECKSEQUENCEVERIFY)
	builder.AddOp(txscript.OP_DROP)

	return builder.Script()
}

// Leaf returns the taproot leaf for this closure.
func (c *VTXOTimeoutClosure) Leaf() txscript.TapLeaf {
	script, _ := c.Script()

	return txscript.TapLeaf{
		LeafVersion: txscript.BaseLeafVersion,
		Script:      script,
	}
}

// ScriptClosure returns a ScriptClosure with witness function for this VTXO
// timeout closure.
func (c *VTXOTimeoutClosure) ScriptClosure() ScriptClosure {
	keyHex := hex.EncodeToString(schnorr.SerializePubKey(c.Key))
	return ScriptClosure{
		ID:     "vtxo_timeout",
		Script: c.Script,
		WitnessFunc: func(controlBlock []byte, sigs map[string][]byte) (
			wire.TxWitness, error) {

			sig, ok := sigs[keyHex]
			if !ok {
				return nil, fmt.Errorf("missing timeout signature")
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

// Script returns the collaborative multisig script matching
// scripts.MultiSigCollabTapLeaf.
//
// Script structure:
//
//	<owner_key> OP_CHECKSIGVERIFY
//	<cosigner_key> OP_CHECKSIG
func (c *CollabMultisigClosure) Script() ([]byte, error) {
	builder := txscript.NewScriptBuilder()
	builder.AddData(schnorr.SerializePubKey(c.OwnerKey))
	builder.AddOp(txscript.OP_CHECKSIGVERIFY)
	builder.AddData(schnorr.SerializePubKey(c.CosignerKey))
	builder.AddOp(txscript.OP_CHECKSIG)

	return builder.Script()
}

// Leaf returns the taproot leaf for this closure.
func (c *CollabMultisigClosure) Leaf() txscript.TapLeaf {
	script, _ := c.Script()

	return txscript.TapLeaf{
		LeafVersion: txscript.BaseLeafVersion,
		Script:      script,
	}
}

// ScriptClosure returns a ScriptClosure for this collaborative multisig.
// Witness order: <cosigner_sig> <owner_sig> <script> <control_block>
func (c *CollabMultisigClosure) ScriptClosure() ScriptClosure {
	ownerKeyHex := hex.EncodeToString(schnorr.SerializePubKey(c.OwnerKey))
	cosignerKeyHex := hex.EncodeToString(schnorr.SerializePubKey(c.CosignerKey))

	return ScriptClosure{
		ID:     "collab_multisig",
		Script: c.Script,
		WitnessFunc: func(controlBlock []byte, sigs map[string][]byte) (
			wire.TxWitness, error) {

			ownerSig, ok := sigs[ownerKeyHex]
			if !ok {
				return nil, fmt.Errorf("missing owner signature")
			}

			cosignerSig, ok := sigs[cosignerKeyHex]
			if !ok {
				return nil, fmt.Errorf("missing cosigner signature")
			}

			scriptBytes, err := c.Script()
			if err != nil {
				return nil, err
			}

			// Witness stack: cosigner_sig, owner_sig, script,
			// control_block (stack is LIFO, so owner_sig is checked
			// first by CHECKSIGVERIFY).
			return wire.TxWitness{
				cosignerSig,
				ownerSig,
				scriptBytes,
				controlBlock,
			}, nil
		},
	}
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
