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
- A `client/lib/arkscript` package that defines:
  - the Closure AST node set in this RFC,
  - validation for VTXO and checkpoint policies, and
  - canonical compilation (leaf encoding + canonical tree construction).
- A `SpendInfo` API that returns per-leaf script + control block + derived tx
  context requirements. The `SpendInfo` type MUST include:
  - `WitnessScript []byte` - the tapscript leaf script bytes,
  - `ControlBlock []byte` - the control block for script-path spending,
  - `RequiredSequence uint32` - the BIP-68 sequence value required for this leaf,
  - `RequiredLockTime uint32` - the nLockTime value required for this leaf.
- Canonicalization rules pinned in-code (leaf ordering + tree shape) so output
  keys are reproducible across implementations.
- A tap tree encoding suitable for PSBT metadata for OOR finalization (see
  “PSBT extensions”).
- Tests covering canonicalization, invariants, and tx-context derivation.

### Repo anchors (where this lands)
PR #58 MUST:
- Add the new implementation under `client/lib/arkscript/`.
- Keep `client/lib/scripts/` as the stable public surface for other
  repositories (notably `darepo` imports `github.com/lightninglabs/darepo-client/lib/scripts`).
- Make `client/lib/scripts/vtxo.go` a thin wrapper over `client/lib/arkscript`
  for default VTXO construction and spend metadata, keeping existing exported
  function signatures stable where feasible.
- Reuse the existing NUMS/unspendable key machinery in `client/lib/scripts/nums.go`
  as the default internal key provider.
- The NUMS key definition in `client/lib/scripts/nums.go` is the canonical
  source for the scripts package. Packages that would create an import cycle
  by importing `lib/scripts` (such as `lib/arkscript`) MAY locally redeclare
  the same NUMS hex constant to break the cycle. Any such redeclaration MUST
  remain byte-for-byte identical to the value in `lib/scripts/nums.go` and
  SHOULD reference that file in a comment.

PR #58 SHOULD document and/or update these call sites to use the new package
internally:
- `client/lib/scripts/*` (default VTXO tapscripts and spend info helpers).
- Any checkpoint and assets integration entry points, if present in this repo
  at the time of implementation, otherwise expose library APIs ready for those
  packages to adopt.

### Compatibility contract (default VTXO template)
To avoid breaking deterministic addresses and on-chain expectations, PR #58
MUST preserve the standard VTXO template behavior:
- The default VTXO output key, leaf scripts, and control blocks MUST be
  byte-for-byte identical to the current behavior produced by
  `client/lib/scripts/VTXOTapScript` for the same `(owner, operator, delay)`.
- This MUST be enforced via golden tests derived from the current implementation.

**Implementation ordering**: Before any refactoring begins, PR #58 MUST create
golden test vectors that capture:
- Default VTXO output key for known `(owner, operator, delay)` test inputs,
- Exact script bytes for exit and collab leaves,
- Control block bytes for each leaf.

These vectors MUST be generated from the current implementation and used to
validate the new implementation produces byte-identical output. Golden tests
MUST be the first deliverable (implementation plan step 0).

PR #58 MAY additionally:
- include a minimal “policy composition” hook for Taproot Assets (attach Ark
  policy root as a sibling to an externally provided root), but MUST keep this
  behind a small API surface and MUST NOT attempt full tapd proof handling in
  v1.

PR #58 MUST NOT:
- implement a fake "EvaluateScriptToBool" VM helper,
- accept arbitrary raw scripts that bypass validation, or
- depend on library heuristics for taproot tree construction.

### Legacy closure migration
The existing `lib/closure` package contains `ConditionMultisigClosure` and
`ConditionCSVMultisigClosure` types that accept arbitrary `Condition []byte`
fields and use an `EvaluateScriptToBool()` VM helper. PR #58 MUST either:
- (a) remove these types from the public API and document the migration path
  to the new `HashLock` AST node for preimage-gated spending conditions, OR
