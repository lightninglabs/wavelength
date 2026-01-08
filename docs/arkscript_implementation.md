# Arkscript Implementation Guide

This document describes what was implemented in the `lib/arkscript` package and
how it works. The package provides a typed AST (Abstract Syntax Tree) for
composing Bitcoin tapscript spending conditions with deterministic compilation.

## Overview

The arkscript package replaces ad-hoc script construction with a structured
approach:

```
AST Nodes → Canonical Scripts → Sorted Leaves → Balanced Tree → Output Key
```

**Key benefits:**
- Type-safe composition (no raw script bytes)
- Deterministic output keys across implementations
- Automatic tx-context derivation (sequence/locktime)
- Support for Taproot Assets composition

## Package Structure

```
lib/arkscript/
├── node.go         # AST node types (Checksig, Multisig, CSV, CLTV, HashLock)
├── tree.go         # Tree builder, leaf ordering, SpendInfo
├── vtxo.go         # VTXO policy builder and validation
├── psbt.go         # PSBT tap tree encoding helpers
├── compose.go      # Taproot Assets composition API
├── golden_test.go  # Backward compatibility vectors
└── vhtlc_test.go   # vHTLC example demonstrating full AST usage
```

## AST Node Types

The package defines 5 composable node types that can be nested to create complex
spending conditions.

### Checksig

Single-key signature check.

```go
node := &Checksig{Key: pubkey}
```

Compiles to:
```
<xonly_pubkey> OP_CHECKSIG
```

### Multisig

N-of-N multi-signature check. All keys must sign.

```go
node := &Multisig{
    Keys: []*btcec.PublicKey{key1, key2, key3},
    Type: MultisigTypeChecksig,  // CHECKSIGVERIFY chain
}
```

Compiles to:
```
<key1> OP_CHECKSIGVERIFY <key2> OP_CHECKSIGVERIFY <key3> OP_CHECKSIG
```

### CSV (Relative Timelock)

Relative timelock gate using OP_CHECKSEQUENCEVERIFY.

```go
node := &CSV{
    Lock:  144,  // ~1 day in blocks
    Inner: &Checksig{Key: pubkey},
}
```

Compiles to:
```
<inner_script> <lock> OP_CHECKSEQUENCEVERIFY OP_DROP
```

**Tx-context:** Sets `RequiredSequence` to the lock value.

### CLTV (Absolute Timelock)

Absolute timelock gate using OP_CHECKLOCKTIMEVERIFY.

```go
node := &CLTV{
    Lock:  500000,  // Block height
    Inner: &Checksig{Key: pubkey},
}
```

Compiles to:
```
<lock> OP_CHECKLOCKTIMEVERIFY OP_DROP <inner_script>
```

**Tx-context:** Sets `RequiredLockTime` to the lock value, `RequiredSequence` to
`0xfffffffe` (non-final).

### HashLock

Preimage gate using hash verification.

```go
node := &HashLock{
    Algorithm: HashAlgoHash160,  // or HashAlgoSHA256
    Hash:      preimageHash,     // 20 bytes for HASH160, 32 for SHA256
    Inner:     &Checksig{Key: pubkey},
}
```

Compiles to:
```
OP_HASH160 <hash> OP_EQUALVERIFY <inner_script>
```

## Composing Complex Policies

Nodes can be nested to create complex spending conditions. Here's an example of
a CSV-gated hashlock:

```go
// Exit path: receiver can claim with preimage after 144 blocks
node := &CSV{
    Lock: 144,
    Inner: &HashLock{
        Algorithm: HashAlgoHash160,
        Hash:      preimageHash,
        Inner:     &Checksig{Key: receiverKey},
    },
}
```

This compiles to:
```
OP_HASH160 <hash> OP_EQUALVERIFY <receiver_key> OP_CHECKSIG 144 OP_CSV OP_DROP
```

## Building a Policy

### Step 1: Define Leaves

Each leaf has a role for canonical ordering:

```go
leaves := []PolicyLeaf{
    {
        Role: LeafRoleCollab,  // Rank 0
        Leaf: txscript.NewBaseTapLeaf(collabScript),
    },
    {
        Role: LeafRoleExit,    // Rank 1
        Leaf: txscript.NewBaseTapLeaf(exitScript),
    },
}
```

### Step 2: Sort Leaves

Leaves are sorted by role rank, then lexicographically by script bytes:

```go
SortLeaves(leaves)
```

### Step 3: Build Tree

The tree is built using a balanced binary algorithm:

```go
policy, err := BuildTree(leaves, &scripts.ARKNUMSKey)
```

### Step 4: Get Output Key

```go
outputKey := policy.OutputKey()
```

### Step 5: Get SpendInfo

For each leaf, retrieve the script and control block:

```go
info, err := policy.SpendInfo(leafIndex)
// info.WitnessScript    - the leaf script
// info.ControlBlock     - BIP-341 control block
// info.RequiredSequence - nSequence requirement
// info.RequiredLockTime - nLockTime requirement
```

