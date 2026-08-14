# internal

## Purpose

Internal helpers not importable from outside the module. This includes test
utilities and shared production-only constants that should stay scoped to this
module.

## Sub-Packages

- `internal/actortest` — Durable actor integration tests using real DB backends (SQLite, Postgres), verifying at-least-once delivery, exactly-once dedup, FIFO ordering, and atomic state+outbox.
- `internal/cmd/tools/accounting` — DB-backed admin command that reports ledger balances, event totals, and optional BTC/fiat valuation.
- `internal/indexerlimits` — Shared client-side bounds for indexer pagination cursors.
- `internal/sqlbase` — `js && wasm`-only `walletdb`-compatible SQL backend
  (SQLite over `go-wasmsqlite`), used by `lwwallet` for browser builds.
- `internal/testutils` — Deterministic key pair and Schnorr signature generation for tests.
- `internal/wasmhost` — `js && wasm`-only host detection (browser page vs.
  Node process) and the `go-wasmsqlite` VFS name that follows from it. The
  single place that answer is derived.

## Relationships

- **Depends on**: `baselib/actor`, `db` (real backends for integration tests),
  `btcwallet/walletdb` (sqlbase's wasm backend), `syscall/js` (wasmhost).
- **Depended on by**: internal module packages only, plus — for `js && wasm`
  builds — `lwwallet` (via `internal/sqlbase` and `internal/wasmhost`), `db`
  and `cmd/wavewalletdk-wasm` (via `internal/wasmhost`).
