# internal/sqlbase

## Purpose

WebAssembly-only (`js && wasm` build tag) SQLite database adapter that
implements `walletdb.DB` and related `walletdb` interfaces on top of a
plain `database/sql` connection. Bridges the LND wallet's database
abstraction to a `sql.DB` backend in environments where the full
`bbolt`/`kvdb` stack is unavailable, enabling `lwwallet` to run in a
WASM context with a SQLite KV store.

## Key Types

- `DB` — Implements `walletdb.DB`. Wraps a `*sql.DB` with retry
  semantics (`DefaultNumTxRetries = 50`) and a per-transaction mutex
  for the WASM single-threaded constraint.
- `Config` — Connection parameters: DSN, max open/idle connections,
  and busy timeout.
- `SQLiteCmdReplacements` — Map of SQLite keyword substitutions applied
  at schema creation time, used to normalize commands across minor
  SQLite variants.
- `ReadWriteBucket` / `ReadWriteCursor` / `ReadWriteTx` — Implement
  the `walletdb` bucket, cursor, and transaction interfaces over the KV
  table schema.

## Relationships

- **Depends on**: `btcwallet/walletdb`, `lnd/sqldb` (for retry
  helpers), `database/sql`.
- **Depended on by**:
  - `lwwallet` (uses this adapter to open the LND wallet DB in WASM
    builds)
- **Sends**: nothing.
- **Receives**: nothing.

## Invariants

- Only compiled under `//go:build js && wasm` — the package is
  invisible to native builds.
- The KV schema (`kv` table with `parent_id`, `key`, `value`) is
  created at `Open` time; callers must not assume any pre-existing
  schema.
- `DefaultNumTxRetries = 50` with exponential backoff handles SQLite
  busy/locked errors in WASM.

## Deep Docs

- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
