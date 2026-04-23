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
  production store now persists `PolicyTemplate` bytes (from
  `vtxo.Record.PolicyTemplate`) alongside `cosigner_key` as the primary
  ownership-semantics field; the legacy `(owner_key, operator_key_desc,
  exit_delay)` enrichment path (`enrichRecordDescriptorMetadataTx`) has been
  removed. Uses `InsertVTXOIfAbsent` for postgres-safe duplicate inserts.
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
  flows can materialize VTXOs. The `enrichRecordDescriptorMetadataTx` helper
  has been removed; materialization now uses the `PolicyTemplate` bytes stored
  on the VTXO record directly.
- `OORSessionStoreDB.ApplyFinalizeAndMaterialize` — Atomic OOR finalize path
  that persists the finalized checkpoint set, marks consumed inputs spent,
  and materializes recipient outputs in a single transaction. Implements
  `oor.FinalizeAtomicStore`.
- `LedgerEntry` — Type alias for `fees.LedgerEntry`; the adapter flattens
  the typed domain fields (`AccountID`, `LedgerEventType`,
  `btcutil.Amount`, `time.Time`) to the raw sqlc parameter shape
  (strings, int64 Unix seconds) at the boundary. Fields include
  `SessionID` (OOR-scoped events) and `IdempotencyKey` (partial unique
  index for replay dedup) alongside the round-scoped `RoundID`.
- `LedgerStoreDB` — Adapter that wraps `TransactionExecutor[*sqlc.Queries]`
  and exposes `InsertLedgerEntry(ctx, LedgerEntry)` plus
  `GetAccountBalance(ctx, AccountID)`. `InsertLedgerEntry` runs
  `qtx.InsertLedgerEntry` inside `ExecTx(WriteTxOption(), ...)` so
  schema CHECK / FK violations roll back atomically and successful inserts
  commit independently of later failures. The sqlc query uses `ON CONFLICT
  DO NOTHING` against the partial unique index
  `(idempotency_key, event_type, debit_account, credit_account) WHERE
  idempotency_key IS NOT NULL`, so at-least-once mailbox redelivery with a
  stable key is a silent no-op rather than a constraint violation. The
  adapter discards the rowcount returned by the `:execrows` query today;
  if a future caller needs to distinguish inserted from silently-deduped
  it can plumb the return up without a schema change.
  `GetAccountBalance` wraps the sqlc `GetAccountBalance` single-pass
  conditional aggregation (debits add, credits subtract) under a
  read-only transaction and returns a `btcutil.Amount`. It satisfies
  `ledger.LedgerBalanceReader`, feeding `LedgerActor.Start`'s treasury
  tracker rehydration so a process restart converges the in-memory
  utilization counter to DB truth before the mailbox opens.
- `UTXOAuditStoreDB` — Adapter that wraps `TransactionExecutor[*sqlc.Queries]`
  and satisfies both `ledger.UTXOAuditStore` (write path:
  `InsertWalletUTXOLog` under `WriteTxOption`, idempotent via
  `ON CONFLICT DO NOTHING` on `UNIQUE(outpoint_hash, outpoint_index, event)`)
  and `ledger.UTXOSnapshotReader` (`ListLiveWalletUTXOs` under
  `ReadTxOption`, reconstructs the current wallet UTXO set as
  "created without a paired spent", plus `CountAuditRows` under
  `ReadTxOption` that returns the total `wallet_utxo_log` row
  count so the ledger actor's reseed can distinguish a genuine
  fresh install from a running deployment whose wallet is
  temporarily empty). Also exposes `PromotePendingWalletUTXOLog(ctx,
  watermark)`: promotes `wallet_utxo_log` rows whose `source_id` was
  matched by a round / sweep handler pre-insert from
  `classification='pending'` to their final attributed classification
  (e.g. `round_funding`, `round_change`, `sweep_consumption`,
  `sweep_return`), and returns the promoted entries for external-* ledger
  booking of unattributed leftovers. One adapter, one `wallet_utxo_log`
  table, one source of truth for UTXO state.
- `FeeScheduleStoreDB` — Append-only store for hot-reloaded fee schedules
  (`fee_schedule_history` table). `InsertFeeSchedule` appends one row per
  `UpdateFeeSchedule` admin RPC call; `LatestFeeSchedule` returns the most
  recently persisted schedule (by `(created_at, id)` descending). The server
  reads the latest persisted schedule on startup and seeds the `fees.Calculator`
  before falling through to the config-file defaults, so a runtime schedule
  change survives a process restart without requiring a config edit.
- `GetVTXOStatsByStatus` / `GetRoundStatsByStatus` / `GetOORSessionStatsByState`
  — Aggregate queries used by the metrics `SystemCollector` at scrape time.
- `GetOORCheckpointByInput` — Returns the checkpoint PSBT for the checkpoint
  that consumed a given input outpoint. Used to extract condition witness data
  (e.g., preimage) from a finalized checkpoint.
- `GetBroadcastableOORCheckpointByInput` — Like `GetOORCheckpointByInput` but
  restricted to checkpoints whose session is in `awaiting_notify` or
  `finalized` state, i.e., checkpoints that are safe to broadcast. Backing the
  `oor.DBSessionStore.LoadCheckpointTxByInput` seam used by batchwatcher.
- `GetOORSpendingSessionTxidByInput` — Returns the OOR session txid that
  consumed a given input outpoint (joins `oor_checkpoints` and `oor_sessions`).
- `OORSessionSpendsScript` — Reports whether an OOR session consumed at least
  one VTXO with the provided pkScript. Used by indexer query-auth to gate
  `GetOORSessionByTxid` access.
