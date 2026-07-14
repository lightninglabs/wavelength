# arkscript Specification

**Status:** Draft (pre-1.0)
**Package:** `lib/arkscript`
**Scope:** Semantic tapscript policy compiler and runtime for Ark protocol
outputs.
**Audience:** Auditors, implementors, and reviewers who need an authoritative
account of what arkscript compiles to, what its on-disk/on-wire encodings
look like, what invariants it enforces, and what it deliberately does not.

---

## 1. Overview

arkscript is the single source of truth for the taproot outputs used in the
Ark protocol on this client. It provides:

- A **sealed AST** of spending conditions (`Node`: `Multisig`, `CSV`,
  `Condition`).
- A **policy layer** that composes named leaves (`PolicyTemplate`,
  `LeafTemplate`) with stable binary encoding.
- **Standard shapes** for the three taproot outputs the protocol uses: the
  VTXO, the checkpoint output, and the vHTLC.
- **Spend metadata** in the form of `SpendInfo` (script + control block) and
  `SpendPath` (script + control block + nSequence/nLockTime + condition
  witnesses).
- **Validation** of the admission-surface invariants (`ValidatePolicy`,
  `ValidateStandardVTXOPolicy`).

Downstream packages (`lib/tree`, `lib/tx/*`, `oor`, `round`, `vtxo`,
`waved`, `db`) consume arkscript's compiled artifacts to build, sign, and
validate Ark transactions. Only arkscript is allowed to produce tapscript
bytes for protocol outputs; no other package is permitted to hand-roll
taproot scripts for VTXO / checkpoint / vHTLC outputs.

Related documents:

- `docs/policy_arkscript_review_guide.md` — reviewer-focused walkthrough.
- `ARCHITECTURE.md` — where arkscript sits in the package graph.
- `docs/mailbox_architecture.md` — the transport these outputs flow over.

---

## 2. AST

### 2.1 Node interface

```go
type Node interface {
    Script() ([]byte, error)
    nodeSealed()
}
```

The `nodeSealed` marker makes `Node` a closed interface: only types defined in
`lib/arkscript` can implement it. This prevents third-party node types from
being smuggled into `PolicyTemplate.Leaves`, which in turn means every leaf
script we compile has a known shape.

Defined in: `lib/arkscript/node.go:14-22`.

### 2.2 Concrete node kinds

#### `Multisig` — N-of-N signature check

```go
type Multisig struct {
    Keys []*btcec.PublicKey  // order matters
}
```

Script encoding (from `scriptChecksig`, `lib/arkscript/node.go:50-65`):

```
<k0> OP_CHECKSIGVERIFY <k1> OP_CHECKSIGVERIFY ... <kn-1> OP_CHECKSIG
```

Keys are emitted as 32-byte x-only (BIP-341 requirement) via
`schnorr.SerializePubKey`. All keys in the set must produce a valid schnorr
signature for the script to succeed. Signatures in the witness stack must be
provided in **reverse** of key order (the last key's CHECKSIG is evaluated
first because the script VM is stack-based, and signatures sit below the
witness script on the stack).

#### `CSV` — relative timelock gate

```go
type CSV struct {
    Lock  uint32  // BIP-68 encoded sequence value
    Inner Node
}
```

Script encoding (from `CSV.Script`, `lib/arkscript/node.go:82-101`):

```
<inner> <lock> OP_CHECKSEQUENCEVERIFY OP_DROP
```

At runtime the spending transaction's input `nSequence` must satisfy the
BIP-68 relative-lock relation to the UTXO's confirmation height. Callers
read the required sequence from a `SpendPath.RequiredSequence` computed by
`DeriveSequence` (see §6).

#### `Condition` — generic predicate prefix

```go
type Condition struct {
    Predicate []byte  // canonical script fragment (enforces its own VERIFY)
    Inner     Node
}
```

Script encoding (from `Condition.Script`, `lib/arkscript/node.go:120-141`):

```
<predicate> <inner>
```

`Condition` is the extension point for non-signature preconditions (hash
locks, absolute locktimes, payment-hash preimages). Helper builders live in
`lib/arkscript/node.go:148-190`:

- `Hash160Condition(hash []byte)` — `HASH160 <hash> EQUALVERIFY`.
- `AbsoluteLockTimeCondition(lock uint32)` — `<lock> CLTV DROP`.
- `PaymentHash160Condition(paymentHash)` — Lightning payment-hash predicate
  that also enforces the 32-byte preimage-size rule.

