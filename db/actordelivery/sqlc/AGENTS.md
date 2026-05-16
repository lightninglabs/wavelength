# db/actordelivery/sqlc

## Purpose

Generated sqlc query bindings for the durable actor mailbox database schema.
Do not edit — regenerate via `make sqlc`.

## Key Types

- `Querier` — Generated interface exposing all typed SQL operations for
  durable actor mailbox persistence: mailbox message enqueue/claim/ack,
  outbox batch claiming and completion, FSM checkpoint read/write/delete,
  Ask result store, dead-letter management, and expiry cleanup.
- `MailboxMessage` / `OutboxMessage` / `FsmCheckpoint` / `AskResult` /
  `DeadLetter` — Generated row structs mapping directly to SQL table columns.

## Relationships

- **Depends on**: nothing beyond standard `database/sql`.
- **Depended on by**: `db/actordelivery` (production `DeliveryStore`
  implementation that wraps these generated queries).

## Invariants

- All files in this directory are generated. Never edit them manually.
- Regenerate by running `make sqlc` after changing `db/queries/` or
  `db/schema/`.

## Deep Docs

- [ARCHITECTURE.md](../../../ARCHITECTURE.md) — System-wide package map.
