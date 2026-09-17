# internal/actortest

## Purpose

Durable actor integration tests using real DB backends (SQLite, Postgres).
Verifies at-least-once delivery, exactly-once deduplication, FIFO ordering,
priority ordering, dead-letter and retry-ceiling invariants, DurableAsk/outbox-delivered
responses, concurrent senders/asks, recovery/restart scenarios, and atomic
state+outbox checkpointing.

## Key Test Infrastructure

- `testHarness` / `newTestHarness` — Central test scaffolding: sets up a
  per-test in-memory SQLite DB, `actor.ActorSystem`, and TX-aware actor
  delivery store, plus raw SQL queries for mailbox-row assertions; tests create
  their own `actor.OutboxPublisher` per case.
- `CounterBehavior` / `CounterMessage` (`IncrementMsg`, `DecrementMsg`,
  `GetCountMsg`, `ForwardMsg`) — Demo durable actor and TLV-coded messages
  used to drive the e2e scenarios.
- `eventuallyWithOutboxPublish` — Helper that actively triggers `OutboxPublisher.PublishPending()` on every polling iteration, making outbox delivery assertions robust under the race detector and CI scheduler pressure.
- `newLedgerActorForTest` (`ledger_e2e_test.go`) — Wires a real
  `ledger.LedgerActor` on the durable mailbox against the same SQLite DB, so
  ledger writes join the actor's fenced `Commit` transaction as in production.
- `nackingSendStore` (`ledger_session_lane_test.go`) — Store wrapper that fails
  the first insert of an outgoing OOR send leg, nacking it into retry backoff
  so the session's receive (enqueued later but immediately claimable) would
  overtake it. Backs `TestOORSelfChangeSurvivesANackedSend`, which proves the
  mailbox's correlation-key lane keeps the two legs of one OOR session in
  order regardless of the claim order the mailbox would otherwise pick.
- Timeout constants: `outboxForwardProcessingTimeout`, `outboxDeliveryTimeout`,
  `durableAskResponseTimeout` — all 30s, kept aligned since DurableAsk
  responses and forwards are also delivered through the outbox.

## Relationships

- **Depends on**: `baselib/actor`, `db` / `db/actordelivery` (real backends,
  not mocks), `ledger` (`LedgerActor` e2e coverage).
- **Depended on by**: nothing (test-only).
