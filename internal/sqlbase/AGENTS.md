# internal/sqlbase

## Purpose

WASM-only (`js && wasm`) implementation of btcwallet's `walletdb.DB`
interface backed by a SQL key-value table instead of bbolt. Adapted from
lnd's `channeldb/kvdb/sqlbase` so `lwwallet` can run btcwallet in the browser,
where bbolt's mmap-based storage is unavailable, by driving the same
bucket/cursor semantics over a `database/sql` connection (in practice SQLite
via the `wasmsqlite` driver, OPFS-backed).

## Key Types

- `db` — Implements `walletdb.DB`. Holds the `*sql.DB` connection set,
  table-name prefix, and schema. `NewSqlBackend(ctx, cfg)` constructs one;
  `Init(maxConnections)` must be called first to size the global connection
  pool. `View`/`Update`/`BeginReadTx`/`BeginReadWriteTx` open transactions;
  `Copy`/`PrintStats`/`Close` round out the `walletdb.DB` contract.
- `Config` — Connection parameters: `DriverName`, `Dsn`, `Timeout`, `Schema`
  (empty for SQLite, which has no multi-schema support), `TableNamePrefix`,
  `SQLiteCmdReplacements`, `WithTxLevelLock`.
- `readWriteTx` / `readWriteBucket` / `readWriteCursor` — Implement
  `walletdb.ReadWriteTx`, `walletdb.ReadWriteBucket`, and
  `walletdb.ReadWriteCursor` respectively over the single `kv` table, where
  every row points to its parent bucket via `parent_id` (NULL means
  top-level). Buckets are represented by rows whose `value` is NULL and whose
  `id` is referenced as another row's `parent_id`.
- `SQLiteCmdReplacements` — One-to-one keyword substitution map (`schema.go`)
  applied when generating the KV table's `CREATE TABLE`/`CREATE INDEX`
  statements, letting the same schema-construction code target SQLite-flavored
  syntax.
- `dbConnSet` — Tracks open `*sql.DB` connections keyed by DSN so repeated
  `NewSqlBackend` calls against the same database reuse one pool instead of
  exhausting the WASM single-connection budget.

## Relationships

- **Depends on**: `github.com/btcsuite/btcwallet/walletdb` (interface being
  implemented), `github.com/lightningnetwork/lnd/sqldb` (retry/error
  classification helpers), `database/sql`.
- **Depended on by**: `lwwallet` (`walletdb_wasm.go` calls `Init`,
  `sqlbase.Config`, and `NewSqlBackend` to open btcwallet's walletdb over
  `go-wasmsqlite` in browser builds).

## Invariants

- Every file in this package carries the `js && wasm` build tag — it is
  compiled only for browser/WASM targets and must never be pulled into
  native darepod builds. Do not remove the build tag or add native-build call
  sites.
- The `kv` table's bucket/key model must stay behaviorally compatible with
  `walletdb`'s bucket semantics (nested buckets via `parent_id`, sequence
  numbers, cursor ordering) since consumers (btcwallet internals) assume
  standard `walletdb` behavior, not SQL-specific behavior.
- This is a vendored/adapted copy of lnd's `sqlbase` package (see `LICENSE`);
  keep changes minimal and behavior-preserving so future upstream fixes can
  still be ported across with a straightforward diff.
- `Init(maxConnections)` must run before `NewSqlBackend`; the WASM build uses
  a small fixed connection budget (`wasmWalletDBMaxConnections = 1` at the
  `lwwallet` call site) because the browser SQL driver does not support
  unbounded concurrent connections.

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
