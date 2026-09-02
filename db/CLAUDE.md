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
  `UpdateStatus`/`Finalize`/`FailRound` plus boarding-intent /
  VTXO-request / client-tree queries).
- `RoundPersistenceStore.FailRound(ctx, roundID)` — terminal-failure
  counterpart to `FinalizeRound`. In one transaction it moves the round
  row to `failed` and returns every boarding intent that round adopted
  to `confirmed`. It is the exact inverse of the `CommitState`
  checkpoint write, and shares that write's transaction for the same
  reason: the round row and the intent statuses are a single fact about
  where the deposit lives.
- `RoundSummary` / `VTXOSummary` — lightweight projections for
  paginated listing (avoids deserializing full trees).
- `VTXOPersistenceStore` — VTXO descriptor store
  (`InsertClientVTXO`, `FetchByOutpoint`). Persists `ChainDepth`.
- `OORArtifactStore`, `OwnedReceiveScriptStore` — OOR session state
  and locally owned receive-script metadata.
- `OwnedReceiveScriptRecord` — one owned receive-script row. Its
  `IdempotencyKey`, `RegistrationLabel`, `RegistrationExpiresAt`, and
  `RegistrationRPCKey` fields define a retry-safe allocation;
  `RegistrationCompletedAt` is set only after the indexer acknowledges that
  remote request. Legacy rows and internal registrations leave the replay
  identity empty, which keeps fresh allocation as the default.
- Idempotent receive-script admission on `OORArtifactPersistenceStore`:
  `AdmitIdempotentOwnedReceiveScript` selects the durable winner,
  `LookupOwnedReceiveScriptByIdempotencyKey` loads it,
  `RenewOwnedReceiveScriptRegistration` replaces an expired remote window by
  compare-and-swap, and `MarkOwnedReceiveScriptRegistered` records remote
  acknowledgement for the current request key. Bounds are
  `MaxOwnedReceiveScriptIdempotencyKeyBytes = 128` and
  `MaxOwnedReceiveScriptRegistrationTTL = 30 * 24h`. Reusing a key with a
  different label fails with `ErrOwnedReceiveScriptReplayMismatch`.
- `OORSessionRegistryStoreDB` / `OORSessionRegistryRecord` — control-plane
  registry of per-session durable OOR actors
  (`UpsertSession`/`GetSession`/`ListSessions`/`ListNonTerminal`). One mutable
  row per session id is shared by both lifecycle directions.
- `OORDispatchAttemptRecord` — immutable keyed outgoing identity, loaded by
  idempotency key or session id. It stores the canonical recipient record
  before the first transport enqueue and remains authoritative after the
  mutable session row becomes terminal or changes direction.
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
- `LatestMigrationVersion = 18` — current schema version.
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
- `ActivityPersistenceStore` / `ActivityStore` — canonical activity-log store
  + its sqlc-backed query interface. `ProjectEntry` is the monotonic
  projector (it suppresses no-op re-projections and refuses
  terminal-to-pending regressions); `GetEntry` reads one canonical row;
  `AppendActivityEvent` extends the immutable event log.
