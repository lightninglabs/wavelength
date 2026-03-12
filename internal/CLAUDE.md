# internal

## Purpose

Internal test helpers not importable from outside the module.

## Sub-Packages

- `internal/actortest` — Durable actor integration tests using real DB backends (SQLite, Postgres), verifying at-least-once delivery, exactly-once dedup, FIFO ordering, and atomic state+outbox.
- `internal/testutils` — Deterministic key pair and Schnorr signature generation for tests.

## Relationships

- **Depends on**: `baselib/actor`, `db` (real backends for integration tests).
- **Depended on by**: nothing (internal, test-only).
