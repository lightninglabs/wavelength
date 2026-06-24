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
- `DeliveryStore` — Interface (defined in `delivery_store.go`) for durable
  mailbox persistence: enqueue, lease, ack, nack, extend, dead-letter,
  deduplication, checkpoint, and outbox operations. The leaseless
  single-worker fast path adds `PeekNextMessage` (read-only claim, no lease,
  no attempts bump; yields an empty lease token), `AckMessageByID` (unfenced
  delete), and `NackMessageByID` (unfenced release that increments attempts).
  The internal helpers `ackMessage` and `nackMessage` (also in
  `delivery_store.go`) route to the fenced or unfenced operation based on
  whether the delivery carries a lease token, keeping every ack/nack call
  site consistent.
- `TxAwareDeliveryStore` — Extends `DeliveryStore` with `ExecTx` for running
  a `TxFunc` inside a DB transaction. Enables atomic FSM updates, the classic
  `processInTransaction` path, the Read/Commit `processWithExec` path, and the
  folded outbox-delivery single-write-transaction in `OutboxPublisher`.
- `TxFunc` — `func(ctx context.Context, store DeliveryStore) error` executed
  inside an `ExecTx` transaction.
- `OutboxWakeRegistrar` — Optional interface on stores that can notify the
  same-process `OutboxPublisher` after new outbox work commits. Polling
  remains the cross-process and restart fallback.
- `MailboxWakeRegistrar` — Optional interface on stores (defined in
  `delivery_store.go`) that can notify a same-process `DurableMailbox` after
  an enqueue commits inside an `ExecTx`. The folded outbox-delivery path
  enqueues into the target mailbox inside the publisher's write transaction, so
  the pre-commit in-process wake fired by `DurableMailbox.Send` races ahead of
  row visibility. The store fires the registered wakes only after the tx
  commits, restoring same-process delivery latency without waiting out a full
  poll interval. Wakes are targeted by mailbox ID: only mailboxes that received
  a message in a given transaction are roused. Each `DurableMailbox` registers
  its `Wake` under its own mailbox ID on construction and calls the returned
  cancel on `Close`, so a stopped mailbox leaves no stale closure.
- `DurableMailboxConfig` — Configuration for `DurableMailbox`: `MailboxID`,
  `Store`, `Codec`, `Clock`, `LeaseDuration`, `PollInterval`, `MaxAttempts`,
  `WakeBuffer` (sizes the wake channel; set to `NumWorkers` by
  `NewDurableActor` so a burst rouses every idle worker), and
  `SingleWorkerLeaseless` (enables the leaseless peek path for single-worker
  Read/Commit actors).
- `DurableActor` — Actor variant with crash-safe mailbox backed by SQL persistence. Provides `Wait(ctx)` to block until the actor stops and `StopAndWait(ctx)` to request a graceful shutdown and then wait.
- `DurableActorConfig[M, R]` — Configuration struct for `DurableActor`: behavior, store, codec, clock, DLO, WaitGroup, `TellRetryPolicy`, lease/heartbeat/poll durations, max attempts, cleanup timeout, deduplication TTL, and `NumWorkers`.
- `DurableActorConfig.NumWorkers` — How many concurrent worker loops drain the actor's single mailbox. Default and any value `<= 1` is one worker (strictly-sequential processing). A value `> 1` turns the actor into a competing-consumer pool: that many goroutines each lease distinct messages via `LeaseNextMailboxMessage`, so independent messages run in parallel while per-correlation-key FIFO still keeps same-key messages ordered. Only for behaviors whose handlers are concurrency-safe and hold no writer across their side effects (e.g. the serverconn egress sender on the Read/Commit path). `NewDurableActor` **fails closed** with `ErrConcurrentClassicBehavior` when `NumWorkers > 1` is paired with a classic (`Left`) `ActorBehavior`, since the classic path wraps the whole `Receive` in one write transaction and assumes sequential delivery; pools are only valid on the Read/Commit (`TxBehavior`) path. The test-only `DurableActorConfig.AllowConcurrentClassicBehavior()` escape hatch bypasses the guard for the egress benchmark that measures the forbidden config; production code must never call it.
- `DefaultDurableActorConfig[M, R]()` — Constructor returning a `DurableActorConfig` with safe defaults (30s lease, 10 max attempts, 1s poll fallback, DefaultTellRetryPolicy).
- `DefaultDurableTxActorConfig[M, R, S]()` — Constructor for the Read/Commit
  execution path. Takes a `TxBehavior` and its `StoreFactory`, binds them via
  `NewTxBehaviorEither`, and returns a config with the Right case populated.
  The store must be a `TxAwareDeliveryStore` (enforced by `NewDurableActor`).
