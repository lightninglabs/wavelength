# timeout

## Purpose

Generic fire-and-forget timeout scheduling actor. Schedules one-shot
timeouts and recurring ticks, delivering `ExpiredMsg` /
`TickFiredMsg` to a caller-supplied callback ref when they fire.

## Key Types

- `Actor` — Holds `oneshots`/`recurring` entry maps; all state mutation
  happens single-threadedly inside `Receive`. Clock callbacks never
  touch actor state directly. They self-signal an internal fire message
  (`internalTimerFired`/`internalTickFired`) carrying a generation
  token, so stale fires racing a Cancel/reschedule are dropped.
- `Config` / `NewActorWithConfig` — Optional clock and logger. Daemons
  use this constructor; `NewActor` and `NewActorWithClock` are the
  logger-less shorthands for tests.
- `Clock` — Interface (`Now`, `AfterFunc`) abstracting the time source;
  `RealClock` is the production impl, tests inject a fake via
  `NewActorWithClock`.
- `ScheduleTimeoutRequest` / `ScheduleRecurringTickRequest` /
  `CancelTimeoutRequest` — Msg variants schedule/cancel a timer;
  one-shot and recurring share the same `ID` namespace.
- `MapTimeoutExpired` / `MapTickFired` — Wrap a target ref to convert
  `ExpiredMsg`/`TickFiredMsg` into a domain actor's own message type.

## Relationships

- **Depends on**: `baselib/actor` (actor framework).
- **Depended on by**: `round` (forfeit/registration timeouts), `oor`
  (retry timers via `SigningOutboxHandler`), `credit` (retry
  callbacks).
- **Messages to/from**: Receives `ScheduleTimeoutRequest` /
  `ScheduleRecurringTickRequest` / `CancelTimeoutRequest` from any
  actor; sends `ExpiredMsg` / `TickFiredMsg` back to the `Callback`
  ref supplied in the request.

## Invariants

- One-shot and recurring timers share the same ID namespace;
  scheduling either type with an existing ID cancels the prior entry
  regardless of type.
- `ScheduleRecurringTickRequest.Interval` must be strictly positive;
  zero/negative is rejected before touching state (an immediate
  re-arming loop would starve the mailbox).
- Recurring ticks are "fixed-delay" (next fire = handler-finish +
  interval), not `time.Ticker`'s fixed-rate-with-drops.
- **The receive goroutine never blocks on a callback.** This actor is a
  process-wide singleton, so one backlogged requester parking it would
  freeze every timer in the daemon. Delivery uses `TryTell`; a failed
  delivery keeps the entry (the only copy of the reminder) and re-arms
  the same fire on a backoff that doubles from 1s and flattens out at
  30s. A recurring entry whose interval is shorter than that 1s base
  starts the doubling from its own interval instead, so one hiccup does
  not stretch a 100ms ticker to a full second. Only the starting point
  moves: the 30s ceiling applies to recurring entries too, so a consumer
  that stays unreachable does back off well past its own cadence. That
  is deliberate, since every attempt against a durable callback is a
  bounded database write, and retrying a 100ms ticker at 100ms forever
  would hammer the store it is already struggling with. Callers must
  therefore tolerate a late, possibly reordered callback.
- **This invariant is load-bearing for every scheduler.** `round`
  (`actor.go`), `oor` (`session_actor.go`) and `credit` (`op_actor.go`)
  all issue a *blocking* `Tell` into this actor's mailbox from inside
  their own receive loops. That is safe only because this actor's
  `Receive` never blocks, so its mailbox always drains. Add a blocking
  call anywhere inside `Receive` and the original incident shape is back:
  scheduler parked on the timeout mailbox, timeout actor parked on a
  scheduler.
- **Only `ErrActorTerminated` and `ErrMailboxClosed` are terminal.**
  Everything else is retried. Durable callbacks never report a full
  mailbox; a slow database surfaces as a wrapped `DeadlineExceeded`, and
  a `Router` target reports `ErrNoActorsAvailable` while its actor is
  being replaced. Treating either as permanent silently discards timers
  that nothing re-derives.
- **Clock goroutines never block on the actor's own mailbox.** A fire
  that finds no room re-arms itself after `selfSignalRetry` instead of
  parking, so at most one fire is outstanding per entry no matter how
  far behind the actor runs.
- **Daemons must inject a logger** via `NewActorWithConfig`. An actor's
  lifecycle context descends from `context.Background()`, so a logger
  resolved from the receive context alone is always `btclog.Disabled`
  and every diagnostic this actor writes is invisible in production.
- Callers must call `Start(ref)` with the actor's own registered ref
  before any request is delivered; calling `Receive` directly without
  a mailbox in front breaks the self-tell model.

## Gotchas

- **Prefer a dedicated instance over the shared singleton for durable
  callbacks.** `waved/server.go` registers a shared `timeout` actor,
  while OOR and credit each register their own. A durable callback's
  delivery is a database write, so handing one to the shared instance
  puts every other subsystem's timer behind that write's latency, and
  behind its retry backoff when the database is slow.
- **Recurring ticks carry the original `FiredAt` across a retry.** A
  tick delayed by backoff reports when its timer fired, not when
  delivery succeeded. A consumer computing `deadline = FiredAt +
  interval` can therefore conclude the deadline already passed the
  moment it receives a retried tick. No in-repo consumer does this
  (wavelength has no recurring-tick caller), but lumos does use the
  recurring scheduler, so its consumers need checking before the pin
  bumps.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map
