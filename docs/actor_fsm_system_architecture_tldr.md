# Actor/FSM System Architecture TL;DR

Quick reference guide for building restart-safe, idempotent flows on top of
`baselib/protofsm` and `baselib/actor`. For comprehensive documentation, see
`actor_fsm_sytem_architecture_cookbook.md`.

## What Exists in `baselib/`

- **`protofsm.StateMachine`**: Pure event→transition engine; emits internal
  events and outbox events.
- **`protofsm.ActorStateMachine`**: Wraps a state machine as an actor behavior;
  dispatches outbox events into the `ActorSystem`.
- **`actor.ActorSystem`**: Owns mailboxes, routers, service keys, and
  lifecycle.
- **Environment**: Passed into each state; can optionally receive the FSM's own
  actor ref (tell-only or ask) for self callbacks.

## Layered Responsibilities

- **FSM state**: Deterministic, pure; no I/O. Consumes event, mutates in-memory
  state, emits outbox commands plus optional internal events.
- **Outbox event**: Describes a side effect. Knows service key and message for
  an actor; wrapper dispatches it (tell/ask). Never performs work itself.
- **Environment**: Provides cheap, repeatable helpers (time, config, pure
  calculations, small caches). Can expose FSM's own tell ref for callbacks.
  Avoid foreign actor refs; let actors handle cross-component routing via
  outbox.
- **Wrapping actor**: Owns FSM instance, persistence hooks, and interaction with
  other actors. Receives external messages, forwards to FSM, persists, and
  dispatches outbox events.
- **Manager/supervisor**: Bootstraps `ActorSystem`, spawns FSM actors, restarts
  from durable state, and prunes terminal actors.

## Idempotency and Determinism Rules

- Same input history from init must produce same state. Keep FSM logic pure;
  inject nondeterminism (time, randomness, external queries) via env functions
  that are repeatable or memoized per event.
- Every outbox message must be safe to deliver at least once. Include
  idempotency key (workflow ID + event cursor) and design target actors to
  dedup.
- **Persist-before-notify**: Commit state + emitted events before dispatching
  so crash cannot announce work that isn't durable.
- **Resume flow**: On restart, reload state + unflushed outbox, spawn FSM actor
  with that state, then drive `EventResume` that replays pending outbox
  messages (they must stay idempotent).
- **Re-run safety**: States must handle receiving same external event again
  without progressing incorrectly; internal events should be safe to enqueue
  multiple times.

## Outbox vs Env vs Direct Actor Calls

- Use **env calls** for cheap, local, repeatable work with no externally
  visible side effects (math, formatting, cached lookups). Prefer this for
  synchronous reads that can repeat on retry.
- Use **outbox events** for side effects (network, disk, other actors,
  irreversible updates). Treat outbox as commands for wrapper/dispatcher to
  send; FSM should not hold foreign actor refs.
- Only **wrapper actors/managers** talk to other actors directly. They convert
  outbox events into `actor.Router` interactions, handle asks/tells, and own
  error handling/backpressure.

## Durability Model (Push and Pub/Sub Friendly)

- Store transitions as append-only event log with cursor. Each processed
  external event yields: new state snapshot, event cursor, and outbox queue.
- Persist `(cursor, state name, state data, outbox entries)` in one
  transaction. On commit success, enqueue outbox to `ActorSystem`.
- For **push (actor-to-actor)**: Outbox entries target service keys. Actors
  remain idempotent; if dispatch fails, keep entry and retry with backoff.
- For **pub/sub (event sourcing)**: Also project state changes into event
  stream table. Consumers track their own cursors; replays are safe because
  payloads are deterministic and dedup keys are present.
- Acknowledge consumption by advancing cursor only after successful transition
  and durable write. Never advance cursors on failed transitions.

## Patterns to Copy

- **Resume pattern**: Boot actor with stored state, emit `EventResume` that
  replays pending outbox events (idempotent) and re-establishes monitors.
- **Ask when needed**: Prefer tell for fire-and-forget, ask only when FSM must
  block on result. Use `RoutedOutboxEvent` ask mode; unwrap errors and map them
  back into FSM error handling.
- **Delta persistence**: Store only changed fields plus current state name to
  reduce conflicts; always write event cursor and state together.
- **Hierarchical scope**: Each FSM/actor knows only layer below. Broader
  orchestration lives in higher-level actor that dispatches without exposing
  system-wide refs to leaf FSMs.

## Quick Build Checklist

- Define sealed types: events, outbox events, states. Keep `ProcessEvent`
  deterministic and side-effect free beyond emitted outbox events.
- Specify env struct with only repeatable helpers and (optionally) FSM's own
  tell ref. Avoid embedding other actor refs.
- In transitions, compute next state, append internal events, and emit outbox
  commands; do not perform I/O inline.
- Wrap FSM with `protofsm.NewSystemsActorStateMachine`; keep manager that
  tracks refs, handles resume, and cleans up terminal actors.
- Add persistence hook (DB/outbox store) in wrapper/manager: write state +
  outbox in one tx, then dispatch.
- Make every outbox command idempotent and include id key. Design receiving
  actors to be safe on repeats.
- Plan for at-least-once delivery and replays. Provide `EventResume` events
  that re-emit pending outbox work.
- Log with structured `*S` calls, and keep lines within 80-char guideline when
  feasible.

## Decision Quick Reference

**Env vs Outbox vs Direct Call:**
- Cheap + repeatable + no side effects → env call
- External I/O or state changes → outbox event
- Manager coordinating FSMs → direct actor call

**Tell vs Ask:**
- Fire-and-forget → Tell
- Need response to proceed → Ask
- Result arrives as separate event later → Tell + callback

**Push vs Pub/Sub:**
- Known consumers at write time → Push (actor-to-actor)
- Unknown/multiple consumers → Pub/Sub (event sourcing)
- Best of both → Hybrid (push for critical, pub/sub for audit/analytics)

---

**See also:**
- `actor_fsm_sytem_architecture_cookbook.md` - Comprehensive guide with examples
- `baselib/PROTOFSM_ACTOR_GUIDE.md` - Step-by-step usage guide
- `baselib/example/` - Complete document approval workflow example
