# RFC: Ark Closure AST and Canonical Taproot Policies (PR #58)

## Status
- Draft
- Normative keywords: MUST, MUST NOT, SHOULD, SHOULD NOT, MAY.

This document is both:
1) a specification for Ark taproot policies compiled from a restricted, typed
   Closure AST, and
2) the contract for what PR #58 will implement (doc-first; code follows).

## Abstract
Ark uses taproot script-path spends to enforce protocol-level spend policies
for:
- BTC-only VTXOs (operator-involving cooperative spends vs CSV-gated exits),
- checkpoint outputs used in OOR coordination, and
- future swap/submarine-style flows that rely on hashlocks and CLTV/CSV.

This RFC specifies a structural Closure AST and deterministic compilation
pipeline:
1) construct AST (no general-purpose script evaluation),
2) validate policy invariants,
3) compile to canonical tapscript leaves + canonical taproot tree, and
4) derive deterministic transaction-context requirements (sequence/locktime).

The key property is auditability: parties can verify policy structure without
evaluating scripts in an ad-hoc VM context.

## PR Scope (what #58 will do)
PR #58 MUST implement, at minimum:
- A `lib/arkscript` (name TBD) package that defines:
  - the Closure AST node set in this RFC,
  - validation for VTXO and checkpoint policies, and
  - canonical compilation (leaf encoding + canonical tree construction).
- A `SpendInfo` API that returns per-leaf script + control block + derived tx
  context requirements.
- Canonicalization rules pinned in-code (leaf ordering + tree shape) so output
  keys are reproducible across implementations.
- A tap tree encoding suitable for PSBT metadata for OOR finalization (see
  “PSBT extensions”).
- Tests covering canonicalization, invariants, and tx-context derivation.

PR #58 MAY additionally:
- include a minimal “policy composition” hook for Taproot Assets (attach Ark
  policy root as a sibling to an externally provided root), but MUST keep this
  behind a small API surface and MUST NOT attempt full tapd proof handling in
  v1.

PR #58 MUST NOT:
- implement a fake “EvaluateScriptToBool” VM helper,
- accept arbitrary raw scripts that bypass validation, or
- depend on library heuristics for taproot tree construction.

## Terminology
- **Policy**: a set of role-tagged leaves plus an internal key. Compiles to one
  P2TR output key and pkScript.
