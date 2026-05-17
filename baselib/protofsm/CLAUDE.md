# baselib/protofsm

## Purpose

Protocol-style finite state machine engine that separates pure state transitions
from side effects. Business logic lives in `(State, Event) → (State, []OutboxEvent)`
transition functions; the runtime dispatches outbox messages after state is
durably persisted.

## Key Types

- `State[E, O, Env]` — Interface for FSM states: `ProcessEvent` returns the next state and outbox events.
- `StateMachine[E, O, Env]` — Non-actor FSM runner (for testing or embedded use).
- `StateMachineCfg[E, O, Env]` — Configuration for state machines (initial state, environment, transition table).
- `ActorStateMachine[E, O, Env]` — FSM wrapped as an actor behavior for use with `baselib/actor`.
- `EmittedEvent[E, O]` — Pair of (next state, outbox events) returned by transitions.
- `StateTransition[E, O, Env]` — Single transition result (new state + emitted events).
- `TransitionTable[S, E, M]` — Declarative transition table mapping (State, Event) → handler.
- `TransitionEntry[S, E, M]` — Single entry in a transition table.
- `RoutedOutboxEvent[M, R]` — Outbox event that targets a specific actor via `ServiceKey` (Tell or Ask delivery).
- `ActorOutboxEvent` — Interface for outbox events that can be dispatched by the actor runtime.
- `Environment` — Marker interface for FSM environment (provides external resources to transitions).
- `ErrorReporter` — Interface for reporting FSM errors to external systems.
- `DeliveryMode` — Enum: `DeliveryModeTell` (fire-and-forget) or `DeliveryModeAsk` (request-response).

## Relationships

- **Depends on**: `baselib/actor` (actor integration, ServiceKey, Message).
- **Depended on by**: `round` (round FSM), `vtxo` (VTXO lifecycle FSM), `oor` (OOR transfer FSM).

## Invariants

- Transition functions must be pure: no I/O, no network calls, no database writes. All side effects are expressed as outbox events.
- Outbox events must be dispatched only after the owning subsystem has persisted
  any restart-critical domain facts.
- `TransitionTable` enforces exhaustive (State, Event) coverage at compile time via type constraints.
- `RoutedOutboxEvent` captures the target `ServiceKey` so the runtime can dispatch to the correct actor without the FSM knowing about actor references.

## Deep Docs

- [baselib/CLAUDE.md](../CLAUDE.md) — Parent baselib package overview.
- [ARCHITECTURE.md](../../ARCHITECTURE.md) — System-wide package map.
