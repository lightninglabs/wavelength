# baselib/actor

## Purpose

Core actor framework providing typed, message-driven concurrent components with
durable mailbox persistence, service discovery via `Receptionist`, and
crash-safe at-least-once delivery with exactly-once deduplication.

## Key Types

- `Actor[M, R]` — Generic actor with typed message `M` and response `R`. Processes messages sequentially from its mailbox.
- `ActorBehavior[M, R]` — Interface that actors implement: `Start`, `Receive`, `Stop`.
- `ActorConfig[M, R]` — Configuration for actor creation (behavior, mailbox, codec, delivery store).
- `ActorRef[M, R]` — Typed reference for sending messages to an actor (`Tell`, `TryTell`, `Ask`).
- `TellOnlyRef[M]` — Fire-and-forget reference (no response type). `Tell` blocks for mailbox room, `TryTell` never does.
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
- `DeliveryStore` / `TxAwareDeliveryStore` — Interfaces for durable mailbox persistence (enqueue, claim, ack, dead-letter). The leaseless single-worker fast path adds `PeekNextMessage` (read-only claim, no lease, no attempts bump; yields an empty lease token), `AckMessageByID` (unfenced delete), and `NackMessageByID` (unfenced release that increments attempts). A `DurableActor` enables it (via `DurableMailboxConfig.SingleWorkerLeaseless`) strictly when `NumWorkers == 1` AND the behavior is the Read/Commit (Right/`TxBehavior`) path, eliminating the per-message lease write transaction. The multi-worker pool and the classic path are byte-for-byte unchanged: they keep `LeaseNextMessage` and the lease-fenced ack. Ack/nack route to the by-ID ops automatically whenever the delivery's lease token is empty; `Delivery.ShouldDeadLetter` counts the in-flight attempt as `Attempts + 1` on the leaseless path so the dead-letter boundary matches the leased path (where attempts is pre-incremented at lease).
- `DurableActor` — Actor variant with crash-safe mailbox backed by SQL persistence. Provides `Wait(ctx)` to block until the actor stops and `StopAndWait(ctx)` to request a graceful shutdown and then wait.
- `DurableActorConfig[M, R]` — Configuration struct for `DurableActor`: behavior, store, codec, clock, DLO, WaitGroup, `TellRetryPolicy`, lease/heartbeat/poll durations, max attempts, cleanup timeout, deduplication TTL, and `NumWorkers`.
- `DurableActorConfig.NumWorkers` — How many concurrent worker loops drain the actor's single mailbox. Default and any value `<= 1` is one worker (strictly-sequential processing). A value `> 1` turns the actor into a competing-consumer pool: that many goroutines each lease distinct messages via `LeaseNextMailboxMessage`, so independent messages run in parallel while per-correlation-key FIFO still keeps same-key messages ordered. Only for behaviors whose handlers are concurrency-safe and hold no writer across their side effects (e.g. the serverconn egress sender on the Read/Commit path). `NewDurableActor` **fails closed** with `ErrConcurrentClassicBehavior` when `NumWorkers > 1` is paired with a classic (`Left`) `ActorBehavior`, since the classic path wraps the whole `Receive` in one write transaction and assumes sequential delivery; pools are only valid on the Read/Commit (`TxBehavior`) path. The test-only `DurableActorConfig.AllowConcurrentClassicBehavior()` escape hatch bypasses the guard for the egress benchmark that measures the forbidden config; production code must never call it.
- `DurableActorConfig.MaxRestarts` / `RestartWindow` — BEAM-style restart intensity budget for the supervision kernel. It defaults OFF: `DefaultMaxRestarts` is `UnlimitedRestarts` (-1) and a zero `MaxRestarts` normalizes to it, so a panicking actor restarts for as long as it keeps panicking. That is deliberate. Restarting forever is no worse than the nack-and-continue loop supervision replaces (both are rate-limited by the nack backoff), whereas a finite budget adds a failure mode the runtime did not have: the actor dies permanently and keeps looking alive to anyone who is not watching. Set a finite budget ONLY where the owner wires `Watch` and reacts to `TerminationRestartIntensityExceeded`; `RecommendedMaxRestarts` (5) over `DefaultRestartWindow` (60s) is the value to reach for when you do. Restart timestamps are tracked in a sliding window off the config's injected clock.
- `(*DurableActor).Watch(ctx) <-chan TerminationInfo` — Registers a terminal lifecycle watcher. The returned channel receives exactly one `TerminationInfo` and is then closed. Delivery is non-blocking by construction (a single-use buffer-of-one channel written once), so a slow or absent watcher can never park the actor's shutdown path (#1093 invariant). Registering after termination returns a channel already loaded with the notification, so there is no race with a stopping actor; cancelling `ctx` deregisters the watcher and closes its channel with no notification. The notification is published when the supervision loop exits, or by `Stop` for an actor that was never started; an actor that is neither started nor stopped never publishes one.
- `TerminationInfo` / `TerminationReason` — What a watcher observes: `TerminationStopped` (Stop/StopAndWait), `TerminationContextCancelled` (lifetime context died without a Stop; reserved for a future externally-owned-context constructor), `TerminationRestartIntensityExceeded` (restart budget spent, `Err` carries the panic, `RestartsExhausted` is true), `TerminationRestartFailed` (checkpoint reload or RestartMessage enqueue failed). `Restarts` counts restarts over the actor's whole lifetime.
- `MessageCodec.Supports(typeID)` — Reports whether the codec has a constructor registered for a TLV type. The supervision path uses it to decide, BEFORE tearing a generation down, whether the checkpoint hand-off is even possible; an actor whose codec never registered a `RestartMessage` degrades to cycling its worker generation with no `OnStop` and no restore, since a teardown it cannot be rebuilt from is strictly worse than leaving the behavior running.
- `PrependRestartMessageWithID` — `PrependRestartMessage` with the enqueued row ID returned. Supervision deletes the row it enqueued last time before writing the next, so a run of restarts leaves at most one restart row in the mailbox. A restart row carries `MaxAttempts` 1 and the runtime never nacks one (a nacked row at `attempts == max_attempts` is neither leasable nor reapable, so it would strand): a failed restart turn dead-letters instead, which makes restore handlers responsible for their own idempotency.
- `DefaultDurableActorConfig[M, R]()` — Constructor returning a `DurableActorConfig` with safe defaults (30s lease, 10 max attempts, 1s poll floor / 30s poll ceiling, DefaultTellRetryPolicy).
- `DurableActorConfig.PollInterval` / `MaxPollInterval` — Floor and ceiling of the idle mailbox poll backoff (defaults 1s / 30s). The fallback poll is NOT the delivery path: a same-process enqueue signals the mailbox's wake channel, and the store's post-commit `RegisterMailboxWake` callback rouses the exact mailbox a committed transaction enqueued into, so delivery latency is unaffected by how far the backoff has decayed. Each consecutive empty poll roughly doubles the wait from `PollInterval` up to `MaxPollInterval`; any wake or successfully claimed message snaps it back to the floor. This matters at scale because on a Postgres-backed store every empty poll is a full SERIALIZABLE write transaction that updates no rows, so thousands of resident-but-idle actors polling at a fixed 1Hz become a pure transaction tax. The timer is never stopped: since `RegisterMailboxWake` is same-process only, the poll remains the sole discovery mechanism for a row enqueued by another process or replica, which makes `MaxPollInterval` the worst-case cross-process/cross-replica delivery latency. A zero value normalizes to the default and a ceiling below the floor is raised to the floor (constant cadence, never a shrinking wait).
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

