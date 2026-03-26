# db/actordelivery

## Purpose

Isolated SQL integration surface for durable actor mailbox persistence.
Separates actor-delivery schema lifecycle from the broader client schema so
other services can reuse durable actor storage without pulling unrelated tables.

## Key Types

- `NewTxAwareDeliveryStoreFromDB` — Constructs an `actor.TxAwareDeliveryStore` from a raw `*sql.DB` and backend type.
- `RunMigrations` — Applies only actor-delivery schema migrations with a dedicated migration bookkeeping table.
- `ActorDeliveryQueries` — Interface for actor delivery SQL operations (enqueue, claim, ack, dead-letter).
- `BatchedActorDeliveryQueries` — Batched transaction wrapper for `ActorDeliveryQueries`.
- `MigrationOption` — Functional options for migration configuration.

## Sub-Packages

- `db/actordelivery/migrations` — Migration runner and embedded SQL migration files.
- `db/actordelivery/sqlc` — Generated type-safe query layer (do not edit manually).

## Relationships

- **Depends on**: `baselib/actor` (implements `TxAwareDeliveryStore` interface).
- **Depended on by**: `darepod` (wires delivery store at startup), `internal/actortest` (integration tests).

## Invariants

- Uses a separate migration bookkeeping table from the main client schema to allow independent versioning.
- The `sqlc` sub-package is generated code — regenerate via `make sqlc`, never edit manually.
- Migration runner is idempotent: safe to call on every startup.

## Deep Docs

- [db/CLAUDE.md](../CLAUDE.md) — Parent db package overview.
- [docs/durable_actor_architecture.md](../../docs/durable_actor_architecture.md) — Durable actor internals.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
