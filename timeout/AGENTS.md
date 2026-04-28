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
