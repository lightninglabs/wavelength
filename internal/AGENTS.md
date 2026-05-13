# internal

## Purpose

Internal helpers not importable from outside the module. This includes test
utilities and shared production-only constants that should stay scoped to this
module.

## Sub-Packages

- `internal/actortest` — Durable actor integration tests using real DB backends (SQLite, Postgres), verifying at-least-once delivery, exactly-once dedup, FIFO ordering, and atomic state+outbox.
- `internal/indexerlimits` — Shared client-side bounds for indexer pagination cursors.
- `internal/testutils` — Deterministic key pair and Schnorr signature generation for tests.

## Relationships

- **Depends on**: `baselib/actor`, `db` (real backends for integration tests).
- **Depended on by**: internal module packages only.
