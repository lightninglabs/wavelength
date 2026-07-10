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
- `IsAnchorOutput` — Identifies the zero-value ephemeral P2A anchor output.
- `IsFundedAnchorOutput` / `IsP2AAnchorScript` — Identify a P2A anchor carrying
  a non-zero "funded" value (spare CPFP handle on an otherwise fee-paying
  parent), as distinct from the zero-value ephemeral form.

## Relationships

- **Depends on**: `lib/arkscript` (for `AnchorPkScript`).
- **Depended on by**: `oor`, `lib/tx/oor`, `lib/tx/checkpoint` (canonical
  construction/validation); `unroll`, `wallet`, `vhtlcrecovery/unrollpolicy`,
  `db` (via `TxVersion`); `txconfirm` (funded-anchor detection for CPFP).

## Invariants

- Anchor output must be exactly one and must be the last output.
- Input ordering follows BIP69: sorted by outpoint hash then index.
- Recipient output ordering follows BIP69: sorted by amount then pkScript.
- Validation is deterministic and byte-identical across client and server.
- `IsAnchorOutput` matches only the zero-value ephemeral anchor; use
  `IsFundedAnchorOutput`/`IsP2AAnchorScript` when the anchor may carry a
  non-zero fee-bump value. Conflating the two misclassifies a funded anchor
  as absent (or vice versa).

## Deep Docs

- [ARCHITECTURE.md](../../../ARCHITECTURE.md) — System-wide package map.
