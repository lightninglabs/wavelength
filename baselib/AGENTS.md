# baselib

## Purpose

Foundational infrastructure providing the actor framework (`baselib/actor`) and
the protofsm state machine engine (`baselib/protofsm`) that all domain packages
build on.

## Sub-Packages

### baselib/actor
- `ActorSystem` — Central manager owning actors, receptionist discovery, DLO, graceful shutdown.
- `Actor[M, R]` — Concrete actor with mailbox, behavior, and lifecycle.
- `ActorRef[M, R]` — Lightweight shareable reference for Tell (fire-and-forget) and Ask (request-response).
- `ActorBehavior[M, R]` — Strategy interface defining how an actor processes messages.
- `ServiceKey[M, R]` — Type-safe identifier for actor registration and discovery.
- `Future[T]` — Eventual result of Ask operations.

### baselib/protofsm
- `StateMachine[InternalEvent, OutboxEvent, Env]` — Core FSM executor processing events and emitting outbox.
- `State[InternalEvent, OutboxEvent, Env]` — Abstract state with ProcessEvent and IsTerminal.
- `StateTransition[InternalEvent, OutboxEvent, Env]` — Next state + emitted events from a transition.
- `EmittedEvent[InternalEvent, OutboxEvent]` — Internal events (recursive) + outbox events (external).

## Relationships

- **Depends on**: nothing (pure abstraction layer).
- **Depended on by**: every domain package (`round`, `vtxo`, `oor`, `wallet`), `chainsource`, `serverconn`, `db`, `darepod`.

## Invariants

- Every actor message must embed `BaseMessage` to satisfy the sealed `Message` interface.
- Actors registered with system are auto-stopped on `system.Shutdown()`.
- `Tell` is asynchronous and does not preserve the caller's DB transaction.
  Code that needs atomic domain-state plus dispatch must use an explicit SQL
  transaction or a synchronous `Ask` path that completes before commit.
- Protofsm processes events iteratively; internal events loop until exhausted before returning.
- `EmittedEvent` separates internal events (routed back to FSM) from outbox events (dispatched externally).

## Deep Docs

- [baselib/actor/README.md](actor/README.md) — Actor framework concepts and class diagram.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
