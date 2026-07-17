# db

## Purpose

Database abstractions and persistent storage for all wavelength state:
boarding intents, boarding sweeps, rounds, VTXOs, OOR sessions, actor
delivery checkpoints, and client-side fee accounting. Supports SQLite and
PostgreSQL backends.

## Key Types

For field-level detail, use `go doc github.com/lightninglabs/wavelength/db.<Symbol>`.

- `BatchedTx[Q]` — generic interface for atomic transactions (`ExecTx`,
  `Backend`).
- `BoardingStore` / `BoardingWalletStore` — interface + concrete
  sqlc-backed store for boarding addresses, intents, and the aggregate
  sweep lifecycle (consumed by `wallet.BoardingStore`). Sweep ops:
  `Create/MarkPublished/MarkFailed/List/ListPending/MarkInputSpent`.
- `NewBoardingSweep` / `BoardingSweepRecord` /
  `BoardingSweepInputRecord` — control-plane domain types. Sweep
  statuses: `pending`, `published`, `confirmed`, `external_resolved`,
  `failed`. Input statuses: `pending`, `published`, `spent`,
  `external_spent`, `failed`.
- `RoundStore` / `RoundPersistenceStore` — round-state interface +
  concrete `BatchedTx[RoundStore]`-backed store
  (`InsertRound`/`Get`/`GetByCommitmentTxid`/`ListActive`/`ListByStatus`/
  `UpdateStatus`/`Finalize` plus boarding-intent / VTXO-request /
  client-tree queries).
- `RoundSummary` / `VTXOSummary` — lightweight projections for
  paginated listing (avoids deserializing full trees).
- `VTXOPersistenceStore` — VTXO descriptor store
  (`InsertClientVTXO`, `FetchByOutpoint`). Persists `ChainDepth`.
- `OORArtifactStore`, `OwnedReceiveScriptStore` — OOR session state
  and locally owned receive-script metadata.
- `LedgerStoreDB` — implements `ledger.LedgerStore`. Wraps
  `sqlc.InsertClientLedgerEntry` (ON CONFLICT DO NOTHING for replay
  idempotency). Joins the outer actor transaction via
  `actor.TxFromContext`. Read API:
  `GetAccountBalance`/`GetTotalOperatorFeesPaid`/`ListLedgerEntries[…]`/
  `CountLedgerEntries`/`ListAccounts`.
- `UTXOAuditStoreDB` — implements `ledger.UTXOAuditStore` via
  `sqlc.InsertWalletUTXOLog` (ON CONFLICT DO NOTHING).
- `UnilateralExitStore` / `UnilateralExitPersistenceStore` —
  control-plane store: `Upsert` / `Get` / `ListNonTerminal` /
  `MarkTerminal`.
- `UnilateralExitJobRecord` — row: `TargetOutpoint`, `ActorID`,
  `Status`, `Trigger`, `LastError`, `SweepTxid`, `Created/UpdatedAt`.
