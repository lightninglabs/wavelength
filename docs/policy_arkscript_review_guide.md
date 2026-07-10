# Policy-First Arkscript: Reviewer Guide

This document is a review aid for the `policy-arkscript` PR stack. It explains
the motivation, the type system, the security model, and walks through concrete
vHTLC examples to show how the new system works end to end.

## 1. What Changed and Why

### The Old Model (`lib/scripts`)

The old codebase represented VTXO outputs as **derived artifacts**:

```
ownerKey  + operatorKey  + exitDelay
    ↓
VTXOTapScript(ownerKey, operatorKey, exitDelay)
    ↓
pkScript (raw 34-byte P2TR output)
```

Every layer (tree builder, OOR runtime, DB, RPC, checkpoint signer) received
the raw tuple `(ownerKey, operatorKey, exitDelay)` and independently
reconstructed the taproot output. The system had no concept of _what policy_ an
output implemented — it just knew how to build the one hardcoded 2-leaf shape.

**Problems this caused:**

1. **No support for non-standard scripts.** A vHTLC has 6 leaves, not 2. The
   old model cannot represent it without ad-hoc escape hatches in every layer.

2. **Drift risk.** Seven different packages each contained their own version of
   "build a VTXO taproot tree from (owner, operator, delay)." A single-line
   divergence in any of them produces a different pkScript — and funds are
   locked to an address nobody can spend.

3. **Intent is lost.** Durable state (DB, actor snapshots, PSBT metadata) only
   stored compiled script bytes. After a crash-recovery, the system could
   reproduce the _address_ but not answer "which spend path was selected?" or
   "what semantic condition must be satisfied?"

### The New Model (`lib/arkscript`)

The new model stores the **semantic policy** as the source of truth and derives
everything else:

```
PolicyTemplate { Leaves: []LeafTemplate }
    ↓  .Compile()
CompiledPolicy { InternalKey, Leaves, RootHash, MerkleProofs }
    ↓  .OutputKey()
pkScript (derived, never stored as primary)
```

A `PolicyTemplate` is a list of `LeafTemplate` nodes, each containing an AST
(`Node`) that describes one tapscript leaf. The AST is:

- **Serializable** — stable binary encoding with version bytes.
- **Compilable** — every node can produce its canonical tapscript bytes.
- **Validatable** — the operator can verify Ark invariants by walking the AST.

---

## 2. The Node AST

There are exactly **three** node types, and the `Node` interface is sealed
(only types in `lib/arkscript` can implement it):

### `Multisig{Keys}`

N-of-N signature check. All keys must sign.

```
Script: <k0> CHECKSIGVERIFY <k1> CHECKSIGVERIFY ... <kN-1> CHECKSIG
```

Key order is significant — it determines witness stack ordering.

### `CSV{Lock, Inner}`

Relative timelock gate. The inner expression executes first, then
`OP_CHECKSEQUENCEVERIFY` enforces the delay.

```
Script: <inner_script> <lock> OP_CHECKSEQUENCEVERIFY OP_DROP
```

### `Condition{Predicate, Inner}`

An opaque script prefix (hashlock, absolute timelock, etc.) prepended before
the inner clause. The predicate must enforce its own `VERIFY`/`DROP` semantics.

```
Script: <predicate_bytes> <inner_script>
```

Helper constructors for common predicates:

| Helper                     | Produces                                        |
|----------------------------|-------------------------------------------------|
| `PaymentHash160Condition`  | `SIZE 32 EQUALVERIFY HASH160 <h> EQUALVERIFY`   |
| `Hash160Condition`         | `HASH160 <h> EQUALVERIFY`                        |
| `AbsoluteLockTimeCondition`| `<n> CHECKLOCKTIMEVERIFY DROP`                   |
| `sha256Condition`          | `SIZE 32 EQUALVERIFY SHA256 <h> EQUALVERIFY`     |

### Composition

These three building blocks compose arbitrarily. A vHTLC claim leaf is:

```go
Condition{
    Predicate: sha256Condition(hash),
    Inner: Multisig{Keys: [receiver, server]},
}
```

A unilateral claim leaf wraps that in a CSV gate:

```go
CSV{
    Lock: claimDelay,
    Inner: Condition{
        Predicate: sha256Condition(hash),
        Inner: Multisig{Keys: [receiver]},
    },
}
```

---

## 3. What Makes a Valid Ark Policy

`ValidatePolicy()` enforces four invariants that every policy accepted by the
Ark operator must satisfy:

### Invariant 1: At least one collab leaf

A leaf is "collab" if it **contains the operator key** in any of its Multisig
nodes (checked recursively through CSV/Condition wrappers). This guarantees the
operator has a cooperative settlement path.

### Invariant 2: At least one exit leaf

