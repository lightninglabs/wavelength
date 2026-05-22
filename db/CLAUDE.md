# db

## Purpose

Database abstractions and persistent storage for all darepo-server state:
rounds, VTXOs, OOR sessions, mailbox envelopes, indexer events, the
double-entry fee ledger, and the wallet UTXO audit log. PostgreSQL + SQLite
backends via SQLC-generated type-safe queries.

## Key Concepts

Use `go doc db.<Symbol>` for signatures. Highlights:

- **`Store`** — Top-level persistence wrapper around `PostgresStore` /
  `SqliteStore`.
- **Round/VTXO stores** — `RoundStoreDB`, `VTXOStoreDB`, `VTXOLockerDB`,
  `VTXORecordStoreDB`, `RecipientEventStore`, `OORSessionStoreDB`. VTXO
  records persist `PolicyTemplate` bytes (from `vtxo.Record.PolicyTemplate`)
  + `cosigner_key` as the authoritative ownership fields; the legacy
  `enrichRecordDescriptorMetadataTx` path and the `(owner_key,
  operator_key_desc, exit_delay)` enrichment are gone. Use
  `InsertVTXOIfAbsent` for postgres-safe duplicate inserts.
- **Sentinels** — `ErrRoundNotConfirmed` from `GetConfirmedRound` lets the
  fraud responder distinguish "still in-flight" from a missing row.
- **OOR atomic finalize** — `OORSessionStoreDB.ApplyFinalizeAndMaterialize`
  persists the finalized checkpoint set, marks consumed inputs spent, and
  materializes recipient outputs in one transaction; implements
  `oor.FinalizeAtomicStore`. Required to close the crash window where
  inputs could be marked spent before recipient outputs.
- **OOR checkpoint lookups** — `GetOORCheckpointByInput` returns any
  checkpoint that consumed an input; `GetBroadcastableOORCheckpointByInput`
  restricts to `awaiting_notify`/`finalized` (used by
  `oor.DBSessionStore.LoadCheckpointTxByInput` for batchwatcher's fraud
  response). `GetOORSpendingSessionTxidByInput` joins
  `oor_checkpoints`+`oor_sessions`. `OORSessionSpendsScript` gates indexer
  query-auth.
- **Ledger adapter** — `LedgerStoreDB` wraps
  `TransactionExecutor[*sqlc.Queries]`. `InsertLedgerEntry` runs
  `qtx.InsertLedgerEntry` under `ExecTx(WriteTxOption, ...)`; the sqlc
  query uses `ON CONFLICT DO NOTHING` against the partial unique index
  `(idempotency_key, event_type, debit_account, credit_account) WHERE
  idempotency_key IS NOT NULL`, so at-least-once mailbox redelivery with
  a stable key is a silent no-op. `GetAccountBalance` runs the single-pass
  conditional aggregation (debits add, credits subtract) under
  `ReadTxOption` and satisfies `ledger.LedgerBalanceReader` for
  `LedgerActor.Start`'s treasury tracker rehydration.
- **UTXO audit adapter** — `UTXOAuditStoreDB` satisfies both
  `ledger.UTXOAuditStore` (write: `InsertWalletUTXOLog`, idempotent via
  `ON CONFLICT DO NOTHING` on `UNIQUE(outpoint_hash, outpoint_index,
  event)`) and `ledger.UTXOSnapshotReader` (`ListLiveWalletUTXOs` +
  `CountAuditRows` so reseed can distinguish fresh install from
  temporarily-empty wallet). `PromotePendingWalletUTXOLog` resolves
  unattributed `pending` rows to their final classification and returns
  the promoted entries for external-* ledger booking.
- **Fee schedule store** — `FeeScheduleStoreDB` is append-only
  (`fee_schedule_history`). `InsertFeeSchedule` records each
  `UpdateFeeSchedule` admin RPC; `LatestFeeSchedule` seeds
  `fees.Calculator` on startup so a runtime change survives restart
  without a config edit.
- **Batch-expiry inheritance** — `CreateVTXORecordTx(... inheritedBatchExpiry)`
  stamps `batch_expiry` from `inheritedBatchExpiry > 0` (OOR outputs
  inherit `min(parent.batch_expiry)` so seal-time fee math prices a refresh
  against the lineage expiry). Round-created records pass 0 and derive
  expiry at read time via `GetVTXOWithRoundExpiry`
  (`COALESCE(vtxos.batch_expiry, confirmation_height + csv_delay)`).
- **Authoritative marking** — `MarkVTXORecordsSpentTx(qtx, outpoints, owner)`
  accepts only `in_flight(owner) → spent` transitions; live VTXOs and
  VTXOs locked by a different owner return an error. `spent → spent` is
  idempotent. Mirrors `vtxo.InMemoryStore`'s invariant.
