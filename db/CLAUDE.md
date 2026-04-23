# db

## Purpose

Database abstractions and persistent storage for all darepo-client state:
boarding intents, rounds, VTXOs, OOR sessions, actor delivery checkpoints,
and client-side fee accounting. Supports SQLite and PostgreSQL backends.

## Key Types

- `BatchedTx[Q]` — Generic interface for atomic transactions (`ExecTx`, `Backend`).
- `BoardingStore` — Interface for boarding intent persistence (CreateBoardingIntent, FetchByOutpoint, ListActive).
- `RoundStore` — Interface for round state persistence. Covers the full round
  lifecycle: `InsertRound`, `GetRound`, `GetRoundByCommitmentTxid`,
  `ListActiveRounds`, `ListRoundsByStatus`, `UpdateRoundStatus`,
  `FinalizeRound`; boarding-intent queries (`InsertRoundBoardingIntent`,
  `GetRoundBoardingIntents`); VTXO-request queries (`InsertRoundVtxoRequest`,
  `GetRoundVtxoRequests`); and client-tree queries (`InsertRoundClientTree`,
  `GetRoundClientTrees`, `InsertClientTreeTxid`).
- `RoundPersistenceStore` — Concrete implementation wrapping `BatchedTx[RoundStore]` with domain conversion.
- `RoundSummary` / `VTXOSummary` — Lightweight descriptors for paginated round listing (avoids deserializing full trees).
- `VTXOPersistenceStore` — Persistent store for VTXO descriptors (InsertClientVTXO, FetchByOutpoint). Persists `ChainDepth` (OOR hop count) alongside other VTXO metadata.
- `OORArtifactStore` — Interface for OOR session state persistence.
- `OwnedReceiveScriptStore` — Interface for persisting locally owned receive-script metadata (UpsertOwnedReceiveScript, LookupOwnedReceiveScript, ListOwnedReceiveScripts).
- `LedgerStoreDB` — Concrete adapter implementing `ledger.LedgerStore`. Wraps `sqlc.InsertClientLedgerEntry` (which uses `ON CONFLICT DO NOTHING` against the three partial unique indexes on `ledger_entries` so replays silently dedupe) and propagates an optional `IdempotencyKey` on each insert. `InsertLedgerEntry` joins the outer actor transaction when one is present via `actor.TxFromContext`, so two `InsertLedgerEntry` calls from a single handler commit atomically with the mailbox ack — no batch API needed. Exposes additional query methods (GetAccountBalance, GetTotalOperatorFeesPaid, ListLedgerEntries, `ListLedgerEntriesWithFeesTotal` (returns a page and the cumulative operator-fees-paid total inside one read tx for mutual consistency), ListLedgerEntriesByType, CountLedgerEntries, ListAccounts) for the daemon RPC layer. The domain type `ledger.LedgerEntry` and interface `ledger.LedgerStore` live in the `ledger` package; `db` only provides the sqlc-backed adapter.
- `UTXOAuditStoreDB` — Concrete adapter implementing `ledger.UTXOAuditStore`. Wraps `sqlc.InsertWalletUTXOLog` (`ON CONFLICT DO NOTHING` on `(outpoint_hash, outpoint_index, event)` for crash-replay idempotency) and query methods (ListUTXOAuditEntries, ListUTXOAuditEntriesByBlock, ListUTXOAuditEntriesByClassification, CountUTXOAuditEntries). Domain types `ledger.UTXOAuditEntry` / `ledger.UTXOAuditStore` live in the `ledger` package.
- `UnilateralExitStore` — Persistence interface for the unilateral exit control plane: `UpsertUnilateralExitJob`, `GetUnilateralExitJob`, `ListNonTerminalUnilateralExitJobs`, `MarkUnilateralExitJobTerminal`.
- `UnilateralExitPersistenceStore` — Concrete sqlc-backed implementation of `UnilateralExitStore`. Created via `NewUnilateralExitStore()` on `Store`.
- `UnilateralExitJobRecord` — Control-plane row: `TargetOutpoint`, `ActorID`, `Status` (`UnilateralExitJobStatus`), `Trigger` (`UnilateralExitJobTrigger`), `LastError`, `SweepTxid`, `CreatedAt`, `UpdatedAt`.
- `UnilateralExitJobStatus` — Integer enum: `Pending(0)`, `Materializing(1)`, `CSVPending(2)`, `Sweeping(3)`, `Completed(4)`, `Failed(5)`, `SweepBroadcasting(6)`. `SweepBroadcasting` is appended last (iota 6) so existing rows at status 3 continue to decode as "sweep broadcast, awaiting conf" without shifting semantics.
- `UnilateralExitJobTrigger` — Integer enum: `Manual(0)`, `CriticalExpiry(1)`, `Restart(2)`, `FraudSpend(3)`.
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
- Latest migration: `000008_unilateral_exit_store` adds `unilateral_exit_jobs`, one row per target outpoint, holding the manager-facing control-plane view for the per-target unroll actor. `status` is an INTEGER column with documented values 0-6 (the `sweep_broadcasting` and `sweeping` values deliberately distinguish "sweep built, not yet submitted" from "sweep broadcast, awaiting confirmation"). `trigger` is 0-3 (`manual`, `critical_expiry`, `restart`, `fraud_spend`). `UnilateralExitJobStatusSweepBroadcasting` is appended at the end of the Go enum (iota value 6) so existing rows written at status=3 continue to decode as "sweep broadcast, awaiting conf" rather than silently shifting semantics; `UnilateralExitJobTriggerFraudSpend` round-trips through the Go `unroll.TriggerFraudSpend` constant rather than being silently downgraded to `TriggerManual`.
- Prior migration: `000007_utxo_audit_log` adds an append-only UTXO audit log (`wallet_utxo_log`) with FK-constrained enum tables (`utxo_classifications`, `utxo_events`), indexes on block_height, outpoint, and classification, and a `UNIQUE(outpoint_hash, outpoint_index, event)` index that makes inserts idempotent under `RestartMessage` replay.
- Migration `000006_fee_accounting` seeds the client chart of accounts — `wallet_balance`, `vtxo_balance` (assets); `fees_paid`, `onchain_fees`, `transfers_out` (expenses); `transfers_in` (revenue); `opening_balance` (equity, the source-of-funds counterparty for wallet UTXO deposits) — and `ledger_entries`. `ledger_entries` carries three optional scope columns — `round_id` (16-byte UUID), `session_id` (32-byte OOR identifier), and `idempotency_key` (outpoint-derived BLOB) — each paired with its own partial unique index (`idx_client_ledger_idempotent_round`, `_session`, `_key`) so every event class gets an at-least-once-idempotent path without colliding with the others. `InsertClientLedgerEntry` uses `ON CONFLICT DO NOTHING` so a redelivered durable-actor message resolves to a silent no-op across all three indexes. The `account_types` enum adds `equity` alongside `asset`, `liability`, `revenue`, `expense`. Ledger event types include `wallet_utxo_created` so the deposit leg written by `handleUTXOCreated` (debit `wallet_balance`, credit `opening_balance`) has a classification distinct from fee / transfer events.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
