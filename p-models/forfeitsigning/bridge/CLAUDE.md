# p-models/forfeitsigning/bridge

## Purpose

Shared JSON trace schema for replaying the P forfeit-signing scenarios against
the receive-session responder and daemon signature broker.

## Key Types

- `Trace` — one named authority or replay scenario.
- `Step` — one publication, delivery, restart, or submission operation.
- `ParseTrace` — validates and decodes a checked-in trace.

## Relationships

- **Depends on:** checked-in JSON files under `../traces`.
- **Depended on by:** bridge tests in `sdk/swaps` and `waved`.

## Invariants

- A trace must have a stable id and at least one step.
- Production tests reject unknown operations, identities, signatures, and
  expectations rather than silently skipping them.

## Deep Docs

- [README.md](../README.md) — Model requirements and conformance matrix.
