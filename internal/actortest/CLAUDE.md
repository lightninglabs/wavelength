# internal/actortest

## Purpose

Durable actor integration tests using real DB backends (SQLite, Postgres).
Verifies at-least-once delivery, exactly-once deduplication, FIFO ordering,
priority ordering, dead-letter queue invariants, DurableAsk/outbox-delivered
responses, concurrent senders/asks, recovery/restart scenarios, and atomic
state+outbox checkpointing.

## Key Test Infrastructure

- `testHarness` / `newTestHarness` — Central test scaffolding: sets up real DB, actor system, delivery store, and outbox publisher.
- `eventuallyWithOutboxPublish` — Helper that actively triggers `OutboxPublisher.PublishPending()` on every polling iteration, making outbox delivery assertions robust under the race detector and CI scheduler pressure.
- Timeout constants: `outboxForwardProcessingTimeout` (30s), `outboxDeliveryTimeout` (30s), `durableAskResponseTimeout` (30s) — aligned to the same budget since DurableAsk responses and chained forwards are also delivered through the outbox and CI can starve these SQLite-backed, `-race` tests.

## Relationships

- **Depends on**: `baselib/actor`, `db/actordelivery` (real backends, not mocks).
- **Depended on by**: nothing (test-only).
