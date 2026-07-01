# internal/actortest

## Purpose

Durable actor integration tests using real DB backends (SQLite, Postgres).
Verifies at-least-once delivery, exactly-once deduplication, FIFO ordering,
priority ordering, dead-letter queue invariants, DurableAsk/outbox-delivered
responses, concurrent senders/asks, recovery/restart scenarios, and atomic
state+outbox checkpointing — including that the folded outbox
`Tell`+`CompleteOutbox` delivery transaction rolls back the target mailbox
enqueue when `CompleteOutbox` fails, so a message is redelivered exactly once
rather than leaking a duplicate on the target.

## Key Test Infrastructure

- `testHarness` / `newTestHarness` — Central test scaffolding: sets up real DB, actor system, delivery store, and outbox publisher. `testHarness.store` is a `*actordelivery.TxAwareActorDeliveryStore` (not the plain `actordelivery.Store`), matching production wiring — darepod hands the `OutboxPublisher` a `TxAwareDeliveryStore` — so these e2e tests exercise the real folded `Tell`+`CompleteOutbox` single-transaction delivery path rather than a looser double-commit approximation.
- `eventuallyWithOutboxPublish` — Helper that actively triggers `OutboxPublisher.PublishPending()` on every polling iteration, making outbox delivery assertions robust under the race detector and CI scheduler pressure.
- `completeFailDeliveryStore` / `completeFailTxStore` — Fault-injection wrappers used by `TestOutboxPublisherAtomicDeliveryRollback`. `completeFailTxStore` wraps the real `TxAwareActorDeliveryStore` so the `OutboxPublisher` still detects transaction support, while its `ExecTx` interposes `completeFailDeliveryStore` (which can force `CompleteOutbox` to fail via an `atomic.Bool` flag) in front of the transaction-scoped store passed into the `TxFunc`.
- Timeout constants: `outboxForwardProcessingTimeout` (30s), `outboxDeliveryTimeout` (30s), `durableAskResponseTimeout` (30s) — raised from 5s/10s/10s and aligned with each other because DurableAsk responses and chained forwarding are also delivered through the outbox, and the full CI race suite can starve these SQLite-backed tests while other packages compete for CPU.

## Relationships

- **Depends on**: `baselib/actor`, `db/actordelivery` (real backends, not mocks).
- **Depended on by**: nothing (test-only).
