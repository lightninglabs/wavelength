# lib/arkscript

## Purpose

Bitcoin script compiler and policy system for constructing Ark protocol taproot
outputs. Provides an AST of semantic nodes that compile to tapscript, plus
higher-level policy templates for VTXO, vHTLC, and checkpoint outputs with
validated invariants.

## Key Types

- `Node` — Sealed interface for all AST nodes representing spending conditions.
  Implementations: `Multisig`, `CSV`, `Condition`, `Preimage`, `CLTV`.
- `PolicyTemplate` — Semantic representation of a tapscript policy with named
  leaves. Supports encode/decode for persistence.
- `CompiledPolicy` — Fully compiled policy with canonical leaf ordering, merkle
  tree, and control block derivation.
- `VTXOPolicy` — Compiled VTXO taproot policy with collab and exit spend paths.
  Provides `CollabSpendInfo()` and `ExitSpendInfo()`.
- `VHTLCPolicy` — 6-leaf vHTLC policy with claim/refund/unilateral paths for
  hash-time-locked conditional transfers.
- `CheckpointPolicy` — Parameters for OOR checkpoint taproot tree construction.
- `SpendInfo` — Witness script + control block needed to spend a specific leaf.

## Relationships

- **Depends on**: (no internal repo imports; pure cryptographic library).
- **Depended on by**: `darepod`, `db`, `lib/tree`, `lib/tx/arktx`,
  `lib/tx/checkpoint`, `lib/tx/oor`, `lib/tx/psbtutil`, `oor`, `round`,
  `vtxo`, `wallet`.

## Invariants

- Node interface is sealed: only types defined in this package can implement it.
- Every collab leaf must contain the operator key for safe cosigning.
- Every exit leaf must be CSV-gated for unilateral recovery.
- Canonical leaf ordering: sorted by version then lexicographic script bytes.
- All taproot outputs use the unspendable ARK NUMS key for key path (no
  key-path spend possible).
- Policy validation enforces at least one operator-containing leaf
  (collaborative) and at least one non-operator leaf (exit/unilateral).
- Exit delay must be >= `MinExitDelay`.

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
