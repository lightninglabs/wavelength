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
- `DurableActorConfig[M, R]` — Configuration struct for `DurableActor`: behavior, store, codec, clock, DLO, WaitGroup, `TellRetryPolicy`, lease/heartbeat/poll durations, max attempts, cleanup timeout, deduplication TTL, and `NumWorkers`.
- `DurableActorConfig.NumWorkers` — How many concurrent worker loops drain the actor's single mailbox. Default and any value `<= 1` is one worker (strictly-sequential processing). A value `> 1` turns the actor into a competing-consumer pool: that many goroutines each lease distinct messages via `LeaseNextMailboxMessage`, so independent messages run in parallel while per-correlation-key FIFO still keeps same-key messages ordered. Only for behaviors whose handlers are concurrency-safe and hold no writer across their side effects (e.g. the serverconn egress sender on the Read/Commit path). `NewDurableActor` **fails closed** with `ErrConcurrentClassicBehavior` when `NumWorkers > 1` is paired with a classic (`Left`) `ActorBehavior`, since the classic path wraps the whole `Receive` in one write transaction and assumes sequential delivery; pools are only valid on the Read/Commit (`TxBehavior`) path. The test-only `DurableActorConfig.AllowConcurrentClassicBehavior()` escape hatch bypasses the guard for the egress benchmark that measures the forbidden config; production code must never call it.
- `DefaultDurableActorConfig[M, R]()` — Constructor returning a `DurableActorConfig` with safe defaults (30s lease, 10 max attempts, 1s poll fallback, DefaultTellRetryPolicy).
- `TellRetryPolicy` — Function type `func(attempts int, lastErr error) (bool, time.Duration)` determining retry behavior for failed Tell messages. Return `(false, _)` to dead-letter immediately.
- `DefaultTellRetryPolicy` — Exponential backoff policy: up to 5 attempts, starting at 1s, capped at 60s.
- `Checkpoint` — Serializable actor state snapshot for recovery.
- `WithoutOutboxID` — Context helper that strips the propagated outbox ID so child operations do not inherit the parent's delivery tracking scope.
- `Promise[T]` / `Future[T]` — Async result types for Ask-pattern responses.
- `DetachAskPromise[R](ctx)` / `DetachedAsk[R]` — Read/Stage/Commit-path
  behaviors can take ownership of an Ask delivery's promise and complete it
  after their turn returns (e.g. from a downstream future's `OnComplete`),
  so a pure-routing coordinator never parks its goroutine on `Await`. The
  framework still completes a *failed* turn's promise with the error (the
  continuation may never have been wired); completion is first-wins.
  Continuations must use `DetachedAsk.CallerCtx`, not the turn context,
  which is cancelled when the turn returns. Returns false for Tells,
  DurableAsks, and redelivered asks whose caller is gone.
- `ChannelMailbox[M, R]` — In-memory channel-based mailbox (non-durable, for lightweight actors).
- `Mailbox[M, R]` — Interface for actor message queues: `Send(ctx, env) error` (blocking; returns `ErrMailboxClosed`, `ErrActorTerminated`, or a context error on failure), `TrySend(env) error` (non-blocking), `Receive(ctx) iter.Seq[envelope]`, `Close()`, `IsClosed() bool`, `Drain() iter.Seq[envelope]`.
- `isExpectedShutdownErr(err) bool` — Internal helper that classifies errors as expected during teardown: context cancellation/deadline, closed DB handle ("sql: database is closed", "sql: connection is already closed", "use of closed network connection"). Used by the lease loop to demote shutdown-path failures to debug instead of warn-flooding test artifacts at itest tail.
- `Message.CorrelationKey() string` — Per-message FIFO key consumed by the
  durable mailbox's claim path. Non-empty keys participate in per-key FIFO:
  a message is claim-eligible only when no earlier same-key message
  (compared by UUIDv7 `id`) exists in the same mailbox, even if the
  earlier message is in retry backoff. Empty (the default on
  `BaseMessage`) means the message is unkeyed and uses the existing
  global `available_at` claim order. The override site is the concrete
  message struct (e.g. `clientconn.ClientMessage` types in `rounds`),
  not the framework — the framework just plumbs the value through
  `EnqueueParams.CorrelationKey`.
