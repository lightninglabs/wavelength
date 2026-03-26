# db

## Purpose

Database abstractions and persistent storage for all darepo-client state:
boarding intents, rounds, VTXOs, OOR sessions, and actor delivery checkpoints.
Supports SQLite and PostgreSQL backends.

## Key Types

- `BatchedTx[Q]` — Generic interface for atomic transactions (`ExecTx`, `Backend`).
- `BoardingStore` — Interface for boarding intent persistence (CreateBoardingIntent, FetchByOutpoint, ListActive).
- `RoundStore` — Interface for round state persistence (CommitState, FetchState, ListRoundsPaginated).
- `RoundPersistenceStore` — Concrete implementation wrapping `BatchedTx[RoundStore]` with domain conversion.
- `RoundSummary` / `VTXOSummary` — Lightweight descriptors for paginated round listing (avoids deserializing full trees).
- `VTXOPersistenceStore` — Persistent store for VTXO descriptors (InsertClientVTXO, FetchByOutpoint). Persists `ChainDepth` (OOR hop count) alongside other VTXO metadata.
- `OORArtifactStore` — Interface for OOR session state persistence.
- `OwnedReceiveScriptStore` — Interface for persisting locally owned receive-script metadata (UpsertOwnedReceiveScript, LookupOwnedReceiveScript, ListOwnedReceiveScripts).
- `VTXOPersistenceStore.ensureRoundExists` — Inserts a minimal "confirmed" round row for incoming VTXOs that reference remote rounds. Uses check-then-insert (not upsert) to avoid overwriting richer round state.

## Relationships

- **Depends on**: `baselib/actor` (DeliveryStore interface), `db/sqlc` (generated query layer), `db/actordelivery` (isolated actor delivery persistence with separate schema lifecycle).
- **Depended on by**: `round`, `vtxo`, `oor`, `wallet` (all consume storage interfaces), `darepod` (wires DB backends).

## Invariants

- Transaction atomicity: either entire checkpoint succeeds or none (prevents partial writes on crash).
- Boarding intents persist from registration until round completion or failure.
- Round checkpoints include commitment tx, VTXO tree, client sub-trees, boarding signatures, and every intent with updated status.
- Default retry logic: 10 retries with exponential backoff (40ms initial, capped at 3s).
- **Never write raw SQL in Go** — add queries to `db/queries/`, regenerate with `make sqlc`.
- Per-subsystem logging: uses instance logger instead of global package logger.
- Latest migration: `000005_vtxo_chain_depth` adds `chain_depth INTEGER NOT NULL DEFAULT 0` to `vtxos` table. UPSERT uses zero-value sentinel pattern (same as `tree_depth`, `batch_expiry`): zero means "not yet populated", non-zero overwrites.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