## Standard VTXO Policy

The default VTXO has 2 leaves:

```go
policy, err := NewVTXOPolicy(ownerKey, operatorKey, exitDelay)
```

**Leaf 0 (collab):** `Multisig([owner, operator])`
```
<owner> OP_CHECKSIGVERIFY <operator> OP_CHECKSIG
```

**Leaf 1 (exit):** `CSV(delay, Checksig(owner))`
```
<owner> OP_CHECKSIG <delay> OP_CSV OP_DROP
```

## vHTLC Example

The vHTLC (virtual HTLC) demonstrates the full power of AST composition. It has
6 leaves covering all spending scenarios:

```go
type VHTLCOpts struct {
    Sender                               *btcec.PublicKey
    Receiver                             *btcec.PublicKey
    Server                               *btcec.PublicKey
    PreimageHash                         []byte  // HASH160, 20 bytes
    RefundLocktime                       uint32  // Absolute (CLTV)
    UnilateralClaimDelay                 uint32  // Relative (CSV)
    UnilateralRefundDelay                uint32  // Relative (CSV)
    UnilateralRefundWithoutReceiverDelay uint32  // Relative (CSV)
}
```

### vHTLC Leaves

| # | Name | AST Composition | Purpose |
|---|------|-----------------|---------|
| 1 | Claim | `HashLock(hash, Multisig([receiver, server]))` | Receiver claims with preimage + server cosign |
| 2 | Refund | `Multisig([sender, receiver, server])` | All parties agree to refund |
| 3 | RefundWithoutReceiver | `CLTV(locktime, Multisig([sender, server]))` | Refund after timeout without receiver |
| 4 | UnilateralClaim | `CSV(delay, HashLock(hash, Checksig(receiver)))` | Receiver exits with preimage |
| 5 | UnilateralRefund | `CSV(delay, Multisig([sender, receiver]))` | Sender+receiver exit together |
| 6 | UnilateralRefundWithoutReceiver | `CSV(delay, Checksig(sender))` | Sender exits alone (longest delay) |

### vHTLC Implementation

```go
func NewVHTLCPolicy(opts VHTLCOpts) (*VHTLCPolicy, error) {
    // 1. Claim: HashLock + Multisig([receiver, server])
    claimClosure := &HashLock{
        Algorithm: HashAlgoHash160,
        Hash:      opts.PreimageHash,
        Inner: &Multisig{
            Keys: []*btcec.PublicKey{opts.Receiver, opts.Server},
            Type: MultisigTypeChecksig,
        },
    }

    // 2. Refund: Multisig([sender, receiver, server])
    refundClosure := &Multisig{
        Keys: []*btcec.PublicKey{opts.Sender, opts.Receiver, opts.Server},
        Type: MultisigTypeChecksig,
    }

    // 3. RefundWithoutReceiver: CLTV + Multisig([sender, server])
    refundWithoutReceiverClosure := &CLTV{
        Lock: opts.RefundLocktime,
        Inner: &Multisig{
            Keys: []*btcec.PublicKey{opts.Sender, opts.Server},
            Type: MultisigTypeChecksig,
        },
    }

    // 4. UnilateralClaim: CSV + HashLock + Checksig(receiver)
    unilateralClaimClosure := &CSV{
        Lock: opts.UnilateralClaimDelay,
        Inner: &HashLock{
            Algorithm: HashAlgoHash160,
            Hash:      opts.PreimageHash,
            Inner:     &Checksig{Key: opts.Receiver},
        },
    }

    // 5. UnilateralRefund: CSV + Multisig([sender, receiver])
    unilateralRefundClosure := &CSV{
        Lock: opts.UnilateralRefundDelay,
        Inner: &Multisig{
            Keys: []*btcec.PublicKey{opts.Sender, opts.Receiver},
            Type: MultisigTypeChecksig,
        },
    }

    // 6. UnilateralRefundWithoutReceiver: CSV + Checksig(sender)
    unilateralRefundWithoutReceiverClosure := &CSV{
        Lock: opts.UnilateralRefundWithoutReceiverDelay,
        Inner: &Checksig{Key: opts.Sender},
    }

    // Build leaves with roles
    leaves := []PolicyLeaf{
        {Role: LeafRoleCollab, Leaf: txscript.NewBaseTapLeaf(claimScript)},
        {Role: LeafRoleCollab, Leaf: txscript.NewBaseTapLeaf(refundScript)},
        {Role: LeafRoleCollab, Leaf: txscript.NewBaseTapLeaf(refundWithoutReceiverScript)},
        {Role: LeafRoleExit, Leaf: txscript.NewBaseTapLeaf(unilateralClaimScript)},
        {Role: LeafRoleExit, Leaf: txscript.NewBaseTapLeaf(unilateralRefundScript)},
        {Role: LeafRoleExit, Leaf: txscript.NewBaseTapLeaf(unilateralRefundWithoutReceiverScript)},
    }

    SortLeaves(leaves)
    return BuildTree(leaves, &scripts.ARKNUMSKey)
}
```

