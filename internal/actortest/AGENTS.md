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
- Timeout constants: `outboxForwardProcessingTimeout` (5s), `outboxDeliveryTimeout` (30s), `durableAskResponseTimeout` (10s). The delivery timeout is intentionally long to reduce scheduler-induced flakiness under the race detector.

## Relationships

- **Depends on**: `baselib/actor`, `db/actordelivery` (real backends, not mocks).
- **Depended on by**: nothing (test-only).
