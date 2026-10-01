# baselib/actor

## Purpose

Core actor framework providing typed, message-driven concurrent components with
durable mailbox persistence, service discovery via `Receptionist`, and
crash-safe at-least-once delivery with exactly-once deduplication.

## Key Types

- `Actor[M, R]` — Generic actor with typed message `M` and response `R`. Processes messages sequentially from its mailbox.
- `ActorBehavior[M, R]` — Interface that actors implement: `Start`, `Receive`, `Stop`.
- `ActorConfig[M, R]` — Configuration for actor creation (behavior, mailbox,
  codec, delivery store). `MailboxSize` is the channel capacity;
  `MailboxOverflowLimit` is the hard cap on the overflow queue behind it
  (zero selects `DefaultMailboxOverflowLimit`).
- `ActorRef[M, R]` — Typed reference for sending messages to an actor (`Tell`, `TryTell`, `Ask`).
- `TellOnlyRef[M]` — Fire-and-forget reference (no response type). `Tell` blocks for mailbox room unless made with an active turn context (then it overflows instead, or fails with `ErrMailboxOverflow` at the cap), `TryTell` never blocks.
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
- `DeliveryStore` / `TxAwareDeliveryStore` — Interfaces for durable mailbox persistence (enqueue, claim, ack, dead-letter). The leaseless single-worker fast path adds `PeekNextMessage` (read-only claim, no lease, no attempts bump; yields an empty lease token), `AckMessageByID` (unfenced delete), and `NackMessageByID` (unfenced release that increments attempts). Postpone adds the same fenced/unfenced pair: `PostponeMessage` (lease-fenced release that decrements attempts to compensate the lease-time bump) and `PostponeMessageByID` (unfenced release that leaves attempts untouched, since the peek never bumped them). A `DurableActor` enables it (via `DurableMailboxConfig.SingleWorkerLeaseless`) strictly when `NumWorkers == 1` AND the behavior is the Read/Commit (Right/`TxBehavior`) path, eliminating the per-message lease write transaction. The multi-worker pool and the classic path are byte-for-byte unchanged: they keep `LeaseNextMessage` and the lease-fenced ack. Ack/nack route to the by-ID ops automatically whenever the delivery's lease token is empty; `Delivery.ShouldDeadLetter` counts the in-flight attempt as `Attempts + 1` on the leaseless path so the dead-letter boundary matches the leased path (where attempts is pre-incremented at lease).
- `DurableActor` — Actor variant with crash-safe mailbox backed by SQL persistence. Provides `Wait(ctx)` to block until the actor stops and `StopAndWait(ctx)` to request a graceful shutdown and then wait.
- `DurableActorConfig[M, R]` — Configuration struct for `DurableActor`: behavior, store, codec, clock, DLO, WaitGroup, `TellRetryPolicy`, lease/heartbeat/poll durations, max attempts, cleanup timeout, deduplication TTL, and `NumWorkers`.
- `DurableActorConfig.NumWorkers` — How many concurrent worker loops drain the actor's single mailbox. Default and any value `<= 1` is one worker (strictly-sequential processing). A value `> 1` turns the actor into a competing-consumer pool: that many goroutines each lease distinct messages via `LeaseNextMailboxMessage`, so independent messages run in parallel while per-correlation-key FIFO still keeps same-key messages ordered. Only for behaviors whose handlers are concurrency-safe and hold no writer across their side effects (e.g. the serverconn egress sender on the Read/Commit path). `NewDurableActor` **fails closed** with `ErrConcurrentClassicBehavior` when `NumWorkers > 1` is paired with a classic (`Left`) `ActorBehavior`, since the classic path wraps the whole `Receive` in one write transaction and assumes sequential delivery; pools are only valid on the Read/Commit (`TxBehavior`) path. The test-only `DurableActorConfig.AllowConcurrentClassicBehavior()` escape hatch bypasses the guard for the egress benchmark that measures the forbidden config; production code must never call it.
- `DefaultDurableActorConfig[M, R]()` — Constructor returning a `DurableActorConfig` with safe defaults (30s lease, 10 max attempts, 1s poll floor / 30s poll ceiling, DefaultTellRetryPolicy).
- `DurableActorConfig.PollInterval` / `MaxPollInterval` — Floor and ceiling of the idle mailbox poll backoff (defaults 1s / 30s). The fallback poll is NOT the delivery path: a same-process enqueue signals the mailbox's wake channel, and the store's post-commit `RegisterMailboxWake` callback rouses the exact mailbox a committed transaction enqueued into, so delivery latency is unaffected by how far the backoff has decayed. Each consecutive empty poll roughly doubles the wait from `PollInterval` up to `MaxPollInterval`; any wake or successfully claimed message snaps it back to the floor. This matters at scale because on a Postgres-backed store every empty poll is a full SERIALIZABLE write transaction that updates no rows, so thousands of resident-but-idle actors polling at a fixed 1Hz become a pure transaction tax. The timer is never stopped: since `RegisterMailboxWake` is same-process only, the poll remains the sole discovery mechanism for a row enqueued by another process or replica, which makes `MaxPollInterval` the worst-case cross-process/cross-replica delivery latency. A zero value normalizes to the default and a ceiling below the floor is raised to the floor (constant cadence, never a shrinking wait).
- `Postpone(delay) error` / `PostponeError` / `ErrPostponed` — The
  attempt-preserving alternative to a nack, for a behavior that cannot handle
  a message *yet* (a capacity cap, a peer still draining) as opposed to one
  that failed. Returning `actor.Postpone(delay)` from a Tell turn releases the
  message for redelivery after `delay` with `attempts` unchanged and without
  marking it processed. Detection matches anywhere in the wrap chain, so a
  behavior may annotate it (`fmt.Errorf("%w: %w", errCapped,
  actor.Postpone(d))`). `ErrPostponed` is the `errors.Is` sentinel;
  `*PostponeError.Delay` carries the backoff. Pass a real delay: zero or
  negative makes the message immediately claim-eligible, which against an
  unchanged condition is a busy loop against the database.