### vHTLC Compiled Scripts

Running the test shows the actual compiled scripts:

```
Claim (HashLock+Multisig):
  OP_HASH160 <hash> OP_EQUALVERIFY <receiver> OP_CHECKSIGVERIFY <server> OP_CHECKSIG

Refund (Multisig):
  <sender> OP_CHECKSIGVERIFY <receiver> OP_CHECKSIGVERIFY <server> OP_CHECKSIG

RefundWithoutReceiver (CLTV+Multisig):
  <locktime> OP_CLTV OP_DROP <sender> OP_CHECKSIGVERIFY <server> OP_CHECKSIG

UnilateralClaim (CSV+HashLock+Checksig):
  OP_HASH160 <hash> OP_EQUALVERIFY <receiver> OP_CHECKSIG <delay> OP_CSV OP_DROP

UnilateralRefund (CSV+Multisig):
  <sender> OP_CHECKSIGVERIFY <receiver> OP_CHECKSIG <delay> OP_CSV OP_DROP

UnilateralRefundWithoutReceiver (CSV+Checksig):
  <sender> OP_CHECKSIG <delay> OP_CSV OP_DROP
```

## Transaction Context Derivation

The package automatically derives tx-context requirements from the AST:

```go
// For a CSV-gated leaf
info, _ := policy.SpendInfo(leafIndex)
info.RequiredSequence  // = CSV lock value
info.RequiredLockTime  // = 0

// For a CLTV-gated leaf
info.RequiredSequence  // = 0xfffffffe (non-final)
info.RequiredLockTime  // = CLTV lock value

// For a leaf with no timelocks
info.RequiredSequence  // = 0xffffffff
info.RequiredLockTime  // = 0
```

You can also derive directly from a node:

```go
seq := DeriveSequence(node)
locktime := DeriveLockTime(node)
```

## PSBT Encoding

The package provides helpers for PSBT tap tree metadata:

```go
// Encode tap tree for PSBT storage
encoded, err := EncodeTapTree(policy)

// Decode tap tree during finalization
leaves, err := DecodeTapTree(encoded)

// Encode hashlock preimage
encoded := EncodeConditionWitness(preimage)

// Decode hashlock preimage
preimage, err := DecodeConditionWitness(encoded)
```

PSBT keys use the `ark/` namespace:
- `ark/taptree` - tap tree encoding
- `ark/condition` - hashlock preimage

## Taproot Assets Composition

For Taproot Assets integration, policies can be composed with an external root:

```go
// Build the Ark policy
policy, err := NewVTXOPolicy(owner, operator, delay)

// Compose with external root (e.g., Taproot Assets commitment)
composed, err := ComposeWithSiblingRoot(policy.CompiledPolicy, externalRoot)

// Get the composed output key
outputKey := composed.OutputKey()

// SpendInfo includes external root in control block
info, err := composed.SpendInfo(leafIndex)
// Control block is 32 bytes longer (external root as sibling)
```

The composition uses BIP-341 ordering:
```
combined = TapBranchHash(min(policyRoot, extRoot), max(policyRoot, extRoot))
```

## Validation

VTXO policies are validated for required invariants:

```go
err := ValidateVTXOLeaves(leaves)
// Returns ErrMissingCollab if no collab leaf
// Returns ErrMissingExit if no exit leaf
```

Error types are comparable with stable codes:
```go
if errors.Is(err, ErrMissingCollab) {
    // Handle missing collab leaf
}
```

## Backward Compatibility

The package maintains byte-for-byte compatibility with the existing
`lib/scripts.VTXOTapScript()` function. This is verified by golden tests in
`golden_test.go`:

```go
// These must match exactly
arkscriptPolicy.OutputKey() == scriptsPackage.TaprootKey()
arkscriptPolicy.RootHash == scriptsPackage.RootHash
arkscriptPolicy.Leaves[i].Script == scriptsPackage.Leaves[i].Script
```

## Testing

Run all arkscript tests:
```bash
make unit pkg=./lib/arkscript timeout=5m
```

Run specific test:
```bash
make unit pkg=./lib/arkscript case=TestVHTLCPolicyConstruction
```

Run with verbose output:
```bash
go test -v ./lib/arkscript/... -run TestVHTLC
```

## Summary

The arkscript package provides:

1. **Type-safe AST nodes** - Checksig, Multisig, CSV, CLTV, HashLock
2. **Composable nesting** - Build complex policies from simple primitives
3. **Canonical ordering** - Deterministic output keys across implementations
4. **Automatic tx-context** - Sequence and locktime derived from AST
5. **PSBT helpers** - Encode/decode tap trees for OOR finalization
6. **Assets composition** - Combine with external Taproot Assets roots
7. **Validation** - Enforce VTXO policy invariants

The vHTLC example in `vhtlc_test.go` demonstrates all these features working
together to create a production-ready HTLC implementation.