- **Leaf**/**Closure**: one tapscript leaf (script bytes + leaf version).
- **Operator key**: pubkey required for cooperative paths (Ark signer).
- **Owner key(s)**: non-operator keys that own the value being controlled.
- **Exit leaf**: CSV-gated leaf spendable without the operator key.
- **Collab leaf**: leaf that requires the operator key.

## Goals
- Deterministically reproduce taproot output keys across implementations.
- Enforce Ark invariants structurally (no raw-script escape hatches).
- Make CLTV/CSV spending requirements machine-readable and deterministic.
- Support swap-style policies via multiple leaves (no `IF/ELSE` required).

## Non-goals
- General-purpose Miniscript replacement.
- Covenant/introspection opcode support.
- Parsing of arbitrary tapscript (parsing is optional and limited).

## Model

### Policy families and roles
Roles are interpreted within a policy family.

**VTXO family roles**
- `collab`: MUST require the operator key.
- `exit`: MUST be CSV-gated and MUST NOT require the operator key.
- `custom`: additional leaves, subject to safety constraints.

**Checkpoint family roles**
- `unroll`: operator-controlled CSV leaf for checkpoint unroll.
- `owner`: owner leaf used to spend checkpoint outputs into an Ark tx.
- `custom`: discouraged in v1.

### Expression nodes (Closure AST)
The node set below MUST be sufficient to express required Ark policies:
- `Checksig(key)`: one schnorr signature required.
- `Multisig(keys, type)`: N-of-N schnorr signatures required.
- `CSV(lock, inner)`: relative timelock gate (BIP-68 + OP_CSV).
- `CLTV(lock, inner)`: absolute timelock gate (BIP-65 + OP_CLTV).
- `HashLock(algo, hash, inner)`: preimage gate (HASH160/SHA256).

Branching (`IF/ELSE`) is intentionally omitted. Swap-style semantics MUST be
modeled as multiple leaves.

## Canonical script encoding

### Common requirements
Encoders MUST:
- use x-only pubkeys (32 bytes) for schnorr checks,
- use minimal script-number encodings, and
- reject ambiguous encodings that would allow multiple byte strings to encode
  the same AST.

Parsers (if implemented) MAY accept a limited set of equivalent legacy opcode
arrangements, but compilation MUST emit exactly one canonical arrangement per
node type.

### `Checksig`
Canonical encoding:
```
<xonly_pubkey> OP_CHECKSIG
```

### `Multisig`
This RFC standardizes N-of-N only (all keys must sign).

Two encodings are allowed; implementations MUST support at least one.

**Type: `checksig` (CHECKSIGVERIFY chain)**
For keys `k[0..n-1]`:
```
<k0> OP_CHECKSIGVERIFY
<k1> OP_CHECKSIGVERIFY
...
<k[n-2]> OP_CHECKSIGVERIFY
<k[n-1]> OP_CHECKSIG
```

**Type: `checksigadd` (CHECKSIGADD + NUMEQUAL)**
For keys `k[0..n-1]`:
```
<k0> OP_CHECKSIG
<k1> OP_CHECKSIGADD
...
<k[n-1]> OP_CHECKSIGADD
<n> OP_NUMEQUAL
```

Key order is significant. For interoperability, key order MUST be specified by
the policy template.

### `CSV`
`CSV(lock, inner)` MUST compile to:
```
<inner>
<lock> OP_CHECKSEQUENCEVERIFY OP_DROP
```

The lock value MUST encode a valid BIP-68 relative locktime when interpreted as
an input sequence value.

### `CLTV`
`CLTV(lock, inner)` MUST compile to:
```
<lock> OP_CHECKLOCKTIMEVERIFY OP_DROP
<inner>
```

### `HashLock`
`HashLock(algo, hash, inner)` MUST compile to:

**HASH160**
```
OP_HASH160 <20-byte-hash> OP_EQUALVERIFY
<inner>
```

**SHA256**
```
OP_SHA256 <32-byte-hash> OP_EQUALVERIFY
<inner>
```

## Canonical policy compilation

### Leaf ordering
To compile a policy deterministically, implementations MUST:
1) compile each leaf expression into leaf script bytes,
2) sort leaves into a canonical order, then
3) build a canonical taproot tree from that ordered list.

Leaf ordering MUST be:
- primary: `family_role_rank`,
- secondary: lexicographic ordering of `leaf_script_bytes`.

Role ranks are pinned as:
- VTXO: `collab`=0, `exit`=1, `custom`=2.
- Checkpoint: `unroll`=0, `owner`=1, `custom`=2.

Leaf names are for selection/audit. Leaf names MUST NOT be required to derive
the same taproot output key.

### Tree construction
Canonical tree shape MUST be deterministic and independent of library
heuristics.

Define `BuildTree(leaves)` recursively over the canonical leaf list:
- If `n == 1`: the root is the single leaf hash.
- If `n > 1`:
  - split `left = leaves[0:n/2]`, `right = leaves[n/2:n]`,
  - compute `L = BuildTree(left)`, `R = BuildTree(right)`,
  - compute `root = TapBranchHash(min(L,R), max(L,R))`.

Leaf hashes MUST be BIP-341 tapleaf hashes using the base leaf version.

### Output key derivation
Given internal key `P` (x-only) and merkle root `m`, the taproot output key
MUST be `Q = TapTweak(P, m)` per BIP-341.

Key-path spends SHOULD be disabled. The invariant is:
- **Keypath must be unspendable** for Ark-controlled outputs.

PR #58 MUST implement a default unspendable internal key. PR #58 MAY also
support per-output unspendable internal keys when external proofs require
uniqueness, but any such scheme MUST be public, deterministic, and MUST NOT
depend on a secret scalar known to any party.

## Validation rules

### Locktime policy
Validation MUST enforce:
- CSV locktimes are valid BIP-68 encodings.
- If seconds-based relative locktimes are used, values MUST be multiples of 512
  seconds (BIP-68 granularity).
- Mixed CLTV types (height vs time) MUST be rejected for a single transaction
  when deriving tx-wide `nLockTime`.

### VTXO policy invariants
A VTXO policy MUST:
- contain at least one `collab` leaf that requires the operator key, and
- contain at least one `exit` leaf that:
  - is CSV-gated, and
  - does not require the operator key.

If multiple exit leaves exist, the minimum exit delay across them MUST meet the
protocol’s `MinExitDelay` requirement.

Custom leaves MUST NOT introduce an ungated spend path that is spendable
without the operator key, unless the policy explicitly validates it as an
approved swap-claim pattern.

### Checkpoint policy invariants (OOR)
A checkpoint output policy MUST:
- contain exactly one `unroll` leaf that:
  - is CSV-gated by the checkpoint unroll delay, and
  - is spendable only by the operator (or a dedicated unroll key), and
- contain at least one `owner` leaf that is in the approved AST subset.

## Deterministic transaction-context derivation
Script satisfaction depends on transaction fields:
- CSV depends on the spending input sequence (BIP-68).
- CLTV depends on the spending transaction locktime (BIP-65) and on the
  spending input sequence being non-final.

Builders MUST deterministically set per-input `nSequence` and tx-wide
`nLockTime` based on selected leaves.

### Per-leaf requirements
For a compiled leaf, derive:
- `RequiredSequence`:
  - if the leaf contains `CSV(lock, ...)`, this MUST be the exact BIP-68
    sequence encoding for `lock`,
  - else if the leaf contains `CLTV(...)`, this MUST be `0xfffffffe`,
  - else this SHOULD be `0xffffffff`.
- `RequiredLockTime`:
  - if the leaf contains `CLTV(lock, ...)`, this MUST be `lock`,
  - else it MUST be `0`.

### Transaction-wide rules
Given inputs `i=0..k-1`, each with selected leaf requirements:
- `nLockTime` MUST be the maximum `RequiredLockTime` across all inputs.
- Any input that requires CLTV MUST have a non-final `nSequence`.
- Mixed CLTV types (height vs time) MUST be rejected.

## PSBT extensions (profile)

### Tap tree encoding
Implementations MAY persist taproot leaf lists in PSBT unknowns to ensure the
same leaf set can be reconstructed during finalization (OOR).

If used, the key SHOULD be namespaced (e.g., `ark/taptree`). For compatibility
with existing Ark tooling, the un-namespaced key `taptree` MAY be used while
transitioning.

Encoding MUST be:
- leaf count (compact size uint),
- for each leaf:
  - depth (1 byte),
  - leaf version (1 byte),
  - script length (compact size uint),
  - script bytes.

Depth MUST be measured against the canonical tree construction in this RFC.

### Condition witness encoding
For hashlock leaves that require a preimage witness element, implementations
MAY persist witness material in PSBT unknowns under a stable key (e.g.,
`ark/condition`) using standard witness serialization.

## Assets composition (Taproot Assets)
Taproot Assets introduces additional roots (asset commitment roots) that must
be combined with Ark spend-policy leaves to reproduce on-chain taproot keys.

PR #58 MUST define a composition API that can:
- compile an Ark policy into a policy root, and
- combine it as a sibling with an externally provided root:
  - `combined = TapBranchHash(min(policyRoot, extRoot),
    max(policyRoot, extRoot))`,
  - output key tweak uses `combined` as the merkle root.

PR #58 SHOULD treat full tapd proof reproduction as out-of-scope for v1, but
MUST keep the hash-level composition deterministic and testable.

## API surface (informative)
The implementation SHOULD expose:
- typed builders for standard VTXO and checkpoint templates,
- `Compile()` returning:
  - P2TR pkScript/output key,
  - ordered leaf list,
  - per-leaf `SpendInfo` (script, control block, tx-context requirements),
- an optional `CompileWithSiblingRoot(extRoot)` for assets composition.

Parsing arbitrary tapscript is out-of-scope. If parsing is provided, it MUST be
limited to scripts previously produced by this compiler or a tightly-scoped
legacy subset with canonical re-encoding.

## Implementation plan (incremental)
1) Implement AST nodes + canonical script encoder.
2) Implement canonical leaf ordering + canonical tree builder (with control
   blocks).
3) Implement VTXO and checkpoint validators.
4) Implement tx-context derivation + `SpendInfo`.
5) Implement PSBT tap tree encoding helpers (writer/reader).
6) Add assets composition hook (root sibling combination).
7) Add tests + golden vectors.

## Testing plan
Unit tests:
- Canonical script encoding per node type.
- Leaf ordering and canonical tree shape (root + per-leaf control blocks).
- Validation for VTXO + checkpoint invariants.
- Tx-context derivation: per-leaf requirements + tx-wide aggregation rules.

Property tests (rapid):
- Policies that satisfy invariants compile deterministically and validate.
- Policies that violate invariants fail with expected error classes.

Golden tests:
- Known-good vectors for the standard VTXO template (output key + leaf scripts).
- Stable tap tree encoding bytes for OOR PSBT metadata.

Commands:
- `make unit pkg=./... timeout=5m`
- `make lint`

## Security considerations
- Keypath spends SHOULD be disabled via an unspendable internal key.
- Unsupported opcodes MUST be rejected in parsed inputs.
- Custom leaves are dangerous; the allowed custom subset SHOULD remain small
  and structurally validated.
