# db

## Purpose

Database abstractions and persistent storage for all darepo-client state:
boarding intents, boarding sweeps, rounds, VTXOs, OOR sessions, actor delivery
checkpoints, and client-side fee accounting. Supports SQLite and PostgreSQL
backends.

## Key Types

- `BatchedTx[Q]` — Generic interface for atomic transactions (`ExecTx`,
  `Backend`).
- `BoardingStore` — Interface for all boarding-related database queries:
  addresses, intents, and aggregate sweep lifecycle. Consumed by
  `BoardingWalletStore`.
- `BoardingWalletStore` — Concrete sqlc-backed implementation of
  `wallet.BoardingStore`. Created via `NewBoardingWalletStore(db,
  chainParams, clock)`. Persists boarding addresses and intents with their
  full lifecycle (confirmed → adopted → swept / expired / failed /
  sweep_pending). Includes sweep operations: `CreatePendingBoardingSweep`,
  `MarkBoardingSweepPublished`, `MarkBoardingSweepFailed`,
  `ListBoardingSweeps`, `ListPendingBoardingSweeps`,
  `MarkBoardingSweepInputSpent`.
- `NewBoardingSweep` / `BoardingSweepRecord` / `BoardingSweepInputRecord` —
  Domain types for the boarding sweep control plane. A sweep is an aggregate
  transaction spending one or more boarding outpoints; each outpoint has its
  own per-input status. Sweep statuses: `pending`, `published`, `confirmed`,
  `external_resolved`, `failed`. Input statuses: `pending`, `published`,
  `spent`, `external_spent`, `failed`.
- `RoundStore` — Interface for round state persistence (full lifecycle:
  `InsertRound`, `GetRound`, `GetRoundByCommitmentTxid`, `ListActiveRounds`,
  `ListRoundsByStatus`, `UpdateRoundStatus`, `FinalizeRound`; boarding-intent,
  VTXO-request, client-tree queries).
- `RoundPersistenceStore` — Concrete implementation wrapping
  `BatchedTx[RoundStore]` with domain conversion.
- `RoundSummary` / `VTXOSummary` — Lightweight descriptors for paginated round
  listing (avoids deserializing full trees).
- `VTXOPersistenceStore` — Persistent store for VTXO descriptors
  (`InsertClientVTXO`, `FetchByOutpoint`). Persists `ChainDepth` (OOR hop
  count) alongside other VTXO metadata.
- `OORArtifactStore` — Interface for OOR session state persistence.
- `OwnedReceiveScriptStore` — Interface for persisting locally owned
  receive-script metadata.
- `LedgerStoreDB` — Concrete adapter implementing `ledger.LedgerStore`. Wraps
  `sqlc.InsertClientLedgerEntry` (ON CONFLICT DO NOTHING for replay
  idempotency). Joins the outer actor transaction via `actor.TxFromContext`.
  Exposes `GetAccountBalance`, `GetTotalOperatorFeesPaid`,
  `ListLedgerEntries`, `ListLedgerEntriesWithFeesTotal`,
  `ListLedgerEntriesByType`, `CountLedgerEntries`, `ListAccounts`.
- `UTXOAuditStoreDB` — Concrete adapter implementing `ledger.UTXOAuditStore`.
  Wraps `sqlc.InsertWalletUTXOLog` (ON CONFLICT DO NOTHING for idempotency)
  and query methods.
- `UnilateralExitStore` — Persistence interface for the unilateral exit control
  plane: `UpsertUnilateralExitJob`, `GetUnilateralExitJob`,
  `ListNonTerminalUnilateralExitJobs`, `MarkUnilateralExitJobTerminal`.
- `UnilateralExitPersistenceStore` — Concrete sqlc-backed implementation of
  `UnilateralExitStore`.
- `UnilateralExitJobRecord` — Control-plane row: `TargetOutpoint`, `ActorID`,
  `Status` (`UnilateralExitJobStatus`), `Trigger`
  (`UnilateralExitJobTrigger`), `LastError`, `SweepTxid`, `CreatedAt`,
  `UpdatedAt`.
- `UnilateralExitJobStatus` — Integer enum: `Pending(0)`, `Materializing(1)`,
  `CSVPending(2)`, `Sweeping(3)`, `Completed(4)`, `Failed(5)`,
  `SweepBroadcasting(6)`. `SweepBroadcasting` appended last so existing rows
  at status 3 continue to decode correctly.
