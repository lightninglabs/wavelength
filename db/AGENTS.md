# db

## Purpose

Database abstractions and persistent storage for all darepo-client state:
boarding intents, boarding sweeps, rounds, VTXOs, OOR sessions, actor
delivery checkpoints, and client-side fee accounting. Supports SQLite and
PostgreSQL backends.

## Key Types

For field-level detail, use `go doc github.com/lightninglabs/darepo-client/db.<Symbol>`.

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
  and `5` (darepo-client#602).
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
- `LatestMigrationVersion = 21` — current schema version.
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
  persistence), `ledger` (interfaces + domain types), `wallet`
  (`BoardingStore` interface + domain types).
- **Depended on by**: `round`, `vtxo`, `oor`, `wallet` (storage
  interfaces), `darepod` (wires DB backends).

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

### Migration notes

- `000020_accounting_wallet_sweeps` — adds `wallet_clearing`,
  `wallet_utxo_spent`, and `wallet_sweep_transfer` for sweep
  accounting. Rebuilds the round idempotency index so keyed
  round events use `idx_client_ledger_idempotent_key` instead of
  collapsing on `(round_id, event_type, debit_account, credit_account)`.
  Renumbered from 000019 to land after `000019_oor_session_registry`,
  which merged to main while this work was in review.
- `000018_pending_intents` — generalizes the Board-only
  `pending_board_requests` outbox into a supertype/subtype set:
  `pending_intent_kinds` (enum table), `pending_intents` (header: 32-byte
  hash-derived intent id + kind FK + requested_at, no payload blob),
  per-kind detail tables `pending_board_intents` / `pending_send_intents`
  with first-class typed columns, and `pending_intent_anchors` (one row per
  anchored outpoint, PK on the outpoint so a newer intent rebinds, FK to the
  header). Drops `pending_board_requests` outright (alpha; rows only exist
  in the narrow crash window between admission and round seal).
- `000021_vhtlc_recovery_job_generations` — rebuilds `vhtlc_recovery_jobs`
  to widen the uniqueness key from `(swap_id, action)` to
  `(swap_id, action, vtxo_txid, vtxo_vout)`, so a refreshed vHTLC (new
  outpoint) arms a new recovery "generation" instead of colliding with the
  prior job. SQLite cannot widen a UNIQUE constraint in place, so the table
  is recreated, rows are copied, and the state / swap-action / unroll-target
  indexes are rebuilt. The down migration collapses each `(swap_id, action)`
  to its newest row before restoring the narrower constraint.
- `000017_spending_reservations` — adds `spending_reservations` table with
  `(outpoint_hash, outpoint_index)` PK, `owner_kind`, `owner_id`, and
  `created_at`. A row exists IFF the owning spend session was durably
  checkpointed. The table supports the startup orphan sweep in the VTXO
  manager (`vtxo.Manager.sweepOrphanedReservations`).
- `000016_unilateral_exit_policy` — adds `exit_policy_kind`
  (NOT NULL, default `'standard_vtxo_timeout'`) and nullable
  `exit_policy_ref` to `unilateral_exit_jobs` via ALTER TABLE so
  policy-specific unroll jobs restart with the same final spend policy.
  Backfills legacy rows via the column default.
- `000015_vhtlc_recovery_jobs` — vHTLC recovery control-plane rows
  with named script parameters, request-id idempotency, SQL-visible
  timestamps, and an optional `claim_preimage` BLOB reserved for
  cross-process claim recovery. Initial arm never stores a raw preimage;
  only the execution-layer escalation path may populate it, and it must
  never be logged.
- `000014_ledger_chain_fields` — first-class `chain_txid` / `chain_vout`
  / `confirmation_height` columns on `ledger_entries` so history reads
  don't decode wallet UTXO idempotency keys per query.
- `000013_pending_board_request` — records the user's explicit `Board`
  RPC intent so a daemon restart between Board admission and round
  seal does not silently drop the request. Keyed by the confirmed
  boarding outpoint. Superseded by `000018_pending_intents`.
- `000012_boarding_sweep_ledger_events` — registers the
  `boarding_sweep_fee_paid` ledger event type so `FeePaidMsg` with
  `FeeType=FeeTypeOnchainSweep` satisfies the `ledger_entries.event_type`
  FK.
- `000011_boarding_sweeps` — `boarding_sweeps` /
  `boarding_sweep_inputs` tables + new `sweep_pending` boarding
  status. Input FK on `previous_status` enforces the rollback
  contract without Go-side re-validation.
- `000010_boarding_tx_proof` — nullable `tx_proof BLOB` on
  `boarding_intents`. TLV via `lib/types.SerializeTxProof`; NULL on
  legacy rows decodes to `fn.None` (wallet rebuilds from
  `conf_tx`/`conf_hash` on next read). Upsert uses
  `COALESCE(excluded.tx_proof, …)` so a NULL re-insert never
  clobbers a good proof. Zero-length slices normalize to nil in Go
  to avoid the `x''` BYTEA pitfall on Postgres.
- `000009_vtxo_ancestry_paths` — replaces scalar `tree_path` /
  `tree_depth` with a normalized `vtxo_ancestry_paths` side table;
  routine queries skip the join, the unroller loads ancestry only
  when resolving an exit.
- `000008_unilateral_exit_store` — adds `unilateral_exit_jobs`.
- `000007_utxo_audit_log` — append-only UTXO audit log.
- `000006_fee_accounting` — seeds the client chart of accounts and
  `ledger_entries` with three partial unique indexes for idempotent
  replay.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
</content>
</invoke>
