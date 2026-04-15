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
- `LedgerStoreDB` — Concrete adapter implementing `ledger.LedgerStore`. Wraps `sqlc.InsertClientLedgerEntry` and exposes additional query methods (GetAccountBalance, GetTotalOperatorFeesPaid, ListLedgerEntries, ListLedgerEntriesByType, CountLedgerEntries, ListAccounts) for the daemon RPC layer. The domain type `ledger.LedgerEntry` and interface `ledger.LedgerStore` live in the `ledger` package; `db` only provides the sqlc-backed adapter.
- `UTXOAuditStoreDB` — Concrete adapter implementing `ledger.UTXOAuditStore`. Wraps `sqlc.InsertWalletUTXOLog` (`ON CONFLICT DO NOTHING` on `(outpoint_hash, outpoint_index, event)` for crash-replay idempotency) and query methods (ListUTXOAuditEntries, ListUTXOAuditEntriesByBlock, ListUTXOAuditEntriesByClassification, CountUTXOAuditEntries). Domain types `ledger.UTXOAuditEntry` / `ledger.UTXOAuditStore` live in the `ledger` package.
- `VTXOPersistenceStore.ensureRoundExists` — Inserts a minimal "confirmed" round row for incoming VTXOs that reference remote rounds. Uses check-then-insert (not upsert) to avoid overwriting richer round state.

## Relationships

- **Depends on**: `baselib/actor` (DeliveryStore interface), `db/sqlc` (generated query layer), `db/actordelivery` (isolated actor delivery persistence with separate schema lifecycle), `ledger` (for the `LedgerStore`, `UTXOAuditStore`, `LedgerEntry`, and `UTXOAuditEntry` interface/domain types that this package adapts).
- **Depended on by**: `round`, `vtxo`, `oor`, `wallet` (all consume storage interfaces), `darepod` (wires DB backends and passes `LedgerStoreDB` / `UTXOAuditStoreDB` into the ledger actor).

## Invariants

- Transaction atomicity: either entire checkpoint succeeds or none (prevents partial writes on crash).
- Boarding intents persist from registration until round completion or failure.
- Round checkpoints include commitment tx, VTXO tree, client sub-trees, boarding signatures, and every intent with updated status.
- Default retry logic: 10 retries with exponential backoff (40ms initial, capped at 3s).
- **Never write raw SQL in Go** — add queries to `db/queries/`, regenerate with `make sqlc`.
- Per-subsystem logging: uses instance logger instead of global package logger.
- Latest migration: `000007_utxo_audit_log` adds an append-only UTXO audit log (`wallet_utxo_log`) with FK-constrained enum tables (`utxo_classifications`, `utxo_events`), indexes on block_height, outpoint, and classification, and a `UNIQUE(outpoint_hash, outpoint_index, event)` index that makes inserts idempotent under `RestartMessage` replay.
- Migration `000006_fee_accounting` seeds the client chart of accounts (`wallet_balance`, `vtxo_balance`, `fees_paid`, `onchain_fees`, `transfers_in`, `transfers_out`) and `ledger_entries`. `ledger_entries` carries both `round_id` and `session_id` columns with two partial unique indexes (`idx_client_ledger_idempotent_round` and `idx_client_ledger_idempotent_session`) so in-round and OOR events are deduped without type overloading.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