A leaf is "exit" if it **does not contain the operator key**. This guarantees
the owner can recover funds without operator cooperation.

### Invariant 3: Every exit leaf is CSV-gated

`ExtractCSVDelay(node)` walks the AST for the outermost `CSV` node. If an exit
leaf has no CSV wrapper, the policy is rejected. This is the critical safety
property — see the security analysis below.

### Invariant 4: Minimum exit delay

The smallest CSV delay across all exit leaves must meet the operator's
configured minimum. This gives the operator enough time to broadcast forfeits
if a participant attempts a stale unilateral exit.

### Concrete Examples

**Standard VTXO** (2 leaves, always valid):

```
Leaf 0 (collab): Multisig{owner, operator}     ← contains operator ✓
Leaf 1 (exit):   CSV{delay, Multisig{owner}}    ← no operator, CSV-gated ✓
```

**vHTLC** (6 leaves):

```
Leaf 0 (collab): Condition{hashlock, Multisig{receiver, server}}  ← server=operator ✓
Leaf 1 (collab): Multisig{sender, receiver, server}               ← server=operator ✓
Leaf 2 (collab): Condition{CLTV, Multisig{sender, server}}        ← server=operator ✓
Leaf 3 (exit):   CSV{d1, Condition{hashlock, Multisig{receiver}}} ← no operator, CSV ✓
Leaf 4 (exit):   CSV{d2, Multisig{sender, receiver}}              ← no operator, CSV ✓
Leaf 5 (exit):   CSV{d3, Condition{CLTV(refundLocktime),
                              Multisig{sender}}}                  ← no operator, CSV ✓, CLTV ✓
```

Leaf 5 (sender unilateral refund-without-receiver) is gated on both the
local CSV delay *and* the invoice/vHTLC CLTV. Reviewers should reject any
out-of-tree change that drops the CLTV gate: without it, a sender can race
an in-flight Lightning payment by exiting on chain before the receiver's
HTLC has expired, double-spending funds the receiver has already accepted
off-chain. This matches the swapdk-server expectation that the sender's
unilateral refund leaf is reachable only after the invoice expiry.

---

## 4. Security Analysis: No Unilateral Spend Before CSV