- `ActivityPersistenceStore.RepairCreditReceivePollCap(ctx, p, failedStatus,
  legacyFailureReason)` — the single sanctioned exception to that monotonic
  rule, for the false receive failure older clients wrote under the retired
  credit poll cap. The guarded current-state update and the corrective
  pending event commit in one transaction, so the reopen is *appended* to
  history rather than erasing the failure. The SQL guard is what keeps the
  exception narrow: the update matches only an inbound receive row whose
  status and failure reason equal the caller-supplied legacy pair, so
  `ProjectEntry`'s terminal-to-pending rule is never relaxed for any other
  row. A zero row count is not an error — it means the row was already
  repaired or never legacy — and the caller sees `eventSeq == 0`.

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
- A dead round returns its adopted boarding intents to `confirmed`, not
  `failed` (wavelength#1051). A round that never broadcast its
  commitment leaves the boarding UTXO exactly as it was, so `confirmed`
  is the truthful status: it returns the deposit to the boardable pool
  and makes it sweepable again, since `boardingIntentSweepable` excludes
  `adopted` and would otherwise never sweep the intent, CSV expiry or
  not.
- `FailRound` is safe to call on any round, because two guards decide
  whether anything moves. The round update matches only a row still at
  `input_sig_sent`, and the deposits are released only if it did, so a
  round whose commitment confirmed keeps its intents (they remain
  literally `adopted`; the listing queries hide them by joining on round
  status rather than by rewriting them). The intent update is scoped to
  that round's own adopted intents, so it can neither release a deposit
  another round has since taken nor clobber a sweep already in flight.
- `boarding_sweeps` rows are never deleted; the daemon resumes
  spend-watch and rebroadcast on restart from
  `ListPendingBoardingSweeps`. `MarkBoardingSweepFailed` restores
  each intent's previous status atomically within the same
  transaction.
- `idx_boarding_sweep_inputs_active_outpoint` (UNIQUE on
  `(outpoint_hash, outpoint_index)` WHERE status IN
  `('pending','published')`) prevents two concurrent sweeps from
  racing on the same boarding UTXO.
- `idx_owned_receive_scripts_idempotency_key` (UNIQUE on `idempotency_key`
  WHERE `idempotency_key IS NOT NULL`) admits exactly one durable receive-script
  allocation per RPC retry key while leaving legacy and internal registrations
  unconstrained.
- Default retry logic: 10 retries with exponential backoff (40ms →
  3s cap).
- SQLite `busy_timeout = 30 000 ms` under WAL mode tolerates
  multi-actor contention bursts.
- Postgres test fixture slots bound Docker startup and migrations only. Helpers
  release the slot before returning the initialized store; retaining it until
  test cleanup can deadlock Go's parallel-test barrier.
- `ledger_entries.entry_id` and `wallet_utxo_log.entry_id` use
  `INTEGER PRIMARY KEY AUTOINCREMENT` to prevent rowid reuse after
  deletion, preserving append-only ordering.
- Per-subsystem logging via the instance logger, not the global
  package logger.
- `oor_dispatch_attempts` is the permanent identity boundary for keyed sends.
  Its primary key admits one session per caller key, and its unique
  `session_id` admits one caller key per deterministic operation. A failure,
  terminal update, or incoming self-transfer never releases a dispatched key.
- `request_data` is the versioned canonical output-index, value, and pkScript
  record. The first submit-capable checkpoint inserts it in the same
  transaction as the mutable snapshot and first transport enqueue. A repeated
  insert must match both session id and bytes or the full transaction fails.
- `ListNonTerminalOORSessionRegistry` intentionally filters on the mutable
  session row's current `status`.
  `oorRegistryBehavior.resolveSelfTransfer` defers a self-transfer incoming
  hint until the outgoing lifecycle is terminal, and
  `sessionBehavior.restore` independently rejects an earlier direction
  replacement. After takeover, the incoming lifecycle's current status and
  snapshot are the state that boot restore must resume; the separate dispatch
  row still answers keyed replay.
- `unilateral_exit_jobs.exit_policy_kind` and `exit_policy_ref`
  persist the durable final spend policy identity. Standard timeout
  jobs use `standard_vtxo_timeout` with an empty ref; policy-specific
  jobs store their registered kind plus the domain-owned durable ref
  needed to reconstruct the same spend policy after restart.

### js/wasm SQLite (`sqlite_open_wasm.go`)

- **Two hosts, one driver.** The VFS name comes from
  `internal/wasmhost.SQLiteVFS()`: `nodefs` under Node, `opfs` in a
  browser. Under Node the configured path is passed through as-is
  (it is already unique and findable on disk); in a browser it is
  mangled by `browserSQLiteFileName` into a stable origin-local name,
  so two networks' `client.db` cannot silently collide within one
  origin.
- **`require_persistent=true` is mandatory.** The daemon's databases
  are the only record of VTXO, swap, and round state — there is no
  server to re-fetch them from. Refusing an in-memory substitute makes
  a storage failure a startup error rather than a wallet that looks
  healthy and forgets everything on exit.
- **`locking_mode=EXCLUSIVE` is what makes WAL reachable**, not an
  optimization. Neither wasm VFS implements `xShmMap`, so the only WAL
  available is the mode SQLite documents for hosts without shared
  memory, where an EXCLUSIVE connection keeps the WAL index on the
  heap. The driver hoists the locking mode ahead of the journal mode
  for that reason and reads the effective mode back, so a regression
  fails the open instead of quietly running on the rollback journal
  with `synchronous` tuned for WAL.
- `journal_mode` travels as its own DSN key rather than in the pragma
  list, because only that route is checked against the mode SQLite
  actually ended up in. `fullfsync` is dropped: it is a Darwin-only
  barrier that neither wasm VFS can express.
- The handle is single-connection (`SetMaxOpenConns(1)`); multiple SQL
  connections would race the same database through one worker.

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
- `000016_round_sweep_delay` — adds `rounds.sweep_delay`. A round is
  checkpointed at `input_sig_sent` and can confirm after a restart; the
  confirmation handler derives each new VTXO's absolute batch expiry as
  `confirmation_height + sweep_delay`, so a delay held only in memory made
  a resumed round stamp `BatchExpiry == CreatedHeight` and the wallet read
  the VTXO back as already expired. `InsertRound` only adopts a non-zero
  incoming value, since the delay is fixed for the life of a round and a
  later checkpoint must not clear what an earlier one recorded. Rows
  predating the column read back zero, which the confirmation path treats
  as "unknown" and leaves the expiry unstamped rather than wrong.
- `000017_idempotent_receive_scripts` — adds retry identity, immutable
  registration terms, a stable remote RPC key, and completion evidence to
  `owned_receive_scripts`. The partial unique index admits one durable
  allocation per non-null idempotency key.
- `000018_oor_outgoing_replay` — adds `oor_dispatch_attempts`, an immutable
  key/session/canonical-request table separate from the mutable lifecycle row.
  SQL cannot normalize opaque legacy snapshots, so migration backfills one
  conservative key/session binding with `request_data = NULL`; this prevents a
  duplicate send but cannot recover recipient outpoints. If legacy failed and
  non-failed rows reused one key, the non-failed row wins deterministically.
  The down migration only drops the new table.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
- [docs/postgres_isolation.md](../docs/postgres_isolation.md) — Isolation
  policy: read-only Postgres transactions run at `REPEATABLE READ` with
  `READ ONLY` (no `SIRead` predicate locks, never a 40001), writers stay
  `SERIALIZABLE`. Also holds the write-path snapshot-isolation audit and the
  inventory of partial unique indexes that any new `ON CONFLICT`
  target has to be checked against, along with the caveat that a conflict
  target can also miss a plain unique constraint declared inline in a
  `CREATE TABLE`.
