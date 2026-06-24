# db/actordelivery

## Purpose

Isolated SQL integration surface for durable actor mailbox persistence.
Separates actor-delivery schema lifecycle from the broader client schema so
other services can reuse durable actor storage without pulling unrelated tables.

## Key Types

- `NewTxAwareDeliveryStoreFromDB` — Constructs an `actor.TxAwareDeliveryStore`
  from a raw `*sql.DB` and backend type.
- `RunMigrations` — Applies only actor-delivery schema migrations with a
  dedicated migration bookkeeping table.
- `Store` — Standalone (non-transactional) delivery store. Implements
  `actor.DeliveryStore` and `actor.OutboxWakeRegistrar`. Created via
  `NewStore(db, clock)`. `RegisterOutboxWake(wake func())` registers a
  same-process callback fired after each `EnqueueOutbox` commit, allowing
  outbox publishers to wake immediately rather than waiting for the next poll
  tick. Multiple wakes can be registered; each is called after every enqueue.
- `TxActorDeliveryStore` — Transaction-scoped delivery store wrapping a live
  `*sql.Tx`. Implements `actor.DeliveryStore` directly against the transaction
  without additional `ExecTx` wrapping. `EnqueueOutbox` sets a shared
  `*outboxEnqueued` flag and `EnqueueMessage` calls `noteMailboxEnqueued` (see
  below) so `TxAwareActorDeliveryStore.ExecTx` can fire targeted post-commit
  wakes at exactly the mailboxes that received a message.
- `TxAwareActorDeliveryStore` — Extends `Store` with `ExecTx` support for
  atomic multi-operation workflows (implements `actor.TxAwareDeliveryStore`).
  Created via `NewTxAwareActorDeliveryStore(db, querier, clock)`. `ExecTx`
  installs a per-transaction mailbox-enqueue set via `withMailboxEnqueueSet`,
  opens a raw `*sql.Tx`, attaches it to the context via `actor.WithTx`, passes
  a `TxActorDeliveryStore` scoped to that transaction, and on successful commit
  fires outbox-wake callbacks and targeted mailbox wakes for each mailbox that
  received a message inside the transaction — avoiding a coarse broadcast to
  every registered mailbox on every unrelated commit.
- `mailbox_wake_context.go` — Internal context helpers for per-transaction
  mailbox-enqueue tracking. `withMailboxEnqueueSet` installs a
  `map[string]struct{}` keyed by mailbox ID into the context.
  `noteMailboxEnqueued` records a mailbox ID into that set from any enqueue
  path (tx-scoped store or the folded outbox path's ambient-tx join). The map
  is reference-typed, so both paths share one backing store with no lock needed
  inside a single-goroutine transaction body.
- `ActorDeliveryQueries` — Interface for all actor delivery SQL operations:
  mailbox enqueue/lease/peek/ack/nack/extend/expire, ask results, outbox
  claim/complete/fail, deduplication, FSM checkpoints, dead letters, and
  cleanup. Implemented by the SQLC-generated query set.
- **Leaseless single-worker consume path** — `PeekNextMailboxMessage` (a
  READ-only claim that mirrors `LeaseNextMailboxMessage`'s eligibility and
  ordering but takes no lease and does NOT bump attempts),
  `AckMailboxMessageByID` (unfenced delete by id), and
  `NackMailboxMessageByID` (unfenced release that increments attempts).
  Exposed on both `Store` and `TxActorDeliveryStore` as `PeekNextMessage`
  (read tx), `AckMessageByID`, and `NackMessageByID`. Used only by
  `NumWorkers == 1` Read/Commit actors, which have no competing consumer to
  fence; the multi-worker pool keeps lease + fenced ack. The by-ID nack
  increments attempts because the peek does not, preserving dead-lettering
  on max attempts.
- `BatchedActorDeliveryQueries` — Batched transaction wrapper for
  `ActorDeliveryQueries`.
- `MigrationOption` — Functional options for migration configuration
  (`WithDatabaseName`, `WithMigrationsTable`).
- `PeekMailboxParams` / `NackMailboxByIDParams` — Exported type aliases
  for the leaseless query parameter types (`adsqlc.PeekNextMailboxMessageParams`
  / `adsqlc.NackMailboxMessageByIDParams`).

## Sub-Packages

- `db/actordelivery/migrations` — Migration runner and embedded SQL migration
  files.
- `db/actordelivery/sqlc` — Generated type-safe query layer (do not edit
  manually).

## Relationships

- **Depends on**: `baselib/actor` (implements `TxAwareDeliveryStore` and
  `OutboxWakeRegistrar` interfaces), `db` (uses `BatchedQuerier`, `WriteTxOption`,
  `ReadTxOption`).
- **Depended on by**: `darepod` (wires delivery store at startup),
  `internal/actortest` (integration tests).

## Invariants

- Uses a separate migration bookkeeping table from the main client schema to
  allow independent versioning.
- The `sqlc` sub-package is generated code — regenerate via `make sqlc`,
  never edit manually.
- Migration runner is idempotent: safe to call on every startup.
- Outbox-wake callbacks and targeted mailbox-wake callbacks are called outside
  any lock and outside any transaction; callers must not assume ordering
  relative to future `ExecTx` calls from other goroutines.
- `ExecTx` fires targeted wakes only for mailboxes whose IDs were recorded in
  the per-transaction enqueue set; idle mailboxes that received no message in
  a given transaction are not woken, avoiding the O(n) broadcast cost under
  load from every durable actor re-polling on every unrelated commit.
- `TxAwareActorDeliveryStore.ExecTx` always defers `tx.Rollback()` so partial
  writes cannot survive a function error; commit is the only success path.
- `PeekNextMessage` is a read-only eligibility check, not an ownership write.
  The store adapter must map every peeked row to an empty lease token before it
  reaches the actor layer, including rows that still carry stale expired lease
  metadata from a previous leased claim. The by-ID nack path clears that stale
  metadata while incrementing attempts, preserving the leaseless p-model:
  `peek -> empty-token delivery -> by-ID ack/nack`.

## Deep Docs

- [db/CLAUDE.md](../CLAUDE.md) — Parent db package overview.
- [docs/durable_actor_architecture.md](../../docs/durable_actor_architecture.md) — Durable actor internals.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
