# baselib/example

## Purpose

Runnable examples demonstrating how to integrate `baselib/actor` and
`baselib/protofsm` into a complete actor-plus-state-machine workflow. Intended
as a learning reference, not production code.

## Key Types

- `DocEvent` — Sealed interface for all FSM events in the example workflow
  (`EventSubmitDocument`, `EventReviewStarted`, `EventApproved`,
  `EventRejected`, `EventResume`).
- `DocOutboxEvent` — Sealed interface for actor-routed outbox events
  (`OutboxRequestReview`, `OutboxNotify`). Implements
  `protofsm.ActorOutboxEvent`.
- `DocState` — Sealed interface for FSM states (`StateInit`,
  `StateAwaitingReview`, `StateApproved`, `StateRejected`).
- `DocEnvironment` — FSM execution context; holds the actor self-reference and
  implements `protofsm.TellRefEnv[DocEvent]`.
- `ReviewServiceBehavior` / `NotificationServiceBehavior` — Example actor
  behaviors registered under `ReviewServiceKey` and `NotifyServiceKey`.

## Relationships

- **Depends on**: `baselib/actor` (ActorSystem, ServiceKey, ActorBehavior),
  `baselib/protofsm` (StateMachine, State, RoutedOutboxEvent,
  NewSystemsActorStateMachine).
- **Depended on by**: nothing (example/test code only).

## Invariants

- Package is `baselib_test` — compiled only in test mode, never linked into
  production binaries.
- Demonstrates the canonical pattern: FSM events are sealed interfaces;
  outbox events implement `protofsm.ActorOutboxEvent` and carry typed
  `RoutedOutboxEvent[M, R]` values; `DocEnvironment` provides the actor
  self-reference so states can reply into the FSM without import cycles.

## Deep Docs

- [baselib/CLAUDE.md](../CLAUDE.md) — Parent baselib overview.
- [baselib/actor/CLAUDE.md](../actor/CLAUDE.md) — Actor framework internals.
- [baselib/protofsm/CLAUDE.md](../protofsm/CLAUDE.md) — FSM engine internals.
- [docs/durable_actor_quickstart.md](../../docs/durable_actor_quickstart.md) — Developer quickstart.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