The predicate bytes are opaque to the AST walker for the purposes of
`ContainsKey` and key extraction (`lib/arkscript/validate.go:208-218`). This
is intentional: the AST reasons about *who can sign*, not about *what
hashlock values are in play*.

### 2.3 What is NOT in the AST

- No `OR` nodes. Ark expresses alternatives as separate tap leaves; the
  tapscript merkle tree already provides the OR semantics.
- No `AND` nodes. Multisig is N-of-N; chain multiple signatures inside a
  single `Multisig`. CSV-gated signatures compose via `CSV{Inner: Multisig}`.
- No custom opcode escape hatch. A future node kind requires a code change
  in `lib/arkscript` and an encoding version bump (see §3.5).

---

## 3. PolicyTemplate

### 3.1 Shape

```go
type LeafTemplate struct {
    Node Node
}

type PolicyTemplate struct {
    Leaves []LeafTemplate
}
```

Defined in: `lib/arkscript/policy_template.go:127-222`.

`PolicyTemplate` preserves the author's leaf order. The canonical taproot
tree ordering (by leaf version, then lexicographic script bytes) is applied
at `BuildTree` time, not at encode time. Two policies with identical leaves
in different input orders produce different encoded bytes but identical
compiled output keys — callers should treat the encoded form as *structural*
and the output key as *semantic*.

### 3.2 Binary encoding — PolicyTemplate

```
+--------+-----------+------------------+
| 1 byte | varint    | N × leaf blob    |
| ver    | leafCount | (each var-length)|
+--------+-----------+------------------+
```

Where:

- `ver` is `policyTemplateVersion` = `1` (`lib/arkscript/policy_template.go:22`).
- `leafCount` is a Bitcoin `VarInt`.
- each leaf blob is a length-prefixed `LeafTemplate` encoding (below).

Implemented by `PolicyTemplate.Encode` / `DecodePolicyTemplate`
(`lib/arkscript/policy_template.go:276-411`).

### 3.3 Binary encoding — LeafTemplate

```
+--------+------------------+
| 1 byte | length-prefixed  |
| ver    | node encoding    |
+--------+------------------+
```

Where `ver` is `leafTemplateVersion` = `1` (`lib/arkscript/policy_template.go:18`).

### 3.4 Binary encoding — Node

Each node is `kind(1) || payload`. Payload layout by kind:

| Kind            | byte | Payload                                                                |
|-----------------|------|------------------------------------------------------------------------|
| `Multisig`      | `1`  | `varint(keyCount) || key[0..32] × keyCount`                            |
| `CSV`           | `2`  | `varint(lock) || varbytes(child-node-encoding)`                        |
| `Condition`     | `3`  | `varbytes(predicate) || varbytes(child-node-encoding)`                 |

Implemented by `EncodeNode` / `decodeNodePayload` / `decodeLockedNode`
(`lib/arkscript/policy_template.go:412-645`).

> **Note on key encoding:** Multisig currently encodes keys as 32-byte
> x-only (via `schnorr.SerializePubKey`). This round-trips lossy for
> y-parity: a compressed key with odd parity that is encoded and decoded
> comes back with synthesised even parity. Upstream layers that need
> lossless parity (DB columns, wire descriptors) persist the compressed
> form in a separate column and prefer it over the lifted-from-policy
> value during rehydration. See lightninglabs/wavelength#252 for the
> design discussion about whether to switch to lossless 33-byte
> compressed encoding in the policy template itself.

### 3.5 Decode limits

Every decode entry point bounds allocation and CPU against
attacker-controlled input. Constants live in
`lib/arkscript/policy_template.go:26-56`:

| Constant                   | Value  | Bounded thing                                  |
|----------------------------|--------|------------------------------------------------|
| `MaxPolicyTemplateBytes`   | 64 KiB | Raw size of a full policy blob.                |
| `MaxLeafTemplateBytes`     | 16 KiB | Raw size of a single leaf blob.                |
| `MaxPolicyLeaves`          | 32     | Leaves in one `PolicyTemplate`.                |
| `MaxPolicyDepth`           | 16     | AST recursion depth (Condition/CSV nesting).   |
| `MaxPolicyNodes`           | 256    | Total AST node count across one decode call.   |
| `MaxMultisigKeys`          | 64     | Keys inside one `Multisig` node.               |