- **Mailbox store** — `MailboxEnvelopeStore` is the sqlc-backed
  `mailbox.Store` (cursor pull, monotonic ack watermarks,
  `UNIQUE(recipient, msg_id)` dedup, per-mailbox capacity). `Pull`
  surfaces `ctx.Err()` directly if cancellation races the sqlc call.
- **Metrics scrape** — `GetVTXOStatsByStatus`, `GetRoundStatsByStatus`,
  `GetOORSessionStatsByState` feed `metrics.SystemCollector`.
- **TLV codec** — `SerializeVTXODescriptor` / `DeserializeVTXODescriptor`
  in `rounds_codec.go` encode `tree.VTXODescriptor` via
  `vtxoDescCoSignerKeyType` / `vtxoDescPolicyType`. The old
  `(exit_delay, owner_key, operator_key, signing_key)` TLV types are
  gone; new-codec blobs are NOT backward-compatible with pre-#187 data.

## Migrations

`LatestMigrationVersion = 14`. Schema changes go through
`db/sqlc/migrations/` + `make sqlc`. Highlights:

- `000014_vtxo_inherited_batch_expiry` — `vtxos.batch_expiry INTEGER`
  (nullable; OOR carries inherited value, rounds use `COALESCE` at read).
- `000013_round_attribution` — `rounds.change_output_idx INTEGER NOT NULL
  DEFAULT -1`; `round_connector_outputs(round_id, output_index)`;
  `round_connector_descriptors.radix INTEGER NOT NULL` (so the fraud
  responder can reconstruct the connector tree shape after a config
  change).
- `000002_rounds` (in-place alteration) — adds `sweep_key_family BIGINT`
  + `sweep_key_index BIGINT` to `rounds`. `BIGINT` because
  `keychain.KeyFamily`/`Index` are `uint32` and Postgres `INTEGER` is
  signed 32-bit. Persists the locator so the sweeper can sign with the
  historical descriptor after a configured-key rotation.
- `000012_utxo_attribution` — `wallet_utxo_log.source_id BLOB` (16-byte
  round_id/batch_id for handler pre-inserts, NULL for diff-loop rows);
  seeds `withdrawal`, `sweep_consumption`, `pending`, `round_change`
  classifications.
- `000011_utxo_audit_log` — `wallet_utxo_log` table with
  `UNIQUE(outpoint_hash, outpoint_index, event)` for the per-block diff.
- `000010_accounting` — double-entry ledger; `session_id`,
  `idempotency_key`, `CHECK (round_id IS NULL OR session_id IS NULL)`,
  9 chart-of-accounts rows (`external_funding` equity + four per-product
  revenue accounts).
- `000009_vtxo_events_metadata` — `value_sat`, `round_id`,
  `batch_expiry_height`, `relative_expiry`, `origin`, `commitment_txid`
  on `indexer_vtxo_events` so poll matches push.

## Relationships

- **Depends on**: `clientconn`, `rounds`, `vtxo`, `fees`, `ledger`
  (interface seams), `db/sqlc` (generated layer).
- **Depended on by**: `rounds`, `oor`, `indexer`, `metrics`, `ledger`,
  root `darepo`. `batchsweeper` reaches the DB indirectly via an
  `OnBatchSwept` callback wired by root.

## Invariants

- Transaction atomicity: full checkpoint or none (no partial writes on
  crash).
- Default retry: 10 with exponential backoff (40 ms initial, 3 s cap).
- **Never write raw SQL in Go** — queries go in `db/queries/`,
  regenerate with `make sqlc`.
- `postgresSchemaReplacements` applies substitutions in
  descending-key-length order (`replacerFile`) so
  `INTEGER PRIMARY KEY AUTOINCREMENT` matches before `INTEGER PRIMARY
  KEY`. Schemas that need monotonic rowids (preventing `event_seq`
  reuse) must use `INTEGER PRIMARY KEY AUTOINCREMENT` (SQLite) /
  `BIGSERIAL PRIMARY KEY` (Postgres); plain `INTEGER PRIMARY KEY` lets
  SQLite recycle deleted rowids and breaks mailbox pull clients whose
  ack cursor sits above the recycled sequence.
- Receive-script metadata columns (`owner_pubkey`, `operator_pubkey`,
  `exit_delay`) on `indexer_receive_scripts` (000006) round-trip as
  nil for non-standardized receive scripts.
- VTXO status includes `Expired` (set by `batchsweeper` after sweep).
- Ledger entries are strictly double-entry: schema enforces
  `amount_sat > 0` and `debit_account <> credit_account`. Sum of all
  account balances must always be zero. `LedgerStoreDB` is the only
  sanctioned write path; the `ExecTx` wrapper guarantees per-call
  atomicity.
- `wallet_utxo_log` is append-only and the source of truth for UTXO
  state. `ListLiveWalletUTXOs` reconstructs the live set as "every
  `event='created'` row lacking a paired `event='spent'`". The
  `UNIQUE(hash, index, event)` constraint keeps that O(n) and prevents
  double-counting.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide map.