- `VTXOStoreDB.MarkVTXOUnrolledByClient` — Transitions a live VTXO to
  `unrolled_by_client` status. Called by batchwatcher after detecting a
  recognized client-owned leaf spend (forfeit, OOR, CSV) to release the lock.
- `VTXORecordStoreDB.FindByPkScript` — Retrieves a VTXO record by pkScript
  rather than by outpoint. Used for authorization lookups.
- `SerializeVTXODescriptor` / `DeserializeVTXODescriptor` — TLV codec helpers
  (in `rounds_codec.go`) for encoding/decoding `tree.VTXODescriptor` values
  in the DB-backed round store.

## Relationships

- **Depends on**: `clientconn` (client identity types), `rounds` (round state
  types), `vtxo` (VTXO record types), `fees` (`LedgerEntry`, `Schedule`),
  `ledger` (`UTXOAuditStore`, `UTXOSnapshotReader` seams), `db/sqlc`
  (generated query layer).
- **Depended on by**: `rounds`, `oor`, `indexer`, `metrics` (scrape-time
  aggregate queries), `ledger` (`LedgerStoreDB` satisfies `fees.LedgerStore`
  and `ledger.LedgerBalanceReader`; `UTXOAuditStoreDB` satisfies
  `ledger.UTXOAuditStore` and `ledger.UTXOSnapshotReader`), root `darepo`
  (all consume storage interfaces). `batchsweeper` reaches the DB indirectly
  via an `OnBatchSwept` callback wired in the root package.

## Invariants

- Transaction atomicity: entire checkpoint succeeds or none (no partial
  writes on crash).
- Default retry logic: 10 retries with exponential backoff (40ms initial,
  capped at 3s).
- **Never write raw SQL in Go** — add queries to `db/queries/`, regenerate
  with `make sqlc`.
- Schema changes go through `db/sqlc/migrations/`; run `make sqlc` after
  changes. `LatestMigrationVersion` is currently `13`. Key migrations:
  - `000013_round_attribution` — adds `change_output_idx INTEGER NOT NULL
    DEFAULT -1` to `rounds` and creates the `round_connector_outputs` side
    table `(round_id, output_index)` so the UTXO diff classifier can
    short-circuit external_deposit booking for round-minted outputs (change
    + connector dust) after a daemon restart.
  - `000012_utxo_attribution` — adds `source_id BLOB` column to
    `wallet_utxo_log` (carries 16-byte `round_id` / `batch_id` for
    handler-pre-inserted rows, NULL for diff-loop-produced rows) and seeds
    four new `utxo_classifications`: `withdrawal`, `sweep_consumption`,
    `pending`, `round_change`. The `pending` classification is the
    diff-loop's two-phase default for unattributed movements; a later
    `PromotePendingWalletUTXOLog` reconcile resolves them.
  - `000011_utxo_audit_log` — adds the `wallet_utxo_log` table (with a
    `UNIQUE(outpoint_hash, outpoint_index, event)` constraint consumed
    via `ON CONFLICT DO NOTHING`) for the per-block UTXO diff audit trail.
  - `000010_accounting` — seeds the double-entry ledger, including
    `session_id`, `idempotency_key`, `CHECK (round_id IS NULL OR
    session_id IS NULL)`, and the nine chart-of-accounts rows
    (`external_funding` equity + four per-product revenue accounts).
  - `000009_vtxo_events_metadata` — adds `value_sat`, `round_id`,
    `batch_expiry_height`, `relative_expiry`, `origin`, `commitment_txid`
    to `indexer_vtxo_events` so poll queries match the transient push.
- Receive-script metadata columns (`owner_pubkey`, `operator_pubkey`,
  `exit_delay`) on `indexer_receive_scripts` (migration 000006) round-trip
  as nil when the registration is not a standardized Ark VTXO receive script.
- VTXO status includes `Expired` for VTXOs in swept batches (set by
  `batchsweeper` after successful sweep).
- Duplicate VTXO inserts must go through `InsertVTXOIfAbsent` to stay
  postgres-safe; in-memory and DB-backed stores must agree on the
  `PolicyTemplate` bytes and `pkScript` before accepting a duplicate. The old
  `(owner_key, operator_key_desc, exit_delay)` check has been replaced by the
  policy-template equality check.
- The TLV round/VTXO codec now encodes `(pkScript, amount, cosigner_key,
  policy_template)` via `vtxoDescCoSignerKeyType`/`vtxoDescPolicyType`. The
  old `(exit_delay, owner_key, operator_key, signing_key)` TLV types are
  gone; blobs written with the new codec are not backward-compatible with
  pre-PR-187 data.
- OOR finalize must use `FinalizeAtomicStore.ApplyFinalizeAndMaterialize`
  when both a VTXO store and a session store are configured — this closes
  the crash window where inputs could be marked spent before recipient
  outputs and session state were durably written.
- Ledger entries are strictly double-entry: schema enforces `amount_sat > 0`
  and `debit_account <> credit_account`. The sum of all account balances
  across the seeded chart of accounts must always be zero. `LedgerStoreDB`
  is the only sanctioned write path so the ExecTx wrapper guarantees inserts
  are committed (or rolled back) atomically per call.
- `wallet_utxo_log` is append-only and doubles as the UTXO-state source
  of truth. `UTXOAuditStoreDB.ListLiveWalletUTXOs` reconstructs the
  current set via the `ListLiveWalletUTXOs` sqlc query (every
  `event='created'` row lacking a paired `event='spent'` row). The
  `UNIQUE(hash, index, event)` constraint keeps the query O(n) rather
  than quadratic and guarantees every outpoint has at most one of
  each row, so the reconstruction never double-counts.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