**Claim:** Under a valid Ark policy, no party can spend a VTXO unilaterally
(without the operator's signature) before the CSV delay expires.

**Proof sketch:**

1. The taproot output uses an **unspendable internal key** (NUMS point). There
   is no key-path spend — all spending must go through a tapscript leaf.

2. Every tapscript leaf in a valid policy is either:
   - A **collab leaf** (contains operator key in a Multisig), or
   - An **exit leaf** (CSV-gated, does not contain operator key).

3. To spend via a **collab leaf**, the spender needs the operator's Schnorr
   signature. The operator only signs to authorize a legitimate settlement
   (OOR transfer, round forfeit, etc.). Without the operator's cooperation,
   this path is unusable.

4. To spend via an **exit leaf**, the transaction must satisfy the
   `OP_CHECKSEQUENCEVERIFY` constraint. The Bitcoin consensus rule for CSV
   requires:
   - The spending transaction's `nSequence` for that input encodes a relative
     delay >= the script's CSV value.
   - The UTXO must have been confirmed for at least that many blocks.

   This means an exit spend is physically impossible until the CSV delay has
   passed since the on-chain commitment.

5. During that delay window, the operator can observe the unilateral exit
   attempt and broadcast the appropriate **forfeit transaction** (which uses a
   collab leaf the operator already co-signed during the original OOR/round
   flow).

**What about Condition predicates?** A `Condition` node prepends an opaque
script fragment, but the validation logic looks _through_ Conditions when
checking for operator key presence and CSV gating:

```go
case *Condition:
    return containsKeyBytes(n.Inner, target)  // recurse into Inner
```

```go
case *Condition:
    return extractCSVDelay(c.Inner)  // recurse into Inner
```

So a `Condition{hashlock, Multisig{attacker}}` without a CSV wrapper would
correctly be flagged as an ungated exit leaf and rejected.

**What about Condition predicates that embed keys?** The `Condition.Predicate`
is opaque bytes — the validator does not parse it for operator keys. This is
safe because the predicate only adds _restrictions_ (hashlocks, timelocks). A
predicate cannot _grant_ spending authority — that comes from the `Inner`
Multisig. The validator correctly checks only the Multisig nodes for key
presence.

**Edge case: empty Multisig in exit leaf?** Impossible — `Multisig.Script()`
returns an error if `len(Keys) == 0`, and `PolicyTemplate.Compile()` would
fail. A Multisig with only unknown keys would pass CSV validation but would be
unspendable by anyone, which is safe (funds locked, not stolen).

### Operator Safety Summary

| Threat                        | Mitigation                                    |
|-------------------------------|-----------------------------------------------|
| Spend without operator sig    | All non-operator leaves require CSV delay      |
| Bypass CSV via key-path spend | Internal key is NUMS (unspendable)             |
| Fake operator key in policy   | Operator validates own key presence at submit   |
| Ungated exit leaf injection   | `ValidatePolicy` rejects non-CSV exit leaves   |
| Predicate smuggling operator  | Predicates add restrictions, not authority      |
| Insufficient exit delay       | `MinExitDelay` check enforces operator minimum  |

---

## 5. Walkthrough: vHTLC Lifecycle

This section traces a complete swap flow to show how the pieces fit together.

### 5.1 Creating a vHTLC Output

Alice wants to receive 100k sats via Lightning. The swap server creates a vHTLC
output on Ark:

```go
opts := arkscript.VHTLCOpts{
    Sender:          serverKey,     // swap server pays
    Receiver:        aliceKey,      // Alice receives
    Server:          operatorKey,   // Ark operator co-signs
    PreimageHash:    sha256(preimage),
    RefundLocktime:  800_000,       // CLTV block height
    UnilateralClaimDelay:  144,     // ~1 day
    UnilateralRefundDelay: 144,
    UnilateralRefundWithoutReceiverDelay: 288,
}

vhtlc, err := arkscript.NewVHTLCPolicy(opts)
pkScript, _ := vhtlc.PkScript()  // P2TR address for 6-leaf policy
```

The server sends an OOR transfer to this pkScript. The `PolicyTemplate` is
serialized and stored alongside the VTXO in the DB, so every layer knows the
semantic meaning of this output.

### 5.2 Sending TO a vHTLC (OOR with Policy Destination)

When the OOR actor builds the transfer, the recipient output carries the full
policy:

```go
// Server-side: OOR actor materializes the vHTLC output
recipient := RecipientOutput{
    Value:              100_000,
    VTXOPolicyTemplate: vhtlcPolicy.Template.Encode(),  // semantic, not raw pkScript
}
```

The server validates this policy via `ValidatePolicy()` before accepting it
into the round/OOR flow — confirming operator key presence on collab leaves and
CSV gating on exit leaves.

The checkpoint transaction commits to this output. The output's tap tree is
embedded in the checkpoint PSBT metadata so it can be recovered after a crash.

### 5.3 Spending a vHTLC as Input (Claim Path)

Alice learns the preimage (e.g., from a settled Lightning invoice) and wants to
claim her funds via an OOR transfer to her own standard VTXO.

**Step 1: Build the TransferInput with custom spend path.**

```go
// Alice builds the claim spend path
claimPath, _ := vhtlc.ClaimPath(preimage)
// claimPath.SpendInfo = {WitnessScript, ControlBlock} for claim leaf
// claimPath.Conditions = [][]byte{preimage}

input := TransferInput{
    VTXO: &vtxo.Descriptor{
        Outpoint: vhtlcOutpoint,
        Amount:   100_000,
    },
    VTXOPolicyTemplate: vhtlcPolicy.Template.Encode(),
    CustomSpend:        claimPath,
}
```

**Step 2: Server validates the spend path against the policy.**

```go
// Server-side: oor/transfer_inputs.go (customSpendKeys)
template, _ := arkscript.DecodePolicyTemplate(input.VTXOPolicyTemplate)
compiled, _ := template.Compile()

// Verify the spend path is actually a leaf of this policy
for i, leaf := range compiled.Leaves {
    info, _ := compiled.SpendInfo(i)
    if bytes.Equal(info.WitnessScript, spendPath.WitnessScript) &&
       bytes.Equal(info.ControlBlock, spendPath.ControlBlock) {
        // Valid leaf — accept
    }
}
```

**Step 3: Checkpoint signing with the custom path.**

Instead of using the standard collab 2-of-2 signing flow, the client uses the
claim leaf:

```go
// Client-side: checkpoint_sign.go
// The operator has already co-signed the claim leaf (it contains server key)
signDesc := claimPath.SpendInfo.BuildSignDescriptor(
    aliceKeyDesc, vhtlcOutput, sigHashes, prevFetcher, inputIndex,
)

// Alice signs
aliceSig, _ := signer.SignOutputRaw(tx, signDesc)

// Assemble witness: [operatorSig, aliceSig, preimage, script, controlBlock]
witness, _ := claimPath.Witness(
    MaybeAppendSighash(operatorSig, SigHashDefault),
    MaybeAppendSighash(aliceSig, SigHashDefault),
)
```

**Step 4: Server validates the finalized witness.**

```go
// Server-side: lib/tx/oor/submit_signature_validate.go
// (ValidateFinalizePackageSigned)
// For custom spends, the server:
// 1. Extracts the witness from the finalized PSBT
// 2. Verifies the operator signature is present and unchanged
// 3. Runs the Bitcoin script VM to validate execution
engine, _ := txscript.NewEngine(pkScript, tx, inputIndex, flags, ...)
err := engine.Execute()  // Must succeed — proves all conditions met
```

### 5.4 Settlement Pairing (Forfeit Construction)

For the operator to accept a vHTLC input, it needs a **forfeit path** — a
pre-signed transaction the operator can broadcast if Alice tries to double-spend
via an old unilateral exit.

`SettlementPairsForParticipant` matches auth (unilateral) leaves with forfeit
(collab) leaves by normalizing away the CSV gate and operator key:

```
Auth leaf:    CSV{144, Condition{hashlock, Multisig{receiver}}}
                       ↓ strip CSV, strip operator
Normalized:   Condition{hashlock, Multisig{receiver}}

Forfeit leaf: Condition{hashlock, Multisig{receiver, server}}
                       ↓ strip operator
Normalized:   Condition{hashlock, Multisig{receiver}}

Match! → SettlementPair{AuthPath: exit claim, ForfeitPath: collab claim}
```

This ensures every unilateral recovery path has a corresponding operator-backed
forfeit that covers the same business logic.

---

## 6. Serialization

### PolicyTemplate Binary Format

```
[version: 1 byte = 0x01]
[leaf_count: varint]
  for each leaf:
    [leaf_version: 1 byte = 0x01]
    [node_bytes: varBytes]
      [node_kind: 1 byte]  // 1=Multisig, 2=CSV, 3=Condition
      [node_data: kind-specific]
```

Multisig: `kind(1) + keyCount(varint) + keys(32 bytes each)`
CSV: `kind(2) + lock(varint) + innerNode(varBytes)`
Condition: `kind(3) + predicate(varBytes) + innerNode(varBytes)`

### SpendPath Binary Format

```
[version: 1 byte = 0x01]
[condition_count: varint]
  for each condition:
    [condition_bytes: varBytes]
[witness_script: varBytes]
[control_block: varBytes]
[required_sequence: varint]
[required_locktime: varint]
```

All encodings use Bitcoin-standard varint/varBytes. Version bytes enable
forward-compatible evolution.

---

## 7. Key Files to Review

### Foundation (read first)

| File | What to look for |
|------|-----------------|
| `lib/arkscript/node.go` | Three node types, sealed interface, script compilation |
| `lib/arkscript/validate.go` | Four Ark invariants, recursive key/CSV detection |
| `lib/arkscript/standard_vtxo.go` | Standard 2-leaf construction, decode/roundtrip |
| `lib/arkscript/policy_template.go` | Binary encoding, `Compile()` to taproot tree |

### vHTLC and Spending

| File | What to look for |
|------|-----------------|
| `lib/arkscript/vhtlc.go` | 6-leaf construction, spend path builders per closure |
| `lib/arkscript/spend_path.go` | SpendPath with conditions, witness assembly |
| `lib/arkscript/settlement.go` | Auth/forfeit pairing via AST normalization |

### Tree and Compilation

| File | What to look for |
|------|-----------------|
| `lib/arkscript/tree.go` | Balanced binary tree, merkle proofs, control blocks |
| `lib/arkscript/compose.go` | External root composition (Taproot Assets) |
| `lib/arkscript/vtxo.go` | VTXOPolicy convenience wrapper, CollabSpendInfo/ExitSpendInfo |

### Tests

| File | What to look for |
|------|-----------------|
| `lib/arkscript/golden_test.go` | Byte-for-byte backward compatibility vectors |
| `lib/arkscript/validate_test.go` | Rejection of invalid policies (missing CSV, etc.) |
| `lib/arkscript/vhtlc_test.go` | Full vHTLC lifecycle with all 6 paths |

---

## 8. Migration Boundaries

This PR introduced the `lib/arkscript` foundation package alongside the
legacy `lib/scripts` package, without changing DB/wire formats up front —
those changes shipped in the follow-up integration PRs. `lib/scripts` has
since been removed entirely: checkpoint, forfeit, and OOR transaction
construction all build their taproot artifacts through `lib/arkscript` now.

The golden test vectors in `golden_test.go` were originally generated by
comparing against the `lib/scripts` implementation, proving **byte-identical**
output keys, scripts, and control blocks and confirming that the new package
was a drop-in replacement. Now that `lib/scripts` is gone, `golden_test.go`
pins those vectors as a frozen regression suite — they must not change unless
the VTXO output format is intentionally changing (a breaking change).

The key improvement is that the checkpoint PSBT can now carry the tap-tree
metadata directly (via the Ark-specific tap tree encoding), making resume
after crash more self-contained than the older sidecar story.
