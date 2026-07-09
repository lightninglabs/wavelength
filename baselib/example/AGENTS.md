# baselib/example

## Purpose

Runnable example demonstrating how to wire a `baselib/protofsm` state machine
with `baselib/actor` services into an end-to-end async workflow. Intended as a
reference and learning aid; not used by production code.

## Key Types

- `DocEvent` — Sealed event interface for the document-approval FSM:
  `EventSubmitDocument`, `EventReviewStarted`, `EventApproved`,
  `EventRejected`, `EventResume`.
- `DocOutboxEvent` — Sealed outbox event interface routing IO to actor
  services: `OutboxRequestReview` (→ `ReviewService`), `OutboxNotify` (→
  `NotificationService`).
- `DocState` — Sealed state interface: `StateInit`, `StateAwaitingReview`,
  `StateApproved`, `StateRejected`.
- `DocEnvironment` — Immutable FSM context holding an actor reference;
  implements `protofsm.TellRefEnv[DocEvent]`.
- `ReviewServiceBehavior` — `ActorBehavior` that performs async document
  review and sends the result event back to the FSM.
- `NotificationServiceBehavior` — `ActorBehavior` that delivers
  approval/rejection notifications.

## Relationships

- **Depends on**: `baselib/actor` (ActorSystem, ServiceKey, TellOnlyRef),
  `baselib/protofsm` (FSM engine, TellRefEnv).
- **Depended on by**: nothing (example/test only).

## Invariants

- All FSM transitions are pure: IO is deferred to `DocOutboxEvent` routing so
  state logic can be tested without mocking actors.
- The example test (`ExampleActorStateMachine`) is a runnable `go test -run`
  example that verifies the full async workflow end-to-end.

## Deep Docs

- [baselib/actor/CLAUDE.md](../actor/CLAUDE.md) — Actor system overview.
- [baselib/protofsm/CLAUDE.md](../protofsm/CLAUDE.md) — FSM engine.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
