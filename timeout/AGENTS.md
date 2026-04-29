# timeout

## Purpose

Generic fire-and-forget timeout scheduling actor. Sends `ExpiredMsg` to a
callback when a one-shot timeout fires, and `TickFiredMsg` on each
recurring tick scheduled via `ScheduleRecurringTickRequest`.

## Architecture

The actor follows a strict self-tell model: clock callbacks never mutate
actor state directly. Each `Clock.AfterFunc` callback Tells an internal
fire message (`internalTimerFired` / `internalTickFired`) into the
actor's own mailbox via the self-ref attached at `Start`. All state
mutation — adding/removing entries from the `oneshots` and `recurring`
maps, delivering user-facing messages, and re-arming recurring chains —
happens single-threadedly inside `Receive`. There is no internal mutex
and no per-entry forwarder goroutine.

Recurring ticks are implemented as a chain of one-shot timers that
re-arm from inside `handleTickFired`, which gives "fixed-delay"
semantics (next fire = handler-finish + interval) rather than
`time.Ticker`'s "fixed-rate with drops". Stale fires that race with a
Cancel or reschedule are filtered by a per-entry generation token.

## Clock Abstraction

- `Clock` — Interface (`Now() time.Time`, `AfterFunc(d, f) Stoppable`)
  that drives all timer creation. Allows deterministic time injection in
  tests.
- `Stoppable` — Interface satisfied directly by `*time.Timer`; returned
  by `Clock.AfterFunc`.
- `RealClock` — Production implementation backed by the standard library.
- `NewActor()` — Constructor using `RealClock`.
- `NewActorWithClock(clock Clock)` — Test constructor; inject a fake
  clock for deterministic timer behavior without wall-clock delays.

## Transform Helpers

- `MapTimeoutExpired[Out](targetRef, mapFn)` — Wraps a target ref to
  convert `*ExpiredMsg` deliveries into the caller's message type using
  `actor.NewMapInputRef`. Eliminates boilerplate adapter types when wiring
  one-shot timeouts to domain actors.
- `MapTickFired[Out](targetRef, mapFn)` — Same pattern for `*TickFiredMsg`.
  Both helpers are the idiomatic way to wire the timeout actor to a domain
  actor.

## Message Types

- `ScheduleTimeoutRequest` — Schedule a one-shot timer for `Duration`.
  Fires `*ExpiredMsg` to `Target` ref.
- `ScheduleRecurringTickRequest` — Schedule a recurring tick. `Interval`
  must be strictly positive (zero/negative is rejected before touching
  state). Fires `*TickFiredMsg` on each tick.
- `CancelTimeoutRequest` — Cancel a scheduled timeout or recurring tick by
  ID. One-shot and recurring timers share the same ID namespace; either
  type can be cancelled with this message.
- `ExpiredMsg` — Delivered to the target ref when a one-shot timer fires.
- `TickFiredMsg` — Delivered on each recurring tick. `FiredAt` carries the
  clock-goroutine capture time, not the Receive-processing time; important
  for test assertions.

## Wiring

Callers must call `Start(ref)` after registering the actor with the
actor system; `ref` is the `actor.ActorRef` (or any
`actor.TellOnlyRef[Msg]`) returned by `RegisterWithSystem` / `Spawn`.
External callers must Tell into the actor through that same ref —
calling `Receive` directly is unsafe under the self-tell model because
clock callbacks expect a serializing mailbox in front of `Receive`.

## Relationships

- **Depends on**: `baselib/actor` (actor framework).
- **Depended on by**: `round` (forfeit collection timeouts, registration
  timeouts), `oor` (retry timers via `SigningOutboxHandler.TimeoutActor`).

## Invariants

- One-shot and recurring timers share the same ID namespace. Scheduling
  either type with an existing ID cancels the prior entry, regardless of
  type.
- `ScheduleRecurringTickRequest.Interval` must be strictly positive;
  zero or negative is rejected before touching actor state.
- Every `Receive` path returns `fn.Ok[Resp](&AckResponse{Success: true})`;
  the only error return is for invalid `Interval` on
  `ScheduleRecurringTickRequest`.
