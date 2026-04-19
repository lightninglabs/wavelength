# baselib/example

## Purpose

Working example demonstrating integration of the `baselib/actor` framework
with `baselib/protofsm`, modeling a document-approval workflow (Init →
AwaitingReview → Approved/Rejected) with actor services and outbox routing.

## Key Types

- `DocEvent` — Sealed interface for FSM events (`EventSubmitDocument`,
  `EventReviewStarted`, `EventApproved`, `EventRejected`, `EventResume`).
- `DocState` — Sealed interface for FSM states (`StateInit`,
  `StateAwaitingReview`, `StateApproved`, `StateRejected`).
- `DocOutboxEvent` — Sealed outbox interface (`OutboxRequestReview`,
  `OutboxNotify`) routing actor messages to external services.
- `DocEnvironment` — FSM environment holding a `TellOnlyRef` back to the FSM
  actor, satisfying the `protofsm.FSMEnv` interface.
- `ReviewServiceBehavior` — Actor behavior implementing the review step;
  sends `EventReviewStarted` then `EventApproved` back via `Tell`.
- `NotificationServiceBehavior` — Actor behavior processing notify messages.

## Relationships

- **Depends on**: `baselib/actor` (ActorSystem, TellOnlyRef, ServiceKey,
  RegisterWithSystem), `baselib/protofsm` (StateMachine, ActorMessage,
  RoutedOutboxEvent, NewSystemsActorStateMachine).
- **Depended on by**: nothing (example/documentation package only).
- **Sends**:
  - → `ReviewService`: `OutboxRequestReview` (carries `ReviewMsg`)
  - → `NotificationService`: `OutboxNotify` (carries `NotifyMsg`)
- **Receives**:
  - ← `ReviewServiceBehavior`: `protofsm.ActorMessage[DocEvent]{EventReviewStarted}`, `{EventApproved}`

## Invariants

- All files use `package baselib_test`; this package is for documentation and
  is never imported by production code.
- FSM events flow back into the actor via `Tell` on the embedded
  `DocEnvironment.actorRef`, not via direct function calls.

## Deep Docs

- [baselib/CLAUDE.md](../CLAUDE.md) — Parent baselib overview.
- [docs/durable_actor_quickstart.md](../../docs/durable_actor_quickstart.md) — ActorBehavior, TLVMessage guide.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
