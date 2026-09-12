# internal

## Purpose

Internal helpers not importable from outside the module. This includes test
utilities and shared production-only constants that should stay scoped to this
module.

## Sub-Packages

- `internal/actortest` — Durable actor integration tests using real DB backends (SQLite, Postgres), verifying at-least-once delivery, exactly-once dedup, FIFO ordering, and atomic state+outbox.
- `internal/cmd/tools/accounting` — DB-backed admin command that reports ledger balances, event totals, and optional BTC/fiat valuation.
- `internal/indexerlimits` — Shared client-side bounds for indexer pagination cursors.
- `internal/sqlbase` — `walletdb`-compatible SQL backend, used by `lwwallet`
  on every platform: SQLite over `go-wasmsqlite` in the browser, over
  `modernc.org/sqlite` natively.
- `internal/testutils` — Deterministic key pair and Schnorr signature generation for tests.
- `internal/wasmhost` — `js && wasm`-only host detection (browser vs Node) and
  the durable SQLite VFS name that follows from it.

## Relationships

- **Depends on**: `baselib/actor`, `db` (real backends for integration tests),
  `btcwallet/walletdb` (the interface sqlbase implements).
- **Depended on by**: internal module packages only, plus `lwwallet` (all
  platforms via `internal/sqlbase`, wasm builds via `internal/wasmhost`), `db`
  and `cmd/wavewalletdk-wasm` (wasm builds, via `internal/wasmhost`).
