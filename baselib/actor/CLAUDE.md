# baselib/actor

## Purpose

Core actor framework providing typed, message-driven in-memory concurrent
components with service discovery via `Receptionist`.

## Key Types

- `Actor[M, R]` — Generic actor with typed message `M` and response `R`. Processes messages sequentially from its mailbox.
- `ActorBehavior[M, R]` — Interface that actors implement: `Start`, `Receive`, `Stop`.
- `ActorConfig[M, R]` — Configuration for actor creation (behavior, mailbox size, wait group, logger, and dead-letter outlet).
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
- `TellRetryPolicy` — Function type `func(attempts int, lastErr error) (bool, time.Duration)` determining retry behavior for failed Tell messages. Return `(false, _)` to dead-letter immediately.
- `DefaultTellRetryPolicy` — Exponential backoff policy: up to 5 attempts, starting at 1s, capped at 60s.
- `Promise[T]` / `Future[T]` — Async result types for Ask-pattern responses.
- `ChannelMailbox[M, R]` — In-memory channel-based mailbox (non-durable, for lightweight actors).
- `Mailbox[M, R]` — Interface for actor message queues: `Send(ctx, env) error` (blocking, returns `ErrMailboxClosed` or `ErrActorTerminated` on failure), `TrySend(env) error` (non-blocking), `Receive(ctx) iter.Seq[envelope]`, `Close()`, `IsClosed() bool`, `Drain() iter.Seq[envelope]`. `Send` previously returned `bool`; it now propagates the exact failure cause so callers can distinguish `ErrMailboxClosed`, `ErrActorTerminated`, and `context.Canceled`/`context.DeadlineExceeded` without inspecting actor state.
- `isExpectedShutdownErr(err) bool` — Internal helper that classifies errors as expected during teardown: context cancellation/deadline, closed DB handle ("sql: database is closed", "sql: connection is already closed", "use of closed network connection"). Used by the lease loop to demote shutdown-path failures to debug instead of warn-flooding test artifacts at itest tail.

## Relationships

- **Depends on**: Go channels and context primitives.
- **Depended on by**: All domain actors (`round`, `vtxo`, `oor`, `wallet`, `serverconn`, `timeout`, `indexer`) and `baselib/protofsm` (FSM-to-actor bridge).

## Invariants

- Messages are processed sequentially per actor — no concurrent `Receive` calls.
- `Tell` and `Ask` enqueue in memory; restart safety belongs to domain SQL tables and transport stores.
- Outbox messages are dispatched by domain-owned SQL outboxes, not by actor message serialization.
- `ServiceKey` lookup via `Receptionist` is type-safe: mismatched types return `ErrServiceKeyTypeMismatch`.
- Transaction context (`WithTx`/`RequireTx`) enables same-DB-transaction joining between actors and their callers.
- `Mailbox.Send` returns the exact failure error (`ErrMailboxClosed`, `ErrActorTerminated`, `context.Canceled`, `context.DeadlineExceeded`) rather than a boolean; `Tell` and `Ask` propagate this directly to callers.
- During daemon teardown, actors stop by closing their in-memory mailboxes and
  waiting for behavior shutdown.

## Deep Docs

- [baselib/CLAUDE.md](../CLAUDE.md) — Parent baselib package overview.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