- (b) explicitly mark these types as deprecated and out-of-scope for the new
  arkscript package, with a clear warning that they bypass structural
  validation.

The `EvaluateScriptToBool()` function in `lib/closure/script.go` MUST be
removed or deprecated as part of PR #58.

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

### Composition: `CSV(lock, Checksig(key))`
The default VTXO exit leaf uses a CSV-gated single-key signature. The AST
composition `CSV(lock, Checksig(key))` MUST produce byte-identical scripts to
the current implementation (verified by golden tests):
```
<xonly_key> OP_CHECKSIG <lock> OP_CHECKSEQUENCEVERIFY OP_DROP
```

This is the canonical exit leaf for the default VTXO template. The inner
expression (`Checksig`) executes first, then the CSV timelock is verified.

### `Multisig`
This RFC standardizes N-of-N only (all keys must sign).

Two encodings are allowed; implementations MUST support at least one.
PR #58 MUST standardize on the `checksig` (CHECKSIGVERIFY-chain) encoding for
v1 to match current Ark VTXO scripts.

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

**Default VTXO template key order**: For the default VTXO collab leaf, key
order MUST be `[owner, operator]` to match the current implementation. The
resulting 2-of-2 script MUST be:
```
<owner_xonly> OP_CHECKSIGVERIFY <operator_xonly> OP_CHECKSIG
```

This is the canonical collab leaf for the default VTXO template.

### `CSV`
`CSV(lock, inner)` MUST compile to:
```
<inner>
<lock> OP_CHECKSEQUENCEVERIFY OP_DROP
```

This suffix ordering (inner expression first, then CSV check) matches the
current implementation. The inner expression evaluates first, leaving its
result on the stack, then the CSV timelock is verified and the lock value
is dropped.

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

The current implementation already follows this ordering: collab leaf at index
0, exit (timeout) leaf at index 1. This is verified by golden tests in
`lib/arkscript/golden_test.go`.

Leaf names are for selection/audit. Leaf names MUST NOT be required to derive
the same taproot output key.

### Tree construction
Canonical tree shape MUST be deterministic and independent of library
heuristics.

**Backward compatibility verification**: Before implementing the canonical tree
builder, PR #58 MUST verify that for the default 2-leaf VTXO case, btcd's
`txscript.AssembleTaprootScriptTree()` produces the same merkle root as the
`BuildTree()` algorithm defined below. If they differ, the `BuildTree()`
algorithm MUST be adjusted to match the current behavior for backward
compatibility with existing on-chain VTXOs.

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

### Structural constraints (to make tx-context derivation unambiguous)
PR #58 MUST restrict timelock composition per leaf:
- Each leaf MUST contain at most one effective CSV lock and at most one
  effective CLTV lock.
- Timelocks MUST apply to the entire spending condition: conceptually, CSV and
  CLTV are treated as outer gates that wrap the leaf’s inner expression.
- If both CSV and CLTV are present, compilation MUST treat them as a gate-set
  and emit the canonical ordering:
  - CLTV prefix (if present),
  - inner expression,
  - CSV suffix (if present).

This constraint ensures `RequiredSequence`/`RequiredLockTime` are derived
unambiguously and consistently.

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

If both CSV and CLTV are present in the same leaf, CSV determines
`RequiredSequence` (already non-final) and CLTV determines `RequiredLockTime`.

### Transaction-wide rules
Given inputs `i=0..k-1`, each with selected leaf requirements:
- `nLockTime` MUST be the maximum `RequiredLockTime` across all inputs.
- Any input that requires CLTV MUST have a non-final `nSequence`.
- Mixed CLTV types (height vs time) MUST be rejected.

## PSBT extensions (profile)

### Tap tree encoding
Implementations MAY persist taproot leaf lists in PSBT unknowns to ensure the
same leaf set can be reconstructed during finalization (OOR).

