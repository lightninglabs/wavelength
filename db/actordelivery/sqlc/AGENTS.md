# db/actordelivery/sqlc

## Purpose

Generated type-safe SQL query layer for durable actor mailbox persistence.
Produced by `make sqlc` from query files under `db/actordelivery/`. Uses a
dedicated migration table separate from the main client schema to allow
independent versioning.

## Key Generated Types

- `ActorDeliveryQueries` interface — All actor-delivery SQL operations:
  mailbox message enqueue/lease/ack/nack/extend/expire; ask-result
  insert/fetch; outbox claim/complete/fail; deduplication; FSM
  checkpoint persist/fetch; dead-letter queue insert/list; cleanup.
- Row types: `MailboxMessage`, `AskResult`, `OutboxMessage`,
  `FsmCheckpoint`, `DeadLetterMessage`.

## Relationships

- **Depends on**: SQLite / PostgreSQL drivers; standard library only.
- **Depended on by**: `db/actordelivery` (wraps in `TxActorDeliveryStore`
  and `TxAwareActorDeliveryStore`).

## Invariants

- **Never edit generated code** — regenerate via `make sqlc`.
- Query files and migration SQL live in `db/actordelivery/`.

## Deep Docs

- [db/actordelivery/CLAUDE.md](../CLAUDE.md) — Parent package overview.
- [docs/durable_actor_architecture.md](../../../docs/durable_actor_architecture.md)
  — Durable actor internals.