- `DeliveryEnqueuedAt(ctx) (time.Time, bool)` — When the message currently
  being processed was first persisted, read from the durable row's `created_at`
  and stamped onto the processing context by the consume path (once, above the
  fork into the three execution paths, so all of them agree). Neither a nack
  nor a postpone rewrites that column, so it survives every redelivery. This is
  the intended horizon reference for a postponing behavior, and the reason it
  is row-derived rather than behavior-derived: per-message state keyed on a
  sender-chosen id is unbounded when the message stream is attacker-controlled.
  The bool is false outside a delivery and for a store that reports no
  timestamp. `Delivery.EnqueuedAt` is the same value on the delivery itself;
  `WithDeliveryEnqueuedAtForTest` stamps a context for cross-package tests.
- `TellRetryPolicy` — Function type `func(lastErr error, attempts int) (bool,
  time.Duration)` determining retry behavior for failed Tell messages. Return
  `(false, _)` to dead-letter immediately. A policy may stop retries early,
  but cannot extend the durable `MaxAttempts` ceiling. A postpone is detected
  *before* this policy is consulted and never reaches it.
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
- `Probe[M, R](ctx, ref) (uint64, error)` (`probe.go`) — Health check for a
  local channel-mailbox actor's **receive loop**, not its behavior. It enqueues
  an internal, side-effect-free envelope that the loop acknowledges and that
  never reaches the behavior or the dead-letter office, and returns the actor's
  completed-turn counter (which counts probes too). Admission uses `TrySend`,
  so a probe never waits for mailbox room; only the response wait respects
  `ctx` and actor shutdown. Requires the direct `ActorRef` returned by
  `NewActor`/`RegisterWithSystem` — routers, mapped refs, and durable actors
  return an error.