- `EnqueueParams.CorrelationKey` — Per-enqueue override stamped into the
  `mailbox_messages.correlation_key` column. Populated automatically from
  `msg.CorrelationKey()` by `DurableMailbox.Send`. A zero (empty) value
  preserves the legacy unkeyed claim semantics.

## Relationships

- **Depends on**: `lnd/tlv` (message serialization).
- **Depended on by**: All domain actors (`round`, `vtxo`, `oor`, `wallet`, `serverconn`, `timeout`, `indexer`), `baselib/protofsm` (FSM-to-actor bridge), `db/actordelivery` (persistence implementation).

## Invariants

- Messages are processed sequentially per actor by default (one worker, no concurrent `Receive` calls). Opting into `DurableActorConfig.NumWorkers > 1` relaxes this: that many worker loops drain the one mailbox concurrently, so `Receive` may run in parallel across distinct messages. The competing-consumer lease guarantees each message is still processed by exactly one worker, and per-correlation-key FIFO holds across workers; only behaviors with concurrency-safe handlers should set it. The combination is structurally restricted to the Read/Commit path: `NewDurableActor` rejects `NumWorkers > 1` on a classic `ActorBehavior` with `ErrConcurrentClassicBehavior` so a stateful, sequentially-assumed actor can never be silently fanned out.
- `Tell` with a `DurableActor` persists the message before returning (crash-safe enqueue).
- Outbox messages are dispatched only after state is persisted (outbox pattern).
- `ServiceKey` lookup via `Receptionist` is type-safe: mismatched types return `ErrServiceKeyTypeMismatch`.
- `RestartMessage` has `RestartPriority` (MaxInt32) ensuring it is processed before all other messages on recovery.
- Transaction context (`WithTx`/`RequireTx`) enables same-DB-transaction joining between actors and their callers.
- `Mailbox.Send` returns the exact failure error (`ErrMailboxClosed`, `ErrActorTerminated`, `context.Canceled`, `context.DeadlineExceeded`) rather than a boolean; `Tell` and `Ask` propagate this directly to callers.
- During daemon teardown, the underlying DB is closed before every actor's lease loop has wound down. The lease loop uses `isExpectedShutdownErr` to demote these "database is closed" errors to debug level; real operational errors still surface as warnings because neither the actor context nor the outer context is done in those cases.
- **Per-correlation-key FIFO claim.** Two messages in the same mailbox that
  share a non-empty `CorrelationKey()` are processed in emission order
  regardless of retry backoff. Without this invariant, a transient Tell
  failure on msg1 would Nack-with-backoff (push `available_at` into the
  future), and a later-enqueued msg2 with a smaller `available_at` would
  overtake msg1 in the `LeaseNextMailboxMessage` claim. The fix is an
  anti-join on `mailbox_messages.id` (UUIDv7, strictly orderable at
  millisecond granularity) so the head of each correlation key drains
  before any later same-key row is claim-eligible. Unkeyed messages
  (empty `CorrelationKey()`) keep the legacy global `available_at`
  order and do not interfere with keyed lanes. Head-of-line blocking
  is bounded to the correlation key, not the mailbox; consumers are
  already strictly serial per mailbox so this does not regress
  throughput.

## Deep Docs

- [baselib/CLAUDE.md](../CLAUDE.md) — Parent baselib package overview.
- [docs/durable_actor_architecture.md](../../docs/durable_actor_architecture.md) — Durable actor internals.
- [docs/durable_actor_quickstart.md](../../docs/durable_actor_quickstart.md) — TLVMessage, ActorBehavior, migration checklist.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