Depth and node-count are shared across all leaves of a single policy decode
via the internal `decodeBudget` — an adversary cannot claim `MaxPolicyNodes`
per leaf. This was the fix for review finding H-2; tests in
`lib/arkscript/policy_template_test.go:TestDecode*RejectsDeep* /
RejectsTooMany* / BudgetSharedAcrossLeaves`.

Issue lightninglabs/wavelength#253 tracks the workload-driven tuning of
these numbers over time.

### 3.6 Versioning

`policyTemplateVersion` and `leafTemplateVersion` are independent single-byte
fields. A version bump implies:

- Existing consumers reject the blob with `unknown ... version N`.
- Producers must emit the new version only after every cluster consumer has
  been upgraded; otherwise rolling-upgrade produces hard failures.

Extensions that can be added WITHOUT a version bump:

- New `Condition` predicate byte strings. Predicates are opaque to the
  AST, so a new HTLC-style condition is just a new builder function in
  `lib/arkscript` that emits canonical script fragments.

Extensions that REQUIRE a version bump:

- New node kinds (add a new `nodeKind` constant).
- Changes to `Multisig` key encoding (e.g. switching to compressed 33-byte
  per #252).
- Any size/shape change to an existing payload.

---

## 4. CompiledPolicy and BuildTree

### 4.1 Compilation

`PolicyTemplate.Compile` (`lib/arkscript/policy_template.go:225-249`) walks
each leaf's AST, calls `Node.Script()`, wraps the bytes in a
`txscript.NewBaseTapLeaf`, and hands the result to `BuildTree`.

```go
type CompiledPolicy struct {
    InternalKey  *btcec.PublicKey
    Leaves       []PolicyLeaf
    RootHash     []byte
    leafHashes   []chainhash.Hash  // unexported: for control-block build
    merkleProofs [][]chainhash.Hash
}
```

Defined in `lib/arkscript/tree.go:33-53`.

### 4.2 Canonical leaf ordering

`sortLeaves` (`lib/arkscript/tree.go:176-198`) sorts by
`(LeafVersion, Script)` — leaf version first, then lexicographic by script
bytes. This gives a deterministic tap-tree shape independent of how the
caller constructed the `PolicyTemplate`. BIP-341 further sorts child hashes
at each merkle branch via `tapBranchHash` (`lib/arkscript/tree.go:296-305`),
so the tree root is stable under reordering at both layers.

### 4.3 Internal key: ARK NUMS point

Every Ark output uses the Ark NUMS internal key
(`lib/arkscript/nums.go`). `BuildTree` enforces this:

```go
if !internalKey.IsEqual(&ARKNUMSKey) {
    return nil, fmt.Errorf("internal key must be the Ark NUMS key")
}
```

(`lib/arkscript/tree.go:210-215`)

Because the NUMS key is provably unspendable via key-path, every Ark output
is script-path-only. This is the foundational invariant that the "no
operator-unilateral spend" admission check (§7) can build on.

### 4.4 Control block derivation

`CompiledPolicy.SpendInfo(leafIndex)` (`lib/arkscript/tree.go:61-86`)
returns a `*SpendInfo` with the witness script and a freshly built BIP-341
control block:

- Control byte = `LeafVersion | (outputKeyParity << 0)`.
- Internal key = 32-byte x-only of `ARKNUMSKey`.
- Inclusion proof = sibling hashes from leaf to root, collected during
  tree construction.

Verifying a control block is the job of the caller (typically via
`txscript.ParseControlBlock` + `ctrlBlock.RootHash(witnessScript)`); the
binding check `SpendPath.VerifyBindsToPkScript` is implemented in
`lib/arkscript/spend_path.go` and used anywhere the client accepts a
caller-supplied control block (see §9).

---

## 5. Standard shapes

### 5.1 VTXO — `StandardVTXOTemplate`

```go
Leaves: [
  Multisig{owner, operator},                       // collab
  CSV{Lock: exitDelay, Inner: Multisig{owner}},    // unilateral exit
]
```

Defined in `lib/arkscript/standard_vtxo.go:36-83`.

Compiled output key computed by `VTXOTapKey` (`lib/arkscript/spend_helpers.go`);
canonical P2TR pkScript by `EncodeStandardVTXOArtifacts`
(`lib/arkscript/standard_vtxo.go:96-122`) — the helper used by the wallet
when constructing recipient descriptors without touching the tree-layer
signing key.

### 5.2 Checkpoint — `CheckpointTapScript`

Two-leaf tree (`lib/arkscript/checkpoint.go:43-75`):

```go
Leaves: [
  CSV{Lock: CSVDelay, Inner: Multisig{operatorKey}},   // operator CSV unroll
  owner-supplied leaf script,                           // collab
]
```

The "owner-supplied" leaf is a raw script blob rather than an AST node at
this layer — the checkpoint builder trusts the upstream `oor` pipeline to
have produced a leaf that matches the owner's collab spend path on the
underlying VTXO.

### 5.3 vHTLC — `NewVHTLCPolicy`

Six-leaf tree (`lib/arkscript/vhtlc.go`). Leaf ordering matches
the source docstring so leaves 1–3 are collaborative (receiver
and/or server participate) and leaves 4–6 are the client-side
unilateral exits gated by a CSV delay:

| # | Name                            | Structure                                                                 |
|---|---------------------------------|---------------------------------------------------------------------------|
| 1 | Claim                           | `Condition{PaymentHash, Multisig{receiver, server}}`                      |
| 2 | Refund                          | `Multisig{sender, receiver, server}`                                      |
| 3 | RefundWithoutReceiver           | `Condition{CLTV(RefundLocktime), Multisig{sender, server}}`               |
| 4 | UnilateralClaim                 | `CSV{UnilateralClaimDelay, Condition{PaymentHash, Multisig{receiver}}}`   |
| 5 | UnilateralRefund                | `CSV{UnilateralRefundDelay, Multisig{sender, receiver}}`                  |
| 6 | UnilateralRefundWithoutReceiver | `CSV{UnilateralRefundWithoutReceiverDelay, Condition{CLTV(RefundLocktime), Multisig{sender}}}` |

Full cross-party behaviour is documented inline at
`lib/arkscript/vhtlc.go` — this spec only enumerates the shape.

Leaf 6 gates the sender's unilateral refund on both the Ark CSV *and* the
invoice/vHTLC CLTV. The CLTV gate is the cross-protocol safety property
that keeps the sender from racing a still-pending Lightning payment: if the
invoice expiry has not been reached, the receiver may still claim with the
preimage on either Ark or Lightning, and a sender who could exit before
that deadline would be able to double-spend the funds the receiver has
already committed to. The CSV alone enforces only that recovery follows the
descriptor's local timeout — it does not bind the sender to the Lightning
HTLC expiry. Pairing CSV and CLTV mirrors the
[swapdk-server](https://github.com/lightninglabs/swapdk-server) sender-side
expectation that the unilateral refund-without-receiver leaf is reachable
only after the invoice has expired.

---

## 6. SpendPath

### 6.1 Shape

```go
type SpendPath struct {
    *SpendInfo                     // WitnessScript + ControlBlock
    RequiredSequence uint32        // BIP-68 (0xffffffff = no constraint)
    RequiredLockTime uint32        // nLockTime (0 = no constraint)
    Conditions       [][]byte      // extra witness items before script
}
```

Defined in `lib/arkscript/spend_path.go:22-38`.

### 6.2 Witness stack order

`SpendPath.Witness(sigItems...)` (`lib/arkscript/spend_path.go:258-274`)
assembles:

```
[ sig_n, sig_{n-1}, ..., sig_0,    // reverse of key order (§2.2)
  condition_0, condition_1, ...,   // in order, as produced
  witnessScript,
  controlBlock ]
```

Any caller that produces its own witness manually MUST follow this exact
order — the script VM consumes top-of-stack first, so signatures need to
land in reverse key order.

### 6.3 Tx-context requirements

- `RequiredSequence` comes from `DeriveSequence(node)`
  (`lib/arkscript/tree.go:146` via `SpendPathForNode`). For CSV-gated paths
  it returns the `CSV.Lock` value; for non-CSV paths it returns
  `0xffffffff` (opt-out of BIP-68).
- `RequiredLockTime` comes from `ExtractAbsoluteLockTime(node)`. Set only
  for leaves with an `AbsoluteLockTimeCondition`.
- If `RequiredLockTime != 0` but `RequiredSequence == 0xffffffff`, the
  builder drops sequence to `0xfffffffe` so CLTV is actually enforced
  (BIP-65 requires `nSequence != 0xffffffff`).

These values MUST be applied by the spending-tx builder to the
corresponding `TxIn.Sequence` and `TxIn.TxOut`'s parent tx `LockTime`;
otherwise the witness passes the VM at signature check but fails at the
CSV/CLTV op-code.

### 6.4 Binary encoding

```
+--------+---------+----------+-----------------+---------+------+------+
| 1 byte | varint  | N × item | witnessScript   | control | seq  | lock |
| ver    | condCnt | varbytes | varbytes        | varbytes| varint| varint|
+--------+---------+----------+-----------------+---------+------+------+
```

Where `ver = spendPathVersion = 1` (`lib/arkscript/spend_path.go:17`). The
decoder (`DecodeSpendPath`) caps `conditionCount` at 64 (`maxConditions`
at `lib/arkscript/spend_path.go:190`).

### 6.5 Condition witness (durable persistence)

The OOR actor durably persists a `TransferInputSnapshot` that includes a
`ConditionWitness [][]byte`. Encoded by
`oor/actor_durable_message.go encodeConditionWitness`:

```
varint(count) || N × varbytes(item)
```

Caps (`oor/actor_durable_message.go:1365-1381`):

| Constant                      | Value | Bounded thing                             |
|-------------------------------|-------|-------------------------------------------|
| `maxConditionWitnessItems`    | 64    | Item count.                               |
| `maxConditionWitnessItemBytes`| 520   | Per-item size (Bitcoin standard script-element max). |

Encoder and decoder enforce the same caps so in-memory state cannot drift
from what the persisted form can represent.

---

## 7. Validation

### 7.1 `ValidatePolicy` — structural

Signature and invariants (`lib/arkscript/validate.go:25-107`):

```go
func ValidatePolicy(nodes []Node, opts PolicyValidationOpts) error

// Invariants:
//  1. At least one operator-containing leaf (collab).
//  2. At least one non-operator leaf (exit).
//  3. No leaf permits operator-unilateral spend.
//  4. Every non-operator leaf is CSV-gated.
//  5. If opts.MinExitDelay > 0: smallest exit delay >= opts.MinExitDelay.
```

`MinExitDelay` is optional here (zero = skip). Use
`ValidateStandardVTXOPolicy` when that check must be mandatory (see §7.2).

Invariant 3 is enforced by `rejectOperatorUnilateral`: every `Multisig`
node reachable from every leaf's AST must contain at least one non-operator
key. This rejects `Multisig{operator}` and `CSV{_, Multisig{operator}}`
style leaves, which were the H-4 attack shape.

### 7.2 `ValidateStandardVTXOPolicy` — strict admission

```go
func ValidateStandardVTXOPolicy(nodes []Node,
    operatorKey *btcec.PublicKey, minExitDelay uint32) error
```

Requires `minExitDelay > 0` fail-closed, then delegates to
`ValidatePolicy`. Intended as the admission check for any surface that
consumes a **standard Ark VTXO recipient**. As of this writing,
`waved/rpc_server.go`'s recipient-output path (`resolveRecipientOutput`,
`validateOutputPolicyTemplate`) does not call this helper directly —
it pre-filters the shape via `DecodeStandardVTXOParams` and falls back to
structural `ValidatePolicy` for both standard and custom shapes. Custom
shapes (vHTLC claims from daemon RPC) also use the structural
`ValidatePolicy`.

### 7.3 Invariants that are NOT enforced here

- **Policy-to-pkScript binding** is NOT checked by either validator —
  that's the caller's job via `PolicyTemplate.MatchesPkScript`.
- **Control-block-to-pkScript binding** lives on `SpendPath` (§9.2).
- **Spec-level constraints** (e.g. "MinExitDelay comes from operator
  terms") are imposed by the admission site, not by `arkscript`.

---

## 8. Encoding stability and extension guidelines

| Change                                                                 | Ok without version bump? |
|------------------------------------------------------------------------|---------------------------|
| Add a new `Condition` predicate helper.                                | Yes.                      |
| Add a new standard shape (e.g. multi-party vHTLC).                     | Yes, if it only composes existing nodes. |
| Raise a `Max*` cap.                                                    | Yes (lenience is backward compatible). Announce it so operators can re-baseline. |
| Lower a `Max*` cap.                                                    | No — breaks existing durable blobs. |
| Add a new `nodeKind`.                                                  | Yes on write-side only — decoders in earlier versions will reject. Coordinate rollout. |
| Change `Multisig` key encoding (e.g. 33-byte compressed per #252).     | No — bump `policyTemplateVersion` + `leafTemplateVersion`. |
| Change the `SpendPath` binary shape.                                   | No — bump `spendPathVersion`. |
| Change `Node.Script()` output for an existing node kind.               | No — this changes compiled output keys. |

---

## 9. Security considerations

### 9.1 DoS decode-bomb defense

The policy decoder operates on attacker-controlled bytes in at least these
call sites (all reachable from unauthenticated network input):

- `round/from_proto.go` — peer-supplied JoinRound messages.
- `lib/types/codec.go` — durable join-auth TLV.
- `waved/rpc_server.go` (multiple) — local RPC.
- `waved/wallet_ops.go` — custom OOR inputs.
- `oor/transfer_inputs.go` — OOR session state.
- `db/vtxo_store.go`, `db/round_store.go` — rehydrate from DB.

Every call goes through `arkscript.DecodePolicyTemplate`, which caps
bytes upfront and then threads a shared depth/node-count budget through
recursion (§3.5). A crafted blob exceeding any cap fails fast before any
meaningful allocation.

### 9.2 Signature-oracle binding

Signers that accept a caller-supplied witness script + control block MUST
verify the control block commits to a tap tree whose output key is the
declared pkScript. `SpendPath.VerifyBindsToPkScript`
(`lib/arkscript/spend_path.go:60-116`) does this:

1. Parse the control block.
2. Assert internal key is `ARKNUMSKey`.
3. Compute the root hash from `ctrlBlock.RootHash(witnessScript)`.
4. Compute the taproot output key via
   `txscript.ComputeTaprootOutputKey(ARKNUMSKey, rootHash)`.
5. Compare the derived P2TR script to the supplied pkScript.

Wired into:

- `waved/wallet_ops.go BuildCustomTransferInputs` — RPC entry.
- `oor/checkpoint_sign.go signCustomCheckpointPSBT` — defense-in-depth at
  the signing site.

Without this check a caller could hand the daemon a control block for an
arbitrary tapscript and obtain a Schnorr signature over it under the VTXO
owner key.

### 9.3 Operator-unilateral-spend rejection

`ValidatePolicy` invariant 3 rejects any `Multisig` leaf whose sole
participant is the operator (§7.1). This catches the specific H-4 attack
shape `[collab, csv_exit, multisig(operator_only)]` and every trivially
equivalent nesting (`CSV{_, Multisig{operator_only}}`,
`Condition{_, Multisig{operator_only}}`). It does NOT catch more exotic
constructions where the operator can unilaterally produce the predicate
witness that unlocks a hash-locked single-operator multisig — such
constructions are explicitly outside the AST's reasoning surface.

### 9.4 Pubkey encoding parity

See issue lightninglabs/wavelength#252. Summary: the current `Multisig`
encoder uses 32-byte x-only, so a `PolicyTemplate` round-trip is lossy
for y-parity. Consumers that need lossless parity (DB operator-key columns)
persist the 33-byte compressed form separately and prefer it over the
lifted-from-policy value on rehydration. A future wire-compatible fix may
switch to lossless 33-byte compressed in the policy template itself.

### 9.5 Admission gates are additive, not subtractive

`ValidatePolicy` and `ValidateStandardVTXOPolicy` enforce a MINIMUM set of
invariants. The admission surfaces in `waved/rpc_server.go` and
`waved/wallet_ops.go` layer additional policy-specific checks on top:

- Recipient outputs are pre-filtered to the standard VTXO shape via
  `DecodeStandardVTXOParams` (`resolveRecipientOutput`), then checked with
  structural `ValidatePolicy` plus a fail-closed non-zero
  operator-exit-delay gate (`validateOutputPolicyTemplate`).
- Custom OOR inputs use structural `ValidatePolicy` but also
  `MatchesPkScript` and `SpendPath.VerifyBindsToPkScript`.

Removing any of these upstream checks on the assumption that arkscript
alone is sufficient would reintroduce the attack shapes they were added
to close.

---

## 10. References

- BIP-341 — Taproot.
- BIP-342 — Tapscript.
- BIP-65 — `OP_CHECKLOCKTIMEVERIFY`.
- BIP-68 — Relative lock-time.
- BIP-112 — `OP_CHECKSEQUENCEVERIFY`.
- `docs/policy_arkscript_review_guide.md` — reviewer walkthrough.
- lightninglabs/wavelength#252 — pubkey encoding design.
- lightninglabs/wavelength#253 — DoS cap tuning.
