# timeout

## Purpose

Generic fire-and-forget timeout scheduling actor. Schedules one-shot
timeouts and recurring ticks, delivering `ExpiredMsg` /
`TickFiredMsg` to a caller-supplied callback ref when they fire.

## Key Types

- `Actor` — Holds `oneshots`/`recurring` entry maps; all state mutation
  happens single-threadedly inside `Receive`. Clock callbacks never
  touch actor state directly — they self-signal an internal fire message
  (`internalTimerFired`/`internalTickFired`) carrying a generation
  token, so stale fires racing a Cancel/reschedule are dropped.
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
  freeze every timer in the daemon. Delivery uses `TryTell`; a full
  mailbox keeps the entry (the only copy of the reminder) and re-arms
  the same fire on a backoff doubling from 1s to 30s. Callers must
  therefore tolerate a late, possibly reordered callback. A callback
  that is terminated or closed gets no retry chain and its entry is
  released.
- **Clock goroutines never block on the actor's own mailbox.** A fire
  that finds no room re-arms itself after `selfSignalRetry` instead of
  parking, so at most one fire is outstanding per entry no matter how
  far behind the actor runs.
- Callers must call `Start(ref)` with the actor's own registered ref
  before any request is delivered; calling `Receive` directly without
  a mailbox in front breaks the self-tell model.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map
