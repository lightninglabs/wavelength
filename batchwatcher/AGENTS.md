# batchwatcher

## Purpose

On-chain batch transaction monitoring and VTXO spend detection. Watches
confirmed batch transactions for sweeps, spends, and expiry, reporting state
changes to the round and sweep subsystems.

## Key Types

- `Actor` — Durable actor monitoring batch transactions on-chain.
- `BatchID` — Identifier for a confirmed batch.
- `Output` — Tracked output within a batch tree.
- `BatchTreeState` — Aggregate state of a batch's VTXO tree on-chain.

## Relationships

- **Depends on**: no internal dependencies (receives chain events from external sources).
- **Depended on by**: `rounds` (confirmation monitoring), `batchsweeper` (sweep eligibility).
- **Messages to/from**:
  - Sends batch confirmation/spend events -> `rounds`.
  - Sends sweep-eligible batch notifications -> `batchsweeper`.

## Invariants

- Must detect all spends of tracked outputs; missed spends can cause incorrect VTXO state.
- Batch state must be persisted before notifications are sent.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
