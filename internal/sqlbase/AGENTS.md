# internal/sqlbase

## Purpose

WebAssembly-gated SQL backend that implements `walletdb.DB` (btcwallet's
key-value database interface) over a raw SQL connection. Provides the
`walletdb`-compatible transaction layer needed to run `btcwallet` inside a
WASM environment where the standard BoltDB-backed implementation is not
available.

This package is gated on `//go:build js && wasm` — it compiles only for
the `wasm` target and is invisible to native builds.

## Key Types

- `Config` — SQL connection configuration. Fields: `DriverName`, `Dsn`,
  `Timeout`, `Schema`, `TableNamePrefix`, `SQLiteCmdReplacements`,
  `WithTxLevelLock`.
- `db` (unexported) — concrete `walletdb.DB` implementation. Wraps a
  `*sql.DB` connection and a global connection set. Implements
  `View`, `Update`, `BeginReadWriteTx`, `BeginReadTx`, `Close`.
- `Init(maxConnections)` — initializes the global connection set; must be
  called before `NewSqlBackend`.
- `NewSqlBackend(ctx, cfg)` — opens or reuses a connection from the global
  set, creates the KV schema table, and returns a `walletdb.DB`.

## Relationships

- **Depends on**: `btcwallet/walletdb` (DB/ReadTx/ReadWriteTx interfaces),
  `lnd/sqldb` (transaction retry logic, serialization error detection).
- **Depended on by**: `lwwallet` (wasm build: provides the walletdb backend
  for the embedded btcwallet), `db/migrate` (wasm build: migration driver).
- **Sends**: nothing.
- **Receives**: nothing.

## Invariants

- `Init` is idempotent; a second call when the connection set is already
  initialized is a no-op.
- `NewSqlBackend` fails with an error if `Init` has not been called, rather
  than panicking or returning a typed nil.
- Transaction retries are handled via `sqldb.ExecuteSQLTransactionWithRetry`
  with `DefaultNumTxRetries = 50` to absorb SQLite serialization errors.

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
