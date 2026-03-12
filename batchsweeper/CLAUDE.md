# batchsweeper

## Purpose

Expired batch recovery via sweep transactions. When batch tree outputs expire
past their CSV timelock, this actor builds and broadcasts sweep transactions to
reclaim funds.

## Key Types

- `Actor` — Durable actor that monitors for sweep-eligible batches and executes sweeps.
- `SweepTxBuilder` — Constructs sweep transactions for expired tree outputs.

## Relationships

- **Depends on**: `batchwatcher` (sweep-eligible batch notifications).
- **Depended on by**: root `darepo` (wiring).
- **Messages to/from**:
  - Receives sweep-eligible notifications <- `batchwatcher`.

## Invariants

- Sweep transactions must only be broadcast after CSV timelock expiry.
- Sweep must be idempotent; re-sweeping an already-swept output is a no-op.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