PR #58 MUST define this as a per-input PSBT unknown (not global), because the
tap tree is an input-spend concern.

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
Leaf order in the encoding MUST be the canonical leaf order defined in “Leaf
ordering”.

Compact size integers MUST follow Bitcoin varint rules; implementations SHOULD
use the existing btcd/wire varint helpers to avoid divergence.

### Condition witness encoding
For hashlock leaves that require a preimage witness element, implementations
MAY persist witness material in PSBT unknowns under a stable key (e.g.,
`ark/condition`) using standard witness serialization.

PR #58 MUST define this as a per-input PSBT unknown.

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

Internal key requirements for composed outputs:
- The internal key MUST still be unspendable (no keypath spend).
- For BTC-only policies, the default internal key SHOULD remain the existing
  NUMS/unspendable key.
- For assets-composed outputs, the API MUST allow the caller to provide an
  unspendable internal key when required to reproduce external proofs, while
  still enforcing “unspendable” as the invariant.

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

### Witness conventions (tapscript stack)
To avoid cross-implementation divergence, PR #58 MUST specify witness stack
conventions for the supported nodes. Unless explicitly noted, the “witness
stack” below refers to the elements before the final `script` and
`controlBlock` elements required by taproot script-path spends.

`Checksig(key)`:
- witness stack: `[sig]`

`Multisig([k0..kn-1], checksig)` (CHECKSIGVERIFY-chain):
- witness stack: signatures MUST be provided in reverse key order so the first
  script check consumes the signature at the top of stack:
  - `[sig(kn-1), ..., sig(k1), sig(k0)]`

`HashLock(algo, hash, inner)`:
- witness stack: `inner_witness + [preimage]`
  - signatures (and any other inner elements) come first
  - preimage MUST be last (top of stack) so `OP_HASH*` consumes it

If a leaf requires both signatures and a hash preimage, the preimage MUST be
the top-most element and signatures MUST be ordered according to the `inner`
node requirements.

### Leaf version
PR #58 MUST use the base tapleaf version (BIP-341) for all compiled leaves.
Implementations SHOULD use the btcd constant (e.g., `txscript.BaseLeafVersion`)
and MUST reject non-base versions for v1.

### Error taxonomy
PR #58 MUST provide exported, comparable error classes for:
- validation failures (expected, user-controlled): include a stable code and
  human-readable message (e.g., `ErrMissingExit`, `ErrMissingCollab`,
  `ErrBypassUngated`, `ErrExitRoleViolation`, `ErrUnsupportedOpcode`),
- internal/compiler errors (unexpected): wrap with context and treat as
  programming errors.

Callers MUST be able to distinguish “invalid policy” from “internal error”.

## PR checklist (review mapping)
This section is non-normative and exists to make review mechanical.

PR #58 should visibly include:
- `client/lib/arkscript/`: AST + validation + canonical compiler + tx-context
  derivation.
- `client/lib/scripts/vtxo.go`: wrapper delegating to `client/lib/arkscript`.
- Golden tests proving default VTXO outputs are unchanged.
- Unit/rapid tests for invariants + canonicalization + PSBT encoding helpers.

## Implementation plan (incremental)
0) **Create golden test vectors first** - Generate vectors from the current
   implementation capturing default VTXO output keys, leaf scripts, and control
   blocks. These vectors MUST be committed before any refactoring begins.
1) Implement AST nodes + canonical script encoder (verify against golden
   vectors).
2) Implement canonical leaf ordering + canonical tree builder (with control
   blocks). Verify 2-leaf case matches btcd's `AssembleTaprootScriptTree()`.
3) Implement VTXO and checkpoint validators.
4) Implement tx-context derivation + `SpendInfo` (including `RequiredSequence`
   and `RequiredLockTime` fields).
5) Implement PSBT tap tree encoding helpers (writer/reader).
6) Add assets composition hook (root sibling combination) - MAY be deferred to
   a follow-up PR if not immediately needed.
7) Final integration tests against golden vectors.

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
