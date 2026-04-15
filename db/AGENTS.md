# db

## Purpose

Database abstractions and persistent storage for all darepo-client state:
boarding intents, rounds, VTXOs, OOR sessions, actor delivery checkpoints,
and client-side fee accounting. Supports SQLite and PostgreSQL backends.

## Key Types

- `BatchedTx[Q]` — Generic interface for atomic transactions (`ExecTx`, `Backend`).
- `BoardingStore` — Interface for boarding intent persistence (CreateBoardingIntent, FetchByOutpoint, ListActive).
- `RoundStore` — Interface for round state persistence (CommitState, FetchState, ListRoundsPaginated).
- `RoundPersistenceStore` — Concrete implementation wrapping `BatchedTx[RoundStore]` with domain conversion.
- `RoundSummary` / `VTXOSummary` — Lightweight descriptors for paginated round listing (avoids deserializing full trees).
- `VTXOPersistenceStore` — Persistent store for VTXO descriptors (InsertClientVTXO, FetchByOutpoint). Persists `ChainDepth` (OOR hop count) alongside other VTXO metadata.
- `OORArtifactStore` — Interface for OOR session state persistence.
- `OwnedReceiveScriptStore` — Interface for persisting locally owned receive-script metadata (UpsertOwnedReceiveScript, LookupOwnedReceiveScript, ListOwnedReceiveScripts).
- `LedgerEntry` — Client-side double-entry ledger record (debit/credit accounts, amount, round linkage, event type).
- `LedgerStore` — Persistence interface for client-side fee ledger (InsertLedgerEntry, GetAccountBalance, GetTotalOperatorFeesPaid, ListLedgerEntries, ListLedgerEntriesByType, CountLedgerEntries, ListAccounts).
- `LedgerStoreDB` — Concrete adapter bridging `LedgerStore` to sqlc queries via `TransactionExecutor[*sqlc.Queries]`.
- `UTXOAuditEntry` — Domain-level UTXO audit log record (outpoint, amount, event, block height, classification).
- `UTXOAuditStore` — Persistence interface for wallet UTXO audit log (InsertUTXOAuditEntry, ListUTXOAuditEntries, ListUTXOAuditEntriesByBlock, ListUTXOAuditEntriesByClassification, CountUTXOAuditEntries).
- `UTXOAuditStoreDB` — Concrete adapter bridging `UTXOAuditStore` to sqlc queries.
- `VTXOPersistenceStore.ensureRoundExists` — Inserts a minimal "confirmed" round row for incoming VTXOs that reference remote rounds. Uses check-then-insert (not upsert) to avoid overwriting richer round state.

## Relationships

- **Depends on**: `baselib/actor` (DeliveryStore interface), `db/sqlc` (generated query layer), `db/actordelivery` (isolated actor delivery persistence with separate schema lifecycle).
- **Depended on by**: `round`, `vtxo`, `oor`, `wallet` (all consume storage interfaces), `ledgeractor` (consumes LedgerStore and UTXOAuditStore), `darepod` (wires DB backends).

## Invariants

- Transaction atomicity: either entire checkpoint succeeds or none (prevents partial writes on crash).
- Boarding intents persist from registration until round completion or failure.
- Round checkpoints include commitment tx, VTXO tree, client sub-trees, boarding signatures, and every intent with updated status.
- Default retry logic: 10 retries with exponential backoff (40ms initial, capped at 3s).
- **Never write raw SQL in Go** — add queries to `db/queries/`, regenerate with `make sqlc`.
- Per-subsystem logging: uses instance logger instead of global package logger.
- Latest migration: `000007_utxo_audit_log` adds an append-only UTXO audit log (`wallet_utxo_log`) with FK-constrained enum tables (`utxo_classifications`, `utxo_events`) and indexes on block_height, outpoint, and classification. Prior migration `000006_fee_accounting` adds double-entry bookkeeping tables.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
