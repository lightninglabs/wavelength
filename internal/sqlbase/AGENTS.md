# internal/sqlbase

## Purpose

WASM-only `walletdb.DB` implementation that maps LND's bucket/key-value
walletdb interface onto a single generic SQL table, so the browser build
(`GOOS=js GOARCH=wasm`) can back wallet storage with the same
Postgres/SQLite `database/sql` connection used elsewhere, without the
CGO-based on-disk `bbolt`/native SQLite drivers that aren't available in
wasm. Ported from `lnd/kvdb/sqlbase`.

## Key Types

- `db` — `walletdb.DB` implementation; owns the `*sql.DB` connection, the
  per-instance table name (`<prefix>_kv`), and an optional
  transaction-level `sync.RWMutex` (`Config.WithTxLevelLock`).
- `Config` — connection config: `DriverName`, `Dsn`, `Timeout`, `Schema`,
  `TableNamePrefix` (namespaces tables since SQLite has no schemas),
  `SQLiteCmdReplacements` (case-sensitive keyword substitution for
  cross-backend DDL), `WithTxLevelLock`.
- `NewSqlBackend(ctx, cfg) (*db, error)` — opens/reuses a pooled
  connection via the global `dbConnSet`, creates the KV table if absent,
  returns the `walletdb.DB`. Requires `Init` to have been called first.
- `Init(maxConnections int)` — one-time global `dbConnSet` initializer;
  no-op if already initialized.
- `dbConnSet` / `dbConn` — process-global, DSN-keyed connection pool with
  reference counting; `Close` only closes the underlying `*sql.DB` once
  the last user releases it.
- `readWriteTx` — wraps a live `*sql.Tx`; `Commit`/`Rollback` release the
  configured locker (real `RWMutex` or `noopLocker`) exactly once via the
  `active` guard.
- `readWriteBucket` — one row-scoped bucket keyed by `(parent_id, key)`;
  `NULL parent_id` is the root bucket, `NULL value` marks a nested-bucket
  row (vs. a leaf key/value row).
- `readWriteCursor` — key-ordered cursor over `parentSelector(id)` rows
  in a single bucket's table scope.
- `newKVSchemaCreationCmd` — builds the `CREATE TABLE IF NOT EXISTS`
  DDL for the KV table plus its indexes, applying `SQLiteCmdReplacements`
  as a final string-substitution pass.

## Relationships

- **Depends on**: `github.com/btcsuite/btcwallet/walletdb` (interface
  contract), `github.com/lightningnetwork/lnd/sqldb` (retry/serialization
  helpers via `ExecuteSQLTransactionWithRetry`, `MapSQLError`,
  `IsSerializationError`).
- **Depended on by**: `lwwallet` (`lwwallet/walletdb_wasm.go` calls
  `Init`/`NewSqlBackend` to construct the wasm build's walletdb backend).

## Invariants

- Every file in this package carries the `//go:build js && wasm` tag;
  none of it compiles or runs in the native daemon build.
- `NewSqlBackend` requires `Init` to have run first — `dbConns == nil`
  is a hard error, not a lazy-init fallback, so callers must sequence
  startup correctly.
- A row's `value` column distinguishes bucket vs. leaf semantics: `NULL`
  means "this key is a sub-bucket", non-NULL means "this key holds a
  value". Code paths that create/read/delete keys must preserve this
  distinction or `CreateBucket`/`Put`/`Delete` will misclassify rows
  (see the explicit `ErrBucketExists`/`ErrIncompatibleValue` branches).
- `dbConnSet` reference-counts connections by DSN; closing a `db` before
  all sharing users have closed must not tear down the shared
  `*sql.DB` out from under them.
- Root-bucket uniqueness relies on two partial unique indexes
  (`<table>_unp` for `parent_id IS NULL`, `<table>_up` for
  `parent_id IS NOT NULL`) because a single index cannot enforce
  uniqueness across NULL `parent_id` values in Postgres; `Put`'s
  `ON CONFLICT` clauses must target the matching partial index via its
  `WHERE` predicate.

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