- `AskThen[M, R, S](ctx, ref, msg, self, timeout, wrap)` — Pipe-to-self
  helper for a behavior that needs another actor's reply. It sends `msg` as an
  Ask bounded by a required `timeout`, returns at once, and delivers
  `wrap(result)` to `self` as an ordinary message handled in a later turn. The
  result is a value, the callee's error, a send error, or
  `context.DeadlineExceeded`; a non-positive timeout yields the deadline error
  without sending. The Ask keeps ctx's values but not its cancellation, so it
  is bounded only by `timeout` and outlives the turn. What the callee sees of
  the caller's transaction depends on its mailbox: a durable callee enqueues
  synchronously, before `AskThen` returns, so that enqueue joins the caller's
  transaction, but the envelope's caller context has the transaction stripped,
  so no callee ever runs its turn inside it, and a channel-mailbox callee
  (which runs after the caller's turn has ended) never sees it at all. The
  reply is delivered with a context stripped of the transaction and
  cancellation. Delivery to `self` is at most once and best effort: it is
  dropped at debug level if `self` has stopped, and logged at warning level
  when a live `self` fails to take it (a durable `self` can fail to encode or
  enqueue). `wrap`
  must be pure: it runs on a helper goroutine and must not touch actor state.
- `ErrAwaitInTurn` / `AwaitInTurnPolicy` / `SetAwaitInTurnPolicy(p)` —
  Process-wide policy for `Future.Await` on an incomplete future from inside a
  receive turn. `AwaitInTurnWarn` (default) logs at info once per call site
  (keyed on the caller's PC) and waits as before; `AwaitInTurnAllow` does
  nothing; `AwaitInTurnError` returns `ErrAwaitInTurn` without waiting. A
  completed future always returns its value, and a context whose turn has
  ended is unaffected. `ThenApply` and `OnComplete` await on helper goroutines
  through the unexported `awaitInternal`, and `MapRef.Ask` goes through the
  `awaitFuture` helper, which uses `awaitInternal` for this package's own
  futures and falls back to the public `Await` for any foreign `Future`
  implementation, so framework helpers never trip the guard.
- `ErrWaitCycle` / `WaitCycle` / `SetWaitCycleHook(hook)` — Runtime backstop
  for a cycle of turns that each `Await` another actor's reply (including an
  actor awaiting itself). Such an `Await` returns an error wrapping
  `ErrWaitCycle` with the path (`a -> b -> a`) instead of parking, logs at
  error level with the call site, and calls the optional hook with a
  `WaitCycle{Path, CallSite}`. The hook is the intended oracle for simulation
  tests (fail the run on any call); a nil hook clears it, and it must not
  block.
- `ChannelMailbox[M, R]` — In-memory channel-based mailbox (non-durable, for lightweight actors). A bounded channel fronted by an overflow queue: `Send` with an active turn context (see `turn.go`) never blocks and spills into the overflow (or returns `ErrMailboxOverflow` at the hard cap), which the receive loop refills into the channel after every receive (so a non-empty overflow implies a non-empty channel). Lock order is `mu` (read side) then `overflowMu`. `NewChannelMailbox` takes `WithOverflowLimit` and `WithMailboxID` options; `OverflowLen()` reports the backlog. After `Close`, `Drain` yields the channel envelopes first and the overflow second, which is send order because the overflow always trails the channel.
- `Mailbox[M, R]` — Interface for actor message queues: `Send(ctx, env) error` (blocking, except for a `ChannelMailbox` send made with an active turn context; returns `ErrMailboxClosed`, `ErrActorTerminated`, or a context error on failure), `TrySend(env) error` (non-blocking), `Receive(ctx) iter.Seq[envelope]`, `Close()`, `IsClosed() bool`, `Drain() iter.Seq[envelope]`.
- `DefaultMailboxOverflowLimit` — 100000. The overflow cap a `ChannelMailbox`
  falls back to when `ActorConfig.MailboxOverflowLimit` / `WithOverflowLimit`
  is zero or negative.
- `TurnActor(ctx) (string, bool)` — ID of the actor whose receive turn `ctx`
  belongs to. Both `Actor.process` and `DurableActor.processDelivery` stamp
  this marker on the context they hand the behavior and clear it when the
  behavior returns, so the bool is false outside a turn and for a context that
  outlived the turn it was created in (a goroutine the behavior spawned is not
  the actor's own goroutine and may park like any other producer). This is the
  marker `ChannelMailbox.Send` keys its non-parking path off.
- `WithTurnForTest(ctx, actorID) (context.Context, func())` — Stamps the same
  turn marker from outside the package, as the runtime does before invoking a
  behavior, and returns the function that ends the turn. For tests that
  exercise turn-sensitive paths without standing up an actor.
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

- **A probe answers "is the receive loop turning", and cannot itself become
  work.** Concurrent `Probe` callers share at most one queued probe — even
  after a caller times out — so a stalled actor can never accumulate
  health-check envelopes behind the message that wedged it. A completed probe
  is replaced on the next call rather than reused as a stale success, and
  shutdown wakes waiters through the actor context rather than by closing the
  probe channel (the drain loop skips probe envelopes), so teardown is never
  reported as a healthy turn. Supervisors should watch the returned
  completed-turn counter for liveness: it advances even when the probe itself
  fails, which is what distinguishes a busy queue or a late acknowledgement
  from a genuinely stuck loop.
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
- Outbox messages are dispatched only after state is persisted (outbox pattern).
- **Outbox fold p-model.** For tx-aware stores, outbox delivery is
  `claim -> (target mailbox enqueue + CompleteOutbox) in one write tx`. If the
  transaction fails before commit, both the enqueue and completion roll back and
  the claim expiry is the retry mechanism; the publisher must log the
  transaction failure even when the inner Tell/Complete operations returned nil,
  because begin/commit failures happen outside those operation-level logs.
- `ServiceKey` lookup via `Receptionist` is type-safe: mismatched types return `ErrServiceKeyTypeMismatch`.
- `RestartMessage` has `RestartPriority` (MaxInt32) ensuring it is processed before all other messages on recovery.
- Transaction context (`WithTx`/`RequireTx`) enables same-DB-transaction joining between actors and their callers. `AskThen` is the deliberate exception on the receiving side: the durable enqueue still joins the caller's transaction, but the transaction is stripped from the envelope's caller context, because that context is the base of the callee's turn context and the caller's turn has committed or rolled back by the time the callee runs. The detached marker is cleared along with it, so the callee's own later `Ask` calls (which do wait for their replies) keep propagating their transaction normally.
- `Mailbox.Send` returns the exact failure error (`ErrMailboxClosed`, `ErrActorTerminated`, `context.Canceled`, `context.DeadlineExceeded`) rather than a boolean; `Tell` and `Ask` propagate this directly to callers.
- **A `Tell` or `Ask` made with the turn's context never parks on a full
  channel mailbox.** The runtime marks the context handed to a behavior as an
  active receive turn, and `ChannelMailbox.Send` given such a context never
  blocks: it uses the channel when there is room and otherwise appends to an
  overflow queue behind it, so an actor cycle cannot park on a full mailbox.
  Per-sender order is kept. A send made with any other context keeps the
  blocking backpressure: `context.Background()`, a context captured at
  construction, or a goroutine that outlives the turn (the marker goes inactive
  when the behavior returns). Always pass the turn context through; mixing it
  with a non-turn context for sends to the same target can reorder them, since
  a parked external sender is handed each freed slot ahead of the overflow.
  `TryTell` is unchanged and still returns `ErrMailboxFull` immediately, and also while
  the overflow is non-empty. Inside a turn its only use is for a caller that
  wants the `ErrMailboxFull` signal so it can drop the message instead of
  queueing it. The overflow has a hard cap
  (`ActorConfig.MailboxOverflowLimit`, default
  `DefaultMailboxOverflowLimit` = 100000). A send that would exceed it
  enqueues nothing and returns an error wrapping `ErrMailboxOverflow` (the
  mailbox ID is in the message), and the first rejection of an overflow
  episode is logged at critical severity, not one log per message. It does
  not panic: durable actors recover behavior panics, so a panic there was
  swallowed and a send made after the durable turn committed, such as arming
  a retry timer, was lost with no restart. A failed `Ask` send completes its
  promise with the error, and a rejected send leaves the queue untouched.
  `OverflowLen` reports the current backlog, and a warning is logged once per
  episode when it passes a tenth of the cap.
- **A turn should not `Await` another actor's reply.** `Future.Await` inside
  `Receive` parks the actor's whole mailbox until the callee answers. If the
  callee is, transitively, waiting on the caller, neither can make progress, and
  the in-turn send rule above does not help because the wait is on a reply, not
  a mailbox slot. Use `AskThen` when the caller has follow-up work to do with
  the reply (it arrives as a message, so all state access stays inside turns),
  or `DetachAskPromise` when the caller only forwards the reply to its own
  caller. Rollout is warn, then enforce: `AwaitInTurnWarn` is the default and
  logs each offending call site once at info level, so the sites can be
  enumerated and migrated without changing behavior; once none remain, a
  process can switch to `AwaitInTurnError`. Awaiting from outside a turn, from
  a goroutine that outlives the turn, or on an already-complete future is
  always fine. Awaits that live in this package's own helper goroutines go
  through `awaitInternal` (or `awaitFuture`) and must keep doing so.
- **An `AskThen` reply is delivered at most once, and may be stale or never
  arrive, so a pending entry needs its own expiry.** `wrap` is applied to one
  result, but delivering it to `self` is best effort: it is lost if `self` has
  stopped, and for a durable `self` also when the enqueue fails while `self`
  lives (logged at warning level). A behavior that records a pending request
  must expire that entry itself rather than rely on the reply or its error
  arriving. A timeout abandons the wait, not the request: the callee may still
  process the message, and its late reply is then discarded. A durable caller's
  turn may also roll back after `AskThen` returned, which rolls the durable Ask
  row back with it while the helper goroutine still delivers a wrapped
  `context.DeadlineExceeded`. A behavior must therefore record that a request
  is pending in the turn that issues it and treat a wrapped result it has no
  pending entry for as stale and ignore it, rather than assuming every
  delivered reply answers a live request.
- **Wait cycles fail fast, within a bounded scope.** `Ask` records the
  answering actor's ID on its future (`actorRefImpl`, `durableActorRefImpl`,
  and `MapRef` and `ThenApply`, which inherit the inner future's target;
  `Router` delegates to a concrete ref). In `Future.Await`, on the slow path and after the
  await-in-turn policy check, a serial actor's active turn registers an edge
  `actor -> target` in a process-wide map and walks the chain under the same
  lock hold. If the walk returns to the waiting actor the `Await` fails with
  `ErrWaitCycle` and registers nothing; otherwise the edge is removed when
  `Await` returns by any route. Because registration and walk are atomic, of
  two actors closing a cycle together exactly one fails, and its turn
  returning lets the other's Ask be processed. An edge counts only while its
  turn is active and its reply is undelivered, so a cycle is a real wait cycle
  (barring the helper-goroutine case below); one with a deadline
  would have unwound after every participant waited out its deadline, and
  failing fast is better. Sends add no edges since an in-turn send never
  parks. Not covered: non-serial turns (`NumWorkers > 1`, a parked worker does
  not park the actor), futures with no target, `awaitInternal` helper
  goroutines, and waits outside the framework (I/O, mutexes, raw channels).
  Two cases are invisible because the target is not the completer. A promise
  taken with `DetachAskPromise` keeps the coordinator as its target although
  another actor completes it, so the coordinator later awaiting the original
  caller can be reported as a cycle that does not exist, and a real cycle
  through the actual completer is missed. A wait that passes through a state
  machine's driver goroutine is also missed: protofsm `StateMachine.Receive`
  awaits an untargeted `AskEvent` future, and the driver may itself wait on
  other actors. An edge also stops counting once the reply it waits on has been
  delivered (a derived `ThenApply` or `MapRef` future checks the root actor
  future's completion, not its own), so an actor that already holds its reply
  but has not yet removed its edge never looks blocked. An actor can have
  several live edges, one per wait in flight, and a cycle through any of them is
  a cycle; each edge is removed only by the wait that registered it. A goroutine
  the behavior spawns with the live turn context registers an edge while that
  turn is active, and Go gives no goroutine identity, so such an `Await` is
  indistinguishable from the actor's own wait and can be reported as a cycle.
  The await-in-turn policy already flags those goroutines: they should use
  `AskThen` or an unguarded framework helper. An edge records its turn, so once
  the turn returns it counts as absent.
- **Durable senders are slowed only by the overflow cap error.** A durable
  turn never parks on a channel mailbox, so the one backpressure signal a
  durable sender gets from a saturated channel target is `ErrMailboxOverflow`
  at the cap. A behavior that returns it is nacked and redelivered, so a
  durable behavior that `Tell`s before it commits and then fails this way is
  retried and its `Tell`s run again. They must tolerate redelivery; the
  framework already provides at-least-once delivery, not exactly-once effects
  of those sends.
- During daemon teardown, the underlying DB is closed before every actor's lease loop has wound down. The lease loop uses `isExpectedShutdownErr` to demote these "database is closed" errors to debug level; real operational errors still surface as warnings because neither the actor context nor the outer context is done in those cases.
- **Postpone preserves the attempt budget; nack spends it.** A nack increments
  `attempts` on every release, and both the claim and the peek queries filter
  on `attempts < max_attempts`. A successful postpone leaves the budget exactly
  as it was: the fenced `PostponeMessage` decrements to compensate the
  lease-time increment, and the leaseless `PostponeMessageByID` leaves it
  untouched because the peek never bumped it. An "always retry"
  `TellRetryPolicy` is **not** a substitute. Both the non-tx path
  (`handleResult` via `Delivery.Nack`, including the Read/Commit `finishNonTx`
  tail) and the classic tx path (`handleResultInTx`) enforce
  `ShouldDeadLetter` before retry release. A policy can stop retries early or
  choose the delay, but it cannot extend `MaxAttempts`. Only a postpone can
  express an attempt-preserving wait.
- **Fenced postpone is attempt-neutral only while the consumer still owns the
  lease.** A classic tx turn whose fenced postpone matches zero rows has lost
  its lease, so the actor rolls the transaction back rather than letting the
  stale consumer ack or dead-letter another owner's row. That rollback also
  discards the compensating decrement, while the earlier lease claim's
  increment remains committed. The lost turn therefore spends one attempt.
  Repeated cross-process lease loss can exhaust the row without a dead letter.
  No current classic-path behavior returns a postpone. Close this gap before
  adopting postpone on that path where competing runtime owners are possible.
- **Postpone is Tell-only.** An Ask has a caller parked on the promise, so
  postponing it would strand that caller for the length of the delay with
  nothing to observe. An Ask behavior returning a `PostponeError` gets ordinary
  error treatment (the promise completes with it) and the caller decides
  whether to re-issue. A behavior serving the same condition over both a routed
  Tell and an RPC Ask should postpone on the Tell path only.
- **A postponed message never auto-dead-letters, so behaviors bound their own
  horizon.** This is the deliberate cost of the feature: postpone removes the
  only mechanism that would eventually give up. A behavior that postpones
  against a condition that never clears postpones forever. The framework cannot
  bound this, because only the behavior knows when waiting stops making sense.
  Track the wait and return a real error once it is no longer justified, so the
  normal nack path can dead-letter it. Use `DeliveryEnqueuedAt(ctx) (time.Time,
  bool)` for that: it reports the durable row's `created_at`, which neither a
  nack nor a postpone rewrites, so it measures the true age of the wait across
  every redelivery. Prefer it over behavior-side state whenever the message
  stream is attacker-controlled, since a map keyed on a sender-chosen id is
  itself unbounded. The bool distinguishes "no timestamp" from "age zero";
  treat absence as no horizon information.
- **A postponed head still blocks its correlation-key lane.** Postpone does not
  exempt a message from per-key FIFO: the row is still in the mailbox, so no
  later same-key message is claim-eligible until it drains, and every further
  postpone extends the block. Blocking is bounded to the key, not the mailbox,
  but a long backoff on a busy key is a throughput decision, not just a retry
  decision. Unkeyed messages have no such interaction. **Scope:** per-key FIFO
  holds for a postponing consumer only when the actor is single-worker OR the
  lane's messages never reach their final attempt. A predecessor leased on its
  final attempt is invisible to the claim anti-join (`m2.attempts <
  m2.max_attempts`), so a pool worker can claim its successor; a postpone then
  decrements the predecessor back below the cap and it reprocesses after the
  successor, inverting order. No adopter combines keyed lanes with
  `NumWorkers > 1` and postpone today, so the SQL is left alone; adding a
  lease-liveness disjunct to the anti-join is the prerequisite for that
  combination.
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