- `NewClassicBehavior[M, R]` — Wraps a classic `ActorBehavior` as the Left
  case of `DurableActorConfig.Behavior`. `DefaultDurableActorConfig` applies
  it automatically; use it directly only on hand-built configs.
- `NewTxBehaviorEither[M, R, S]` — Binds a `TxBehavior` and its
  `StoreFactory` into the Right case of `DurableActorConfig.Behavior`.
  `DefaultDurableTxActorConfig` applies it automatically.
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
  which is cancelled when the turn returns. `CallerCtx` is NOT a reliable
  carrier of the caller's deadline: on the durable (Read/Stage/Commit)
  path — the path that actually adopts detaching — the caller's context is
  never persisted with the durable Ask, so `CallerCtx` is the actor's own
  lifetime context, not the caller's, and a real caller deadline never
  flows into the continuation (it is observed only by the caller's own
  `future.Await`). On the non-durable channel-mailbox path `CallerCtx` is
  the originating send context. Because the durable path's `CallerCtx`
  does not cancel on a caller hang-up, a detaching behavior MUST wrap
  `CallerCtx` in `context.WithTimeout` itself before handing it to
  `OnComplete` — that wrap is the sole bound on the continuation. Returns
  false for Tells, DurableAsks, and redelivered asks whose caller is gone.
  Defined in `ask_detach.go`.
- `Delivery[M, R]` — Wraps a claimed message with ack/nack semantics.
  `MutationFailed()` reports whether the last `Nack` store write failed to
  persist its row mutation; the worker loop backs off for a poll interval
  before re-claiming when this is set, because a failed leaseless nack leaves
  the row unchanged and immediately re-eligible (there is no lease expiry to
  throttle the next peek). `EffectiveAttempts()` returns the retry-policy
  attempt count: leased deliveries are pre-incremented at claim time;
  leaseless peeked deliveries are not, so the current in-flight attempt is
  counted here. `ShouldDeadLetter()` uses `EffectiveAttempts()` to match the
  leased dead-letter boundary on both paths.
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
- **Leaseless consume ownership model.** `SingleWorkerLeaseless` removes the
  lease-token fence, so its safety argument is "one live runtime owner for this
  mailbox", not merely "one goroutine in this process". Do not enable it for a
  mailbox that can be drained by another daemon/process at the same time unless
  an external singleton/ownership fence already exists. A peeked delivery always
  carries an empty lease token, even when the persisted row still has stale
  expired lease metadata from an older leased claim; that empty token is the
  p-model edge that routes ack/nack to the by-ID operations. Retry-policy
  decisions must use `Delivery.EffectiveAttempts()` so the in-flight peeked
  attempt is counted before a nack can raise the row to `max_attempts`.
  `NewDurableActor` sets `SingleWorkerLeaseless` strictly when `NumWorkers == 1`
  AND the behavior is the Read/Commit (Right/`TxBehavior`) path; the
  multi-worker pool and the classic path keep `LeaseNextMessage` and the
  lease-fenced ack byte-for-byte. Heartbeating is skipped for leaseless
  deliveries (no lease to extend), avoiding spurious "Failed to extend lease"
  warnings.
- `Tell` with a `DurableActor` persists the message before returning (crash-safe enqueue).
- Outbox messages are dispatched only after state is persisted (outbox pattern).
- **Outbox fold p-model.** For tx-aware stores, `OutboxPublisher` folds the
  target mailbox enqueue and `CompleteOutbox` into ONE write transaction:
  `claim -> (Tell + CompleteOutbox) in one ExecTx`. This halves the
  per-delivery commit count and closes the enqueue-without-complete
  redelivery window. If the transaction fails, both the enqueue and
  completion roll back; the claim expiry is the retry mechanism. The
  publisher logs any transaction-level begin/commit failure even when the
  inner Tell/Complete operations returned nil. Because the enqueue runs
  inside an ambient transaction, the row is invisible to the target
  consumer until the outer tx commits; a `MailboxWakeRegistrar` store fires
  the per-mailbox wake only after the tx commits, so the consumer re-polls
  against a now-visible row instead of waiting out a full poll interval.
  Polling remains the cross-process and restart fallback.
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
