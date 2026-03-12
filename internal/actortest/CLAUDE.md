# internal/actortest

## Purpose

Durable actor integration tests using real DB backends (SQLite, Postgres).
Verifies at-least-once delivery, exactly-once deduplication, FIFO ordering, and
atomic state+outbox checkpointing.

## Relationships

- **Depends on**: `baselib/actor`, `db` (real backends, not mocks).
- **Depended on by**: nothing (test-only).
