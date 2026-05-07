# db/actordelivery/sqlc

## Purpose

sqlc-generated type-safe query layer for the actor-delivery schema. All files
in this package are generated from `db/actordelivery/` SQL definitions — do
not edit them manually. Regenerate with `make sqlc`.

## Key Types

- `Querier` — Generated interface for actor delivery SQL operations: enqueue,
  claim, ack, dead-letter, and heartbeat. Used by
  `db/actordelivery.Store` to implement `baselib/actor.TxAwareDeliveryStore`.
- `Queries` — Concrete struct implementing `Querier`. Wraps a `DBTX`.

## Relationships

- **Depends on**: `database/sql`.
- **Depended on by**: `db/actordelivery` (uses `Querier` for all mailbox
  persistence operations).

## Invariants

- All files are generated. Do not edit manually.
- Schema migrations are managed separately by `db/actordelivery/migrations`
  so the actor delivery store can be embedded in services that do not need
  the full client schema.

## Deep Docs

- [db/actordelivery/CLAUDE.md](../CLAUDE.md) — Parent package overview.
- [ARCHITECTURE.md](../../../ARCHITECTURE.md) — System-wide package map.
