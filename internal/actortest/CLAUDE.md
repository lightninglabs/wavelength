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
- Timeout constants: `outboxForwardProcessingTimeout`, `outboxDeliveryTimeout`, `durableAskResponseTimeout` — all 30s, sized for CI scheduler pressure rather than local dev speed.
- `CounterBehavior` (`counter_behavior.go`) / `CounterMessage` family (`counter_messages.go`, `NewCounterCodec`) — demo `ActorBehavior[CounterMessage, CounterResult]` implementation used by `e2e_test.go` to exercise Tell, Ask, outbox-forward, and DurableAsk (`actor.AskResponse`) paths end-to-end with TLV-encoded messages.
- `newLedgerActorForTest` / `requireBalance` (`ledger_e2e_test.go`) — wires a real `ledger.LedgerActor` on top of the same in-memory SQLite DB used by the tx-aware delivery store, so a handler's `InsertLedgerEntry` joins the actor's `Commit` transaction via `actor.TxFromContext` exactly as in production.

## Relationships

- **Depends on**: `baselib/actor`, `db`, `db/actordelivery`, `db/actordelivery/sqlc`, `db/sqlc`, `ledger` (real backends, not mocks).
- **Depended on by**: nothing (test-only).
