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
  `actor.DeliveryStore`, `actor.OutboxWakeRegistrar`, and
  `actor.MailboxWakeRegistrar`. Created via `NewStore(db, clock)`.
  `RegisterOutboxWake(wake func())` registers a same-process callback fired
  after each `EnqueueOutbox` commit, allowing outbox publishers to wake
  immediately rather than waiting for the next poll tick. Multiple wakes can
  be registered; each is called after every enqueue.
  `RegisterMailboxWake(mailboxID string, wake func()) (cancel func())`
  registers a same-process callback fired after a commit that enqueued a
  message into that specific mailbox — see "Mailbox Wake" below.
- `TxActorDeliveryStore` — Transaction-scoped delivery store wrapping a live
  `*sql.Tx`. Implements `actor.DeliveryStore` directly against the transaction
  without additional `ExecTx` wrapping. `EnqueueOutbox` sets a shared
  `*outboxEnqueued` flag, and `EnqueueMessage` calls `noteMailboxEnqueued` on
  the tx context, so `TxAwareActorDeliveryStore.ExecTx` can fire outbox-wake
  and targeted mailbox-wake callbacks after commit.
- `TxAwareActorDeliveryStore` — Extends `Store` with `ExecTx` support for
  atomic multi-operation workflows (implements `actor.TxAwareDeliveryStore`).
  Created via `NewTxAwareActorDeliveryStore(db, querier, clock)`. `ExecTx`
  opens a raw `*sql.Tx`, attaches it to the context via `actor.WithTx` plus a
  fresh per-tx mailbox-enqueue set (`withMailboxEnqueueSet`), passes a
  `TxActorDeliveryStore` scoped to that transaction, and on successful commit
  fires outbox-wake callbacks (if any outbox row was enqueued) and targeted
  `notifyMailboxWake` callbacks for exactly the mailbox IDs recorded in the
  set.
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
- **Mailbox wake** (`mailbox_wake_context.go`) — `withMailboxEnqueueSet(ctx)`
  installs a fresh `map[string]struct{}` of enqueued mailbox IDs on the
  context for one `ExecTx`; `noteMailboxEnqueued(ctx, mailboxID)` records a
  target mailbox into that set (no-op outside an `ExecTx`). Both
  `(*Store).EnqueueMessage` and `(*TxActorDeliveryStore).EnqueueMessage` call
  `noteMailboxEnqueued` so the set is populated regardless of which store path
  an enqueue takes — including the folded outbox-delivery path, where the
  target actor's plain `Store` joins the publisher's ambient transaction via
  `TransactionExecutor.ExecTx` rather than going through
  `TxActorDeliveryStore` directly. `TxAwareActorDeliveryStore.ExecTx` reads the
  set back after commit and calls `(*Store).notifyMailboxWake(mailboxIDs)`,
  which fires only the wake callbacks registered (via `RegisterMailboxWake`)
  for those specific mailbox IDs — idle mailboxes untouched by the transaction
  are never woken.

## Sub-Packages

- `db/actordelivery/migrations` — Migration runner and embedded SQL migration
  files.
- `db/actordelivery/sqlc` — Generated type-safe query layer (do not edit
  manually).

## Relationships

- **Depends on**: `baselib/actor` (implements `TxAwareDeliveryStore`,
  `OutboxWakeRegistrar`, and `MailboxWakeRegistrar` interfaces — see
  `baselib/actor/delivery_store.go` and `baselib/actor/durable_mailbox.go`,
  which registers its own wake via `RegisterMailboxWake` when the configured
  store implements `MailboxWakeRegistrar`), `db` (uses `BatchedQuerier`,
  `WriteTxOption`, `ReadTxOption`).
- **Depended on by**: `darepod` (wires delivery store at startup),
  `internal/actortest` (integration tests).

## Invariants

- Uses a separate migration bookkeeping table from the main client schema to
  allow independent versioning.
- The `sqlc` sub-package is generated code — regenerate via `make sqlc`,
  never edit manually.
- Migration runner is idempotent: safe to call on every startup.
- Outbox-wake and mailbox-wake callbacks are called outside any lock and
  outside any transaction; callers must not assume ordering relative to
  future `ExecTx` calls from other goroutines.
- `TxAwareActorDeliveryStore.ExecTx` always defers `tx.Rollback()` so partial
  writes cannot survive a function error; commit is the only success path.
- `notifyMailboxWake` must only fire for mailbox IDs actually present in the
  committed transaction's enqueue set — broadcasting to every registered
  mailbox on every commit defeats the purpose of the targeted wake (avoiding
  re-poll storms across every idle durable actor) and was the regression this
  mechanism fixes for the folded outbox-delivery path.
- `RegisterMailboxWake`'s returned `cancel` must delete only its own handle
  (keyed by a per-registration counter, not just the mailbox ID) and prune the
  mailbox's outer map entry once empty, so a restarted `DurableMailbox` that
  reuses the same durable mailbox ID never collides with, or is silently
  dropped by, a still-live prior registration.
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
