# internal

## Purpose

Internal helpers not importable from outside the module. This includes test
utilities and shared production-only constants that should stay scoped to this
module.

## Sub-Packages

- `internal/actortest` — Durable actor integration tests using real DB backends (SQLite, Postgres), verifying at-least-once delivery, exactly-once dedup, FIFO ordering, and atomic state+outbox.
- `internal/cmd/tools/accounting` — DB-backed admin command that reports ledger balances, event totals, and optional BTC/fiat valuation.
- `internal/indexerlimits` — Shared client-side bounds for indexer pagination cursors.
- `internal/sqlbase` — `js && wasm`-only reimplementation of btcwallet's `walletdb` KV interface on top of `database/sql`, used where no real sqlite/bbolt driver is available in wasm builds.
- `internal/testutils` — Deterministic key pair and Schnorr signature generation for tests.

## Relationships

- **Depends on**: `baselib/actor`, `db` (real backends for integration tests).
- **Depended on by**: internal module packages only.