- `UnilateralExitJobTrigger` — Integer enum: `Manual(0)`,
  `CriticalExpiry(1)`, `Restart(2)`, `FraudSpend(3)`.
- `ancestryTreeCache` — Process-local LRU decode cache (up to 4096 entries)
  for finalized VTXO ancestry trees. Trees are immutable once committed;
  `groupAncestryRowsWithCache` / `loadAncestryPathsWithCache` accept an
  optional cache to avoid re-deserializing the same tree fragment across
  `ListLiveVTXOs` batch reads.
- `isDBClosedError(err) bool` — Classifies teardown-path errors for demotion
  to debug-level logging.
- `MaxTreeDeserializeDepth = 32` / `MaxTreeChildrenPerNode = 64` — Safety
  bounds enforced during `DeserializeTree` to prevent stack overflow or OOM.
- `resolveInputPackage` / `loadPackageBundleBySessionID` — Two-stage OOR
  ancestry resolver in `oor_unroll_resolver.go`.
- `LatestMigrationVersion = 11` — Current schema version.

## Relationships

- **Depends on**: `baselib/actor` (DeliveryStore interface), `db/sqlc`
  (generated query layer), `db/actordelivery` (actor delivery persistence),
  `ledger` (LedgerStore / UTXOAuditStore interface and domain types), `wallet`
  (BoardingStore interface and domain types).
- **Depended on by**: `round`, `vtxo`, `oor`, `wallet` (storage interfaces),
  `darepod` (wires DB backends).

## Invariants

- Transaction atomicity: entire checkpoint succeeds or none (no partial writes).
- Boarding intents persist from registration until round completion or failure.
- `boarding_sweeps` rows persist the signed sweep tx and are never deleted; the
  daemon resumes spend-watch and rebroadcast on restart from
  `ListPendingBoardingSweeps`. `MarkBoardingSweepFailed` restores each
  intent's previous status atomically within the same write transaction.
- `idx_boarding_sweep_inputs_active_outpoint` (UNIQUE on `(outpoint_hash,
  outpoint_index)` WHERE status IN ('pending', 'published')) prevents two
  concurrent sweeps from racing on the same boarding UTXO.
- Default retry logic: 10 retries with exponential backoff (40ms initial,
  capped at 3s).
- SQLite `busy_timeout` is 30 000 ms under WAL mode to tolerate multi-actor
  contention bursts without surfacing as upstream errors.
- `ledger_entries.entry_id` and `wallet_utxo_log.entry_id` use
  `INTEGER PRIMARY KEY AUTOINCREMENT` to prevent SQLite rowid reuse after
  deletion, preserving append-only ledger ordering.
- **Never write raw SQL in Go** — add queries to `db/queries/`, regenerate
  with `make sqlc`.
- Per-subsystem logging: uses instance logger instead of global package logger.
- Migration `000011_boarding_sweeps` adds `boarding_sweeps` and
  `boarding_sweep_inputs` tables plus a new `sweep_pending` boarding status.
  Input FK `FOREIGN KEY (previous_status) REFERENCES boarding_statuses` ensures
  only valid previous statuses are stored; ON CONFLICT the FK enforces the
  rollback contract without Go-side re-validation.
- Migration `000010_boarding_tx_proof` adds a nullable `tx_proof BLOB` to
  `boarding_intents`. TLV-encoded via `lib/types.SerializeTxProof`; NULL on
  legacy rows decodes to `fn.None`; the wallet rebuilds from `conf_tx` /
  `conf_hash` on next read. The upsert uses `COALESCE(excluded.tx_proof,
  boarding_intents.tx_proof)` so a NULL re-insert never clobbers a good proof;
  zero-length slices are normalised to nil in Go to avoid the dialect pitfall
  with `x''` in Postgres BYTEA.
- Migration `000009_vtxo_ancestry_paths` replaces scalar `tree_path` /
  `tree_depth` columns with a normalised `vtxo_ancestry_paths` side table;
  routine queries skip the join while the unroller loads ancestry only when
  resolving an exit.
- Migration `000008_unilateral_exit_store` adds `unilateral_exit_jobs`.
- Migration `000007_utxo_audit_log` adds an append-only UTXO audit log.
- Migration `000006_fee_accounting` seeds the client chart of accounts and
  `ledger_entries` with three partial unique indexes for idempotent replay.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
