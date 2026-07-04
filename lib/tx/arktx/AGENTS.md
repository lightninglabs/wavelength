# lib/tx/arktx

## Purpose

Helpers for constructing and validating Ark transactions representing the
virtual-chain step following checkpoints. Enforces canonical output ordering
(critical because multiple subsystems rely on byte-identical transaction
construction).

## Key Types

- `TxVersion` — Canonical transaction version (v3) for Ark transfers to support
  package relay.

## Key Functions

- `ValidateCanonicalTx` / `ValidateCanonicalPSBT` — Validate canonical ordering
  rules including exactly one anchor output placed last.
- `CanonicalizeOrdering` — Sorts transaction inputs/outputs in-place according
  to v0 canonical rules (BIP69 ordering).
- `IsAnchorOutput` — Identifies v0 Ark anchor outputs (P2A, value 0).
- `IsP2AAnchorScript` — Matches the keyless P2A anchor pkScript regardless of
  the output's value, i.e. it also matches a "funded" anchor whose value was
  lifted above the P2A dust threshold. Use when locating a CPFP anchor to
  spend on a parent that is already independently valid.
- `IsFundedAnchorOutput` — Identifies the "funded" (non-zero value) anchor
  form: a P2A output whose parent pays its own fee and confirms standalone,
  with the anchor reserved as an optional CPFP fee-bump handle. Complement of
  `IsAnchorOutput`, which matches only the zero-value ephemeral form.

## Relationships

- **Depends on**: `lib/arkscript` (for `AnchorPkScript`).
- **Depended on by**: `lib/tx/checkpoint`, `lib/tx/oor`, `oor`.

## Invariants

- Anchor output must be exactly one and must be the last output.
- Input ordering follows BIP69: sorted by outpoint hash then index.
- Recipient output ordering follows BIP69: sorted by amount then pkScript.
- Validation is deterministic and byte-identical across client and server.

## Deep Docs

- [ARCHITECTURE.md](../../../ARCHITECTURE.md) — System-wide package map.
