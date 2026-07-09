# internal/sqlbase

## Purpose

A `walletdb.DB` implementation that emulates a bbolt-style nested
key/value/bucket hierarchy on top of a generic `database/sql` driver, so
btcwallet's walletdb consumers can run against SQL backends (SQLite/Postgres)
instead of bbolt. Built exclusively for `js && wasm` targets, where it backs
the browser OPFS SQLite store used by `lwwallet`.

## Key Types

- `Config` — Connection/driver settings (`DriverName`, `Dsn`, `Timeout`,
  `Schema`, `TableNamePrefix`, `SQLiteCmdReplacements`, `WithTxLevelLock`)
  passed to `NewSqlBackend`.
- `db` — Unexported `walletdb.DB` implementation; owns the `*sql.DB` handle,
  the per-namespace table name, and (optionally) a process-wide `sync.RWMutex`
  used when `WithTxLevelLock` is set.
- `dbConnSet` / `dbConn` — Process-global, reference-counted pool of open
  `*sql.DB` handles keyed by DSN, so multiple `NewSqlBackend` callers sharing a
  DSN reuse one connection. Initialized once via `Init(maxConnections)`.
  `Open`/`Close` are the internal ref-count accessors, incrementing on each
  new caller and closing the underlying `*sql.DB` only when the count drops
  to zero.
- `readWriteTx` — `walletdb.ReadWriteTx` implementation wrapping a single
  `*sql.Tx` opened at `sql.LevelSerializable`; provides the `QueryRow`/
  `Query`/`Exec` helpers (each with a timeout context) used by buckets and
  cursors.
- `readWriteBucket` — `walletdb.ReadWriteBucket` implementation. A bucket is a
  row group sharing a `parent_id`; nested buckets are rows with `value IS
  NULL`, leaf keys are rows with a non-NULL `value`.
- `readWriteCursor` — `walletdb.ReadWriteCursor` implementation that walks a
  bucket's rows in key order via `First`/`Next`/`Prev`/`Last`/`Seek`, tracking
  position with `currKey` (not a live SQL cursor).

## Relationships

- **Depends on**: `github.com/btcsuite/btcwallet/walletdb` (interfaces this
  package implements), `github.com/lightningnetwork/lnd/sqldb` (retryable
  transaction execution and serialization-error classification),
  `github.com/btcsuite/btclog/v2` (package logger via `UseLogger`).
- **Depended on by**: `lwwallet` (`lwwallet/walletdb_wasm.go`) — the only
  in-repo caller, using it to open btcwallet's walletdb against an
  OPFS-backed SQLite driver in the browser/WASM build.

## Invariants

- Every file carries the `//go:build js && wasm` tag; this package is never
  compiled into native darepod/darepocli binaries, only into WASM builds.
- All buckets and keys within one `Config.TableNamePrefix` share a single
  physical table (`<prefix>_kv`); the nested-bucket hierarchy is simulated via
  `parent_id` self-references, not real SQL schemas/tables per bucket.
- A row's `value IS NULL` marks it as a sub-bucket, not a stored value; `Get`
  and `Put` must preserve this distinction (an empty `[]byte{}` value is valid
  and distinct from NULL) or bucket/key semantics silently corrupt.
- `Init(maxConnections)` must run once before any `NewSqlBackend` call;
  `dbConns` is a package-global guarded by `dbConnsMu`, shared by every caller
  in the process, so connections for the same DSN are reference-counted rather
  than reopened.
- Transactions run at `sql.LevelSerializable`; callers relying on
  `db.Update`/`db.View` retries must keep their closures idempotent since
  `sqldb.ExecuteSQLTransactionWithRetry` re-invokes `f` (after calling
  `reset`) on serialization conflicts, up to `DefaultNumTxRetries`.

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map
