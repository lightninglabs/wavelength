# internal

## Purpose

Internal helpers not importable from outside the module. This includes test
utilities and shared production-only constants that should stay scoped to this
module.

## Sub-Packages

- `internal/actortest` — Durable actor integration tests using real DB backends (SQLite, Postgres), verifying at-least-once delivery, exactly-once dedup, FIFO ordering, and atomic state+outbox.
- `internal/cmd/tools/accounting` — DB-backed admin command that reports ledger balances, event totals, and optional BTC/fiat valuation.
- `internal/expiryfixture` — Signed VTXO ancestry fixtures for
  acceptance-boundary tests. `Round` builds one round-direct entry and its
  commitment; `Merge` builds a two-parent fan-in, optionally sharing a single
  commitment. Test-only: it takes a `testing.TB`.
- `internal/indexerlimits` — Shared client-side bounds for indexer pagination cursors.
- `internal/sqlbase` — `js && wasm`-only `walletdb`-compatible SQL backend
  (SQLite over `go-wasmsqlite`), used by `lwwallet` for browser builds.
- `internal/testutils` — Deterministic key pair and Schnorr signature generation for tests.
- `internal/wasmhost` — `js && wasm`-only host detection (browser vs Node) and
  the durable SQLite VFS name that follows from it.

## Relationships

- **Depends on**: `baselib/actor`, `db` (real backends for integration tests),
  `btcwallet/walletdb` (sqlbase's wasm backend), `arkrpc` / `lib/tree` /
  `lib/arkscript` / `lib/tx/psbtutil` (expiryfixture's signed ancestry).
- **Depended on by**: internal module packages only, plus `lwwallet` (wasm
  builds, via `internal/sqlbase` and `internal/wasmhost`), `db` and
  `cmd/wavewalletdk-wasm` (wasm builds, via `internal/wasmhost`).
