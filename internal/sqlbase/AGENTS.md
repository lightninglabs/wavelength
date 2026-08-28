# internal/sqlbase

## Purpose

A `walletdb`-compatible key/value backend implemented over `database/sql`.
It emulates `btcwallet/walletdb` buckets/cursors on top of a relational
table so `lwwallet` can run the wallet stack on any `database/sql` driver
instead of the BoltDB/`kvdb` backends: `go-wasmsqlite` (OPFS) in the
browser, where BoltDB is unavailable at all, and `modernc.org/sqlite` on
native builds.

## Key Types

- `db` (unexported) — implements `walletdb.DB`. Constructed via
  `NewSqlBackend(ctx, cfg *Config)`. `View`/`Update` drive read/read-write
  transactions with retry via a caller-supplied `reset` func.
- `Config` — driver name, DSN, timeout, schema, table-name prefix, per-backend
  `SQLiteCmdReplacements`, and `WithTxLevelLock` (forces a single-writer
  in-process lock).
- `readWriteTx` / `readWriteBucket` / `readWriteCursor` (unexported) —
  transaction, bucket, and cursor implementations backing `walletdb.ReadTx` /
  `walletdb.ReadWriteBucket` / `walletdb.ReadWriteCursor`; buckets and nested
  buckets are simulated as rows in a single `<prefix>_kv` table, with each
  row's `parent_id` self-referencing the row of the bucket it belongs to
  (`NULL` for the top-level bucket).
- `Init(maxConnections int)` — initializes the process-global connection
  pool (`dbConnSet`) that dedups connections by DSN across callers.

## Relationships

- **Depends on**: `btcwallet/walletdb` (interface being implemented),
  `lnd/sqldb` (shared SQL error classification).
- **Depended on by**: `lwwallet` (all platforms).

## Invariants

- `DefaultNumTxRetries = 50`: `Update`/`View` retry on transaction errors that
  permit repetition, calling the caller's `reset` before each retry.
- `WithTxLevelLock` serializes all read-write transactions through a single
  in-process lock; omit it only for backends that tolerate concurrent writers.
- Buckets/cursors are simulated over SQL tables, not a native KV store — key
  ordering and cursor semantics must match `walletdb`'s contract exactly, or
  callers built against `walletdb` (e.g. the wallet's key-derivation state)
  silently misbehave.

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