- `UnilateralExitJobStatus` — `Pending(0)`, `Materializing(1)`,
  `CSVPending(2)`, `Sweeping(3)`, `Completed(4)`, `Failed(5)`,
  `SweepBroadcasting(6)`, `FailedRecoverable(7)`. **Append-only**: new
  values are added at the end so a row's numeric meaning never shifts.
  `FailedRecoverable` is a terminal failure that left no on-chain
  footprint, so boot-time reconciliation may roll the VTXO back to live;
  it is excluded from `ListNonTerminalUnilateralExitJobs` alongside `4`
  and `5` (wavelength#602).
- `UnilateralExitJobTrigger` — `Manual(0)`, `CriticalExpiry(1)`,
  `Restart(2)`, `FraudSpend(3)`.
- `VHTLCRecoveryStoreDB` — durable vHTLC recovery store. Persists
  armed and escalated recovery jobs with request-id idempotency,
  explicit vHTLC script parameters, fee cap, unroll target linkage,
  exact exit transaction artifacts, and terminal/cancellation state.
- `ancestryTreeCache` — process-local LRU decode cache (≤ 4096
  entries) for finalized VTXO ancestry trees (immutable once
  committed). `groupAncestryRowsWithCache` /
  `loadAncestryPathsWithCache` accept the cache to avoid
  re-deserializing the same fragment across `ListLiveVTXOs` batches.
- `isDBClosedError(err) bool` — classifies teardown-path errors for
  demotion to debug-level logging.
- `MaxTreeDeserializeDepth = 32` / `MaxTreeChildrenPerNode = 64` —
  safety bounds enforced during `DeserializeTree`.
- `resolveInputPackage` / `loadPackageBundleBySessionID` — two-stage
  OOR ancestry resolver (`oor_unroll_resolver.go`).
- `LatestMigrationVersion = 16` — current schema version.
- `PendingIntentPersistenceStore` — implements `wallet.PendingIntentStore`,
  the persistence half of the generic restart-safe intent outbox (header
  `pending_intents` + per-kind detail tables + `pending_intent_anchors`).
  Maps the sealed `wallet.PendingIntentPayload` concrete types to/from typed
  detail columns (no blob). Intents are written before the wallet publishes
  them to the round actor; `CommitState` clears anchors by outpoint
  (boarding outpoints AND forfeited VTXO outpoints) inside the
  point-of-no-return round checkpoint transaction, then sweeps orphaned
  detail rows and headers, so replay-after-adoption is structurally
  impossible. Methods: `UpsertPendingIntent` (header + detail + anchors
  atomically; anchor rebind sweeps anchor-less older intents),
  `ListPendingIntents` (per kind, with anchors), `DeletePendingIntent`,
  `ClearPendingIntentsByKind`.
- `PendingIntentStore` / `BatchedPendingIntentStore` — internal sqlc-backed
  query interfaces for the pending-intent tables.
- `SpendingReservationPersistenceStore` — Persists the durable index of VTXO
  outpoints reserved by an active spend owner (e.g. an outgoing OOR session).
  A row exists IFF the owning session was durably checkpointed, so a startup
  sweep can deterministically release orphaned Spending VTXOs with no row.
  Methods: `UpsertReservation(ctx, outpoint, ownerKind, ownerID)` (upserts a
  row), `ListReservedOutpoints(ctx)` (returns all reserved outpoints for the
  startup sweep). Implements both `oor.ReservationStore` and
  `vtxo.SpendingReservationStore`.
- `SpendingReservationStore` / `BatchedSpendingReservationStore` — Internal
  sqlc-backed query interfaces for the reservation table.

## Relationships

- **Depends on**: `baselib/actor` (DeliveryStore interface), `db/sqlc`
  (generated query layer), `db/actordelivery` (actor delivery
  persistence), `ledger` (interfaces + domain types), `wallet` (domain
  types for boarding sweeps and the pending-intent outbox), `vtxo`
  (VTXO/ancestry domain types), `round` (round-state domain types),
  `vhtlcrecovery` (recovery-job domain types).
- **Depended on by**: `round`, `vtxo`, `oor`, `wallet` (storage
  interfaces), `waved` (wires DB backends).

## Invariants

- **Never write raw SQL in Go** — add queries to `db/queries/`,
  regenerate with `make sqlc`.
- Transaction atomicity: entire checkpoint succeeds or none.
- Boarding intents persist from registration until round completion
  or failure.
- `boarding_sweeps` rows are never deleted; the daemon resumes
  spend-watch and rebroadcast on restart from
  `ListPendingBoardingSweeps`. `MarkBoardingSweepFailed` restores
  each intent's previous status atomically within the same
  transaction.
- `idx_boarding_sweep_inputs_active_outpoint` (UNIQUE on
  `(outpoint_hash, outpoint_index)` WHERE status IN
  `('pending','published')`) prevents two concurrent sweeps from
  racing on the same boarding UTXO.
- Default retry logic: 10 retries with exponential backoff (40ms →
  3s cap).
- SQLite `busy_timeout = 30 000 ms` under WAL mode tolerates
  multi-actor contention bursts.
- `ledger_entries.entry_id` and `wallet_utxo_log.entry_id` use
  `INTEGER PRIMARY KEY AUTOINCREMENT` to prevent rowid reuse after
  deletion, preserving append-only ordering.
- Per-subsystem logging via the instance logger, not the global
  package logger.
- `unilateral_exit_jobs.exit_policy_kind` and `exit_policy_ref`
  persist the durable final spend policy identity. Standard timeout
  jobs use `standard_vtxo_timeout` with an empty ref; policy-specific
  jobs store their registered kind plus the domain-owned durable ref
  needed to reconstruct the same spend policy after restart.

### Migration baseline

The migration history was squashed to a domain-grouped baseline ahead of
the public release, so each file lays down one domain of the schema
rather than replaying feature-development order. New migrations append
after the baseline; bump `LatestMigrationVersion` in `db/migrations.go`
when adding one.

- `000001_init` — `chain_info`, the normalized `internal_keys`
  registry, and macaroon root keys.
- `000002_boarding` — boarding statuses / addresses / intents
  (including the SPV `tx_proof` column) plus `boarding_sweeps` /
  `boarding_sweep_inputs`. The sweep-input FK on `previous_status`
  enforces the rollback contract without Go-side re-validation, and
  the partial unique index on active sweep inputs prevents two
  concurrent sweeps racing on the same boarding UTXO.
- `000003_rounds` — round FSM persistence: `rounds`,
  `round_boarding_intents`, `round_vtxo_requests`,
  `round_client_trees`, `client_tree_txids`.
- `000004_vtxos` — `vtxos` plus the normalized `vtxo_ancestry_paths`
  side table. Routine queries skip the ancestry join; the unroller
  loads ancestry only when resolving an exit.
- `000005_oor` — OOR artifact store (packages, checkpoints, VTXO
  bindings), receiver-side polling state, and the
  `oor_session_registry` that owns per-session durable actors.
- `000006_accounting` — chart of accounts and `ledger_entries` with
  three partial unique indexes for idempotent replay plus first-class
  `chain_txid` / `chain_vout` / `confirmation_height` columns, and the
  append-only `wallet_utxo_log` audit log.
- `000007_unilateral_exit` — `unilateral_exit_jobs` (with the durable
  `exit_policy_kind` / `exit_policy_ref` identity) and
  `vhtlc_recovery_jobs`. The vHTLC uniqueness key is
  `(swap_id, action, vtxo_txid, vtxo_vout)` so a refreshed vHTLC (new
  outpoint) arms a new recovery generation instead of colliding with
  the prior job.
- `000008_intents` — `spending_reservations` (a row exists IFF the
  owning spend session was durably checkpointed; supports the startup
  orphan sweep in `vtxo.Manager.sweepOrphanedReservations`) plus the
  pending-intent outbox supertype/subtype set: `pending_intent_kinds`,
  `pending_intents` header, `pending_board_intents` /
  `pending_send_intents` detail tables, and `pending_intent_anchors`.
- `000009_credit_operations` — client-side credit orchestration
  operations keyed by stable idempotency keys.
- `000010_activity_log` — canonical activity feed: `activity_entries`
  current-state projection plus the `activity_events` append-only
  transition log.
- `000011_pending_intent_status` — terminal-failure status for the
  pending-intent outbox.
- `000012_exit_funding_addresses` — persisted per-outpoint exit-plan
  funding addresses.
- `000013_ancestry_commitment_height` — per-fragment commitment
  confirmation height on `vtxo_ancestry_paths` (unroller watch-height
  floor).
- `000014_ancestry_multi_fragment` — drops the per-commitment UNIQUE
  constraint on `vtxo_ancestry_paths`: fragment identity is
  (commitment_txid, tree_path), so an OOR spend of inputs at different
  leaves of one commitment tree persists one row per leaf.
- `000015_ledger_round_uuid` — adds `ledger_entries.round_uuid`, the
  canonical TEXT UUID mirror of the raw 16-byte `round_id` BLOB, plus a
  partial index. The ledger and the round tables historically stored the
  same identifier in different encodings, and no BLOB↔TEXT conversion
  exists in the SQL dialect subset shared by SQLite and Postgres; the
  TEXT mirror makes ledger rows joinable against `rounds.round_id` /
  `vtxos.forfeit_round_id` (e.g. the `ListVTXOsByStatus` settlement fee
  join). New inserts stamp it via `roundUUIDText`; existing rows are
  backfilled by the version-15 Go post-migration step
  (`backfillLedgerRoundUUIDs` in `post_migration_checks.go`), wired into
  both store constructors via `makePostStepCallbacks` (its first
  production user). A crash between the post-step and the clean
  SetVersion leaves the migration dirty and the next boot fails with
  ErrDirty; forcing the version and re-running is safe because the
  backfill guards on `round_uuid IS NULL` and re-executes as a no-op.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
