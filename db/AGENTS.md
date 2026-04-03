# db

## Purpose

Database abstractions and persistent storage for all darepo-server state:
rounds, VTXOs, OOR sessions, mailbox envelopes, and indexer events. Supports
PostgreSQL and SQLite backends with SQLC-generated type-safe queries.

## Key Types

- `Store` — Main persistence layer wrapping PostgresStore or SqliteStore.
- `RoundStoreDB` — Round state persistence (create, fetch, update).
- `VTXOStoreDB` — VTXO lifecycle queries (insert, lock, update status).
- `VTXOLockerDB` — Global VTXO locking across rounds and OOR.
- `RecipientEventStore` — OOR recipient event log.
- `TransactionExecutor` — Batched transaction support for atomic operations.
- `MailboxEnvelopeStore` — SQL-backed `mailbox.Store` implementation that
  persists envelopes with cursor-based pull and monotonic ack watermarks.
  Supports both SQLite and Postgres via sqlc, with `UNIQUE(recipient, msg_id)`
  deduplication and per-mailbox capacity enforcement.
- `GetVTXOStatsByStatus` / `GetRoundStatsByStatus` / `GetOORSessionStatsByState`
  — Aggregate queries used by the metrics `SystemCollector` at scrape time.

## Relationships

- **Depends on**: `clientconn` (client identity types), `rounds` (round state
  types), `vtxo` (VTXO record types), `db/sqlc` (generated query layer).
- **Depended on by**: `rounds`, `oor`, `indexer`, `metrics` (scrape-time
  aggregate queries), root `darepo` (all consume storage interfaces).
  `batchsweeper` reaches the DB indirectly via an `OnBatchSwept` callback
  wired in the root package.

## Invariants

- Transaction atomicity: entire checkpoint succeeds or none (no partial writes on crash).
- Default retry logic: 10 retries with exponential backoff (40ms initial, capped at 3s).
- **Never write raw SQL in Go** — add queries to `db/queries/`, regenerate with `make sqlc`.
- Schema changes go through `db/sqlc/migrations/`; run `make sqlc` after changes.
- VTXO status includes `Expired` for VTXOs in swept batches (set by
  batchsweeper after successful sweep).

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
