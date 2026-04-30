# baselib/actor

## Purpose

Core actor framework providing typed, message-driven concurrent components with
durable mailbox persistence, service discovery via `Receptionist`, and
crash-safe at-least-once delivery with exactly-once deduplication.

## Key Types

- `Actor[M, R]` — Generic actor with typed message `M` and response `R`. Processes messages sequentially from its mailbox.
- `ActorBehavior[M, R]` — Interface that actors implement: `Start`, `Receive`, `Stop`.
- `ActorConfig[M, R]` — Configuration for actor creation (behavior, mailbox, codec, delivery store).
- `ActorRef[M, R]` — Typed reference for sending messages to an actor (`Tell`, `Ask`).
- `TellOnlyRef[M]` — Fire-and-forget reference (no response type).
- `ActorSystem` — Container managing actor lifecycles, registration, and
  shutdown. `DeadLetters() ActorRef[Message, any]` returns the dead-letter
  outlet configured via `ActorConfig.DLO`.
- `SystemConfig` — Configuration for `NewActorSystem`. `Log
  fn.Option[btclog.Logger]` injects a logger into the actor runtime; pass
  `fn.None` to disable actor-system-level tracing.
- `ServiceKey[M, R]` — Typed key for actor discovery via `Receptionist`.
  Methods: `Broadcast(sys, ctx, msg)` for fan-out to all registered actors,
  `Unregister(sys, ref)` to remove a single ref, `UnregisterAll(sys)` to
  remove all refs for this key.
- `Receptionist` — Service locator mapping `ServiceKey` → `ActorRef` for decoupled actor wiring.
- `Message` — Sealed interface for all actor messages (must embed `BaseMessage`).
- `MessageCodec` — TLV-based codec for message serialization/deserialization.
- `DeliveryStore` / `TxAwareDeliveryStore` — Interfaces for durable mailbox persistence (enqueue, claim, ack, dead-letter).
- `DurableActor` — Actor variant with crash-safe mailbox backed by SQL persistence. Provides `Wait(ctx)` to block until the actor stops and `StopAndWait(ctx)` to request a graceful shutdown and then wait.
- `DurableActorConfig[M, R]` — Configuration struct for `DurableActor`: behavior, store, codec, clock, DLO, WaitGroup, `TellRetryPolicy`, lease/heartbeat/poll durations, max attempts, cleanup timeout, and deduplication TTL.
- `DefaultDurableActorConfig[M, R]()` — Constructor returning a `DurableActorConfig` with safe defaults (30s lease, 10 max attempts, 100ms poll, DefaultTellRetryPolicy).
- `TellRetryPolicy` — Function type `func(attempts int, lastErr error) (bool, time.Duration)` determining retry behavior for failed Tell messages. Return `(false, _)` to dead-letter immediately.
- `DefaultTellRetryPolicy` — Exponential backoff policy: up to 5 attempts, starting at 1s, capped at 60s.
- `Checkpoint` — Serializable actor state snapshot for recovery.
- `WithoutOutboxID` — Context helper that strips the propagated outbox ID so child operations do not inherit the parent's delivery tracking scope.
- `Promise[T]` / `Future[T]` — Async result types for Ask-pattern responses.
- `ChannelMailbox[M, R]` — In-memory channel-based mailbox (non-durable, for lightweight actors).

## Relationships

- **Depends on**: `lnd/tlv` (message serialization).
- **Depended on by**: All domain actors (`round`, `vtxo`, `oor`, `wallet`, `serverconn`, `timeout`, `indexer`), `baselib/protofsm` (FSM-to-actor bridge), `db/actordelivery` (persistence implementation).

## Invariants

- Messages are processed sequentially per actor — no concurrent `Receive` calls.
- `Tell` with a `DurableActor` persists the message before returning (crash-safe enqueue).
- Outbox messages are dispatched only after state is persisted (outbox pattern).
- `ServiceKey` lookup via `Receptionist` is type-safe: mismatched types return `ErrServiceKeyTypeMismatch`.
- `RestartMessage` has `RestartPriority` (MaxInt32) ensuring it is processed before all other messages on recovery.
- Transaction context (`WithTx`/`RequireTx`) enables same-DB-transaction joining between actors and their callers.

## Deep Docs

- [baselib/CLAUDE.md](../CLAUDE.md) — Parent baselib package overview.
- [docs/durable_actor_architecture.md](../../docs/durable_actor_architecture.md) — Durable actor internals.
- [docs/durable_actor_quickstart.md](../../docs/durable_actor_quickstart.md) — TLVMessage, ActorBehavior, migration checklist.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
