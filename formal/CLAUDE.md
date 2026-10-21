# Formal Models

## Purpose

Machine-checked protocol and implementation invariants that complement the
state-space exploration under `p-models/`.

## Key Areas

- `lean/forfeitsigning/` proves that the receive-side forfeit signing gate can
  admit a request only after authority is published and every funded identity
  field matches.

## Invariants

- A proof artifact must identify the production boundary it constrains.
- Executable bridge vectors must be checked against production code so an
  abstract proof cannot silently drift from its implementation.
- Counterexamples for the unsafe rule belong beside the positive proof.

## Deep Docs

- [`lean/forfeitsigning/README.md`](lean/forfeitsigning/README.md) — proof
  statement, abstraction boundary, and verification commands.
