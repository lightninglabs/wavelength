# internal/sqlbase

## Purpose

SQL database abstraction layer that implements the `btcwallet` `walletdb.DB`
interface over SQLite/Postgres for WASM environments. Maps the nested bucket
semantics used by btcwallet's embedded KV store to parent-child SQL rows,
allowing `lwwallet` to use a browser-safe SQLite backend instead of bbolt.
Only compiles under `//go:build js && wasm`.

## Key Types

- `Config` — database configuration: `DriverName`, `Dsn`, `Timeout`,
  `Schema`, `TableNamePrefix`, `SQLiteCmdReplacements`,
  `WithTxLevelLock` flag.
- `db` (unexported) — implements `walletdb.DB`; created by `NewSqlBackend`.

## Relationships

- **Depends on**: `btcwallet/walletdb` (interface to satisfy), `lnd/sqldb`
  (connection helpers), `database/sql`.
- **Depended on by**: `lwwallet` (WASM wallet backend, `walletdb_wasm.go`).
- **Sends**: nothing.
- **Receives**: nothing (called by lwwallet at wallet open time).

## Invariants

- Build constraint `//go:build js && wasm` gates the entire package; no
  other build target sees it. Non-WASM lwwallet paths use a different
  walletdb backend.
- Nested buckets are modeled with a `parent_id` foreign key and a unique
  index on `(parent_id, key)`. Schema changes require a migration.
- `Init(maxConnections int)` must be called once before `NewSqlBackend` to
  initialize the global connection pool; calling `NewSqlBackend` without
  `Init` panics.

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
