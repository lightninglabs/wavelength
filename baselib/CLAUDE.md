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

### baselib/example
- Runnable reference wiring a `protofsm` state machine to `actor` services;
  not used by production code. See `baselib/example/CLAUDE.md`.

## Relationships

- **Depends on**: `lnd/tlv`, `lnd/fn/v2`, `lnd/clock` (external, no
  darepo-client-specific logic); `darepo-client/build` (context-scoped logger
  helper only) is the one root-module import, otherwise this is a pure
  abstraction layer.
- **Depended on by**: every domain package (`round`, `vtxo`, `oor`, `wallet`), `chainsource`, `serverconn`, `db`, `darepod`.

## Invariants

- Every actor message must embed `BaseMessage` to satisfy the sealed `Message` interface.
- Actors registered with system are auto-stopped on `system.Shutdown()`.
- `DurableMailbox.Send` preserves the sender's DB transaction in the context. Same-DB actors share the tx for atomic enqueue via `ExecTx` joining.
- Protofsm processes events iteratively; internal events loop until exhausted before returning.
- `EmittedEvent` separates internal events (routed back to FSM) from outbox events (dispatched externally).

## Deep Docs

- [baselib/actor/README.md](actor/README.md) — Actor framework concepts and class diagram.
- [docs/durable_actor_architecture.md](../docs/durable_actor_architecture.md) — Durable actor internals.
- [docs/durable_actor_quickstart.md](../docs/durable_actor_quickstart.md) — Developer migration guide.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
