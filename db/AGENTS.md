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
- `LedgerEntry` — Type alias for `fees.LedgerEntry`. Uses typed `AccountID`
  and `LedgerEventType` fields rather than raw strings. `LedgerStoreDB`
  satisfies `fees.LedgerStore` (verified by compile-time assertion in
  `ledger_store.go`).
- `LedgerStoreDB` — Adapter that wraps `TransactionExecutor[*sqlc.Queries]`
  and exposes `InsertLedgerEntry(ctx, LedgerEntry)`. Each call runs the
  underlying `qtx.InsertLedgerEntry` inside `ExecTx(WriteTxOption(), ...)` so
  schema CHECK / FK violations roll back atomically and successful inserts
  commit independently of later failures.
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
  types), `vtxo` (VTXO record types), `fees` (LedgerEntry type alias and
  LedgerStore interface), `db/sqlc` (generated query layer).
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
  changes. Current head migration: `000011_utxo_audit_log` which adds the
  `wallet_utxo_log` table and `utxo_classifications`/`utxo_events` enum
  tables for tracking wallet UTXO set changes per block. Migration
  `000010_accounting` (previous) introduced the double-entry accounts,
  ledger entries, and ledger event types tables.
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

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
