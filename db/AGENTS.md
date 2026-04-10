# db

## Purpose

Database abstractions and persistent storage for all darepo-server state:
rounds, VTXOs, OOR sessions, mailbox envelopes, indexer events, the
double-entry fee ledger, and the wallet UTXO audit log. Supports PostgreSQL
and SQLite backends with SQLC-generated type-safe queries.

## Key Types

- `Store` — Main persistence layer wrapping PostgresStore or SqliteStore.
- `RoundStoreDB` — Round state persistence (create, fetch, update).
- `VTXOStoreDB` — VTXO lifecycle queries (insert, lock, update status). The
  production store enriches receive-script-backed records with
  `(owner_key, operator_key_desc, exit_delay)` from
  `receive_script_vtxo_metadata.go` at materialization time, so the
  DB-backed and in-memory `vtxo.Store` implementations round-trip the same
  record shape. Uses `InsertVTXOIfAbsent` for postgres-safe duplicate inserts.
- `VTXOLockerDB` — Global VTXO locking across rounds and OOR.
- `RecipientEventStore` — OOR recipient event log.
- `TransactionExecutor` — Batched transaction support for atomic operations.
- `MailboxEnvelopeStore` — SQL-backed `mailbox.Store` implementation that
  persists envelopes with cursor-based pull and monotonic ack watermarks.
  Supports both SQLite and Postgres via sqlc, with `UNIQUE(recipient, msg_id)`
  deduplication and per-mailbox capacity enforcement. The `Pull` path
  preserves context cancellation: if the underlying sqlc call fails after
  `ctx` was cancelled, the cancellation error is surfaced instead of being
  wrapped in a generic `pull envelopes` error.
- `ReceiveScriptVTXOMetadata` helpers (`receive_script_vtxo_metadata.go`) —
  Resolve the persisted Ark descriptor fields for a pkScript so OOR and round
  flows can materialize VTXOs without the in-memory descriptor cache.
- `OORSessionStoreDB.ApplyFinalizeAndMaterialize` — Atomic OOR finalize path
  that persists the finalized checkpoint set, marks consumed inputs spent,
  and materializes recipient outputs in a single transaction. Implements
  `oor.FinalizeAtomicStore`.
- `LedgerEntry` — Domain-level representation of a double-entry ledger
  record (debit/credit accounts, amount in sats, round id, event type,
  description, created-at). Stand-in for the future `fees.LedgerEntry`;
  the matching `fees.LedgerStore` interface and compile-time assertion are
  deferred to the `fees-03-fees-pkg` branch.
- `LedgerStoreDB` — Adapter that wraps `TransactionExecutor[*sqlc.Queries]`
  and exposes `InsertLedgerEntry(ctx, LedgerEntry)`. Each call runs the
  underlying `qtx.InsertLedgerEntry` inside `ExecTx(WriteTxOption(), ...)` so
  schema CHECK / FK violations roll back atomically and successful inserts
  commit independently of later failures.
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

- Transaction atomicity: entire checkpoint succeeds or none (no partial
  writes on crash).
- Default retry logic: 10 retries with exponential backoff (40ms initial,
  capped at 3s).
- **Never write raw SQL in Go** — add queries to `db/queries/`, regenerate
  with `make sqlc`.
- Schema changes go through `db/sqlc/migrations/`; run `make sqlc` after
  changes. Current head migration: `000009_vtxo_events_metadata` which adds
  `value_sat`, `round_id`, `batch_expiry_height`, `relative_expiry`, `origin`,
  and `commitment_txid` columns to `indexer_vtxo_events` so poll queries
  match the transient mailbox push payload.
- Receive-script metadata columns (`owner_pubkey`, `operator_pubkey`,
  `exit_delay`) on `indexer_receive_scripts` (migration 000006) round-trip
  as nil when the registration is not a standardized Ark VTXO receive script.
- VTXO status includes `Expired` for VTXOs in swept batches (set by
  `batchsweeper` after successful sweep).
- Duplicate VTXO inserts must go through `InsertVTXOIfAbsent` to stay
  postgres-safe; in-memory and DB-backed stores must agree on the
  (owner, operator, exit delay) descriptor before accepting a duplicate.
- OOR finalize must use `FinalizeAtomicStore.ApplyFinalizeAndMaterialize`
  when both a VTXO store and a session store are configured — this closes
  the crash window where inputs could be marked spent before recipient
  outputs and session state were durably written.
- Ledger entries are strictly double-entry: schema enforces `amount_sat > 0`
  and `debit_account <> credit_account`. The sum of all account balances
  across the seeded chart of accounts must always be zero. `LedgerStoreDB`
  is the only sanctioned write path so the ExecTx wrapper guarantees inserts
  are committed (or rolled back) atomically per call.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