- **Depends on**: `lnd/tlv` (message serialization), `lnd/fn/v2` (Result/Option/Either types), `lnd/clock` (testable time), `build` (logger-from-context helper).
- **Depended on by**: All domain actors (`round`, `vtxo`, `oor`, `wallet`, `serverconn`, `timeout`), `baselib/protofsm` (FSM-to-actor bridge), `db/actordelivery` (persistence implementation).

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
- `Tell` with a `DurableActor` persists the message before returning (crash-safe enqueue).
- **Panic means restart, not redeliver.** A behavior that *returns* an error is an ordinary message failure: it nacks and retries per `TellRetryPolicy`. A behavior that *panics* is treated as corrupted in-memory state: the recovered value becomes a `behaviorPanic`, the delivery's normal ack/nack/dead-letter bookkeeping runs first (so the poison message burns its attempt), and only then does the worker hand the panic to supervision. Supervision cancels the current worker generation (draining ALL workers, not just the panicking one), runs the behavior's `OnStop` bounded by `CleanupTimeout`, reloads the persisted FSM checkpoint, prepends a `RestartMessage` at `RestartPriority`, and starts a fresh generation. The nack-before-restart ordering is load-bearing: it is what makes a deterministic poison message climb to `max_attempts` and dead-letter instead of crash-looping the actor forever.
- **A restart reuses the behavior INSTANCE; the clean slate is the handler's job.** The framework does not rebuild the behavior. It stops the workers, optionally calls `OnStop`, and redelivers a `RestartMessage`; the same Go value keeps serving afterwards with whatever fields the panic left behind. The actor is therefore clean exactly when its `RestartMessage` handler rebuilds every piece of in-memory state from the durable row, and stale otherwise. This is not hypothetical: the behaviors that adopt supervision (`credit.opBehavior`, `oor.sessionBehavior`, `oor.oorRegistryBehavior`, `unroll.behavior`) all carry a reload seam, and a handler that returns Ok without using it leaves the actor exactly as far ahead of durable truth as the panic left it. A behavior with no in-memory turn state (the `serverconn` egress sender) may consume the message as a no-op, but it should say so and say why.
- **A restart message is not retried.** It is enqueued with `MaxAttempts` 1, and the runtime dead-letters (rather than nacks) a restart turn that fails, because a nacked row at `attempts == max_attempts` strands forever. Restore handlers get exactly one shot per restart and must be idempotent.
- **`OnStop` may run mid-life and more than once.** A supervised restart calls it before the rebuild, so implementations must be idempotent and must leave the behavior able to serve a new generation rather than assuming it is being discarded. A panic escaping `OnStop` is recovered (it is invoked precisely when the behavior's invariants are broken) and terminates the actor with `TerminationRestartFailed` rather than taking the process down.
- **A panicking turn's own writes are rolled back.** On the classic path the whole `Receive` runs inside one framework transaction, so supervision returns the panic from that transaction to force a rollback and redoes the message's ack/nack bookkeeping outside it. Committing the partial writes alongside the nack would persist exactly the torn state the restart exists to escape, and the checkpoint reload would then hand it straight back.
- **Restart preserves public identity.** The restart runs on an internal generation context derived from the actor's lifetime context, deliberately bypassing the `Once`-guarded `Start`/`Stop`. The actor keeps its ID, its `DurableMailbox` (so senders keep enqueueing across the restart gap, and the mailbox's promise registry survives), and its cached `Ref`. Callers holding an `ActorRef` observe nothing beyond a pause in processing.
- **In-flight Ask promises across a restart.** The panicking turn's promise is completed with the panic error by the normal result handling. A sibling worker's turn sees its generation context cancelled, returns a context error, and has its promise completed with that error; its durable bookkeeping still runs on a detached context. A message not yet handed to the behavior is simply redelivered afterwards and its caller still gets the eventual result. `DurableAsk` responses travel through the outbox, so a restart only delays them.
- **Exceeding the restart budget is terminal, which is why it is off by default.** Once a finite `MaxRestarts` is exhausted inside `RestartWindow`, supervision logs at error level (an internal-bug class, so the level rule allows it), cancels the actor's lifetime context so further sends fail fast rather than piling into a mailbox nothing will drain, tears the actor down, and publishes `TerminationRestartIntensityExceeded` to watchers. Nothing else notices: an actor that dies this way still holds its ID and its mailbox rows, so a finite budget without a `Watch` observer converts a visible crash loop into invisible permanent death.
- Outbox messages are dispatched only after state is persisted (outbox pattern).
- **Outbox fold p-model.** For tx-aware stores, outbox delivery is
  `claim -> (target mailbox enqueue + CompleteOutbox) in one write tx`. If the
  transaction fails before commit, both the enqueue and completion roll back and
  the claim expiry is the retry mechanism; the publisher must log the
  transaction failure even when the inner Tell/Complete operations returned nil,
  because begin/commit failures happen outside those operation-level logs.
- `ServiceKey` lookup via `Receptionist` is type-safe: mismatched types return `ErrServiceKeyTypeMismatch`.
- `RestartMessage` has `RestartPriority` (MaxInt32) ensuring it is processed before all other messages on recovery.
- Transaction context (`WithTx`/`RequireTx`) enables same-DB-transaction joining between actors and their callers.
- `Mailbox.Send` returns the exact failure error (`ErrMailboxClosed`, `ErrActorTerminated`, `context.Canceled`, `context.DeadlineExceeded`) rather than a boolean; `Tell` and `Ask` propagate this directly to callers.
- **Never `Tell` from inside a receive goroutine without a bound.** A blocking
  send into a full peer mailbox parks the whole receive loop, and if the peer
  is waiting on that actor the pair deadlocks. Use `TryTell`, which returns
  `ErrMailboxFull` immediately so the caller can drop, stash, or reschedule
  the message. A `context.WithTimeout` around `Tell` is the weaker option: it
  burns the entire deadline against a peer that is already wedged.
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
