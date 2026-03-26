# Darepo-Server Architecture

This document provides a top-level map of the darepo-server codebase: an Ark
protocol operator daemon that coordinates rounds, manages VTXOs, processes
out-of-round transfers, and communicates with N clients via a durable
mailbox-based RPC protocol.

## Domain Layers

The codebase is organized into four layers. Dependencies flow downward; no
package may import from a higher layer.

### Layer 1: Core Domain (Business Logic)

| Package | Purpose |
|---------|---------|
| [`rounds`](rounds/) | Round lifecycle FSM (registration, signing, finalization, confirmation) |
| [`oor`](oor/) | Out-of-round transfer coordinator FSM |
| [`vtxo`](vtxo/) | VTXO locking, lifecycle tracking, and persistence |
| [`batch`](batch/) | Batch transaction building, MuSig2 nonce/signature coordination |

### Layer 2: Infrastructure (Chain, Storage, Messaging)

| Package | Purpose |
|---------|---------|
| [`clientconn`](clientconn/) | Server-side 1:N durable mailbox bridge to clients |
| [`mailbox`](mailbox/) | Durable envelope store and delivery primitives |
| [`mailboxrpcserver`](mailboxrpcserver/) | gRPC mailbox service implementation |
| [`db`](db/) | PostgreSQL/SQLite persistence: rounds, VTXOs, OOR, mailbox state |
| [`lndbackend`](lndbackend/) | LND chain backend integration (ChainSource, WalletController) |
| [`indexer`](indexer/) | Wallet-scoped VTXO/round/OOR event query service |
| [`batchwatcher`](batchwatcher/) | On-chain batch transaction monitoring and VTXO spend detection |
| [`batchsweeper`](batchsweeper/) | Expired batch recovery via sweep transactions |

### Layer 3: Application & Orchestration

| Package | Purpose |
|---------|---------|
| root (`darepo`) | Server orchestrator: wires all subsystems, exposes gRPC API |
| [`cmd/arkd`](cmd/arkd/) | Daemon entry point |
| [`cmd/arkcli`](cmd/arkcli/) | Admin CLI (trigger batch, list rounds, etc.) |
| [`cmd/merge-sql-schemas`](cmd/merge-sql-schemas/) | Schema consolidation utility |
| [`adminrpc`](adminrpc/) | Admin service gRPC stub definitions |
| [`build`](build/) | Logging infrastructure, deployment modes, version info |

### Layer 4: Testing & Tooling

| Package | Purpose |
|---------|---------|
| [`harness`](harness/) | In-process Bitcoin/LND integration test environment |
| [`itest`](itest/) | Real-daemon integration tests (boarding, OOR, refresh, restart) |
| [`systest`](systest/) | System-level end-to-end tests |
| [`internal`](internal/) | Internal test utilities container |
| [`internal/testutils`](internal/testutils/) | Deterministic key/signature generation for tests |
| [`rules`](rules/) | ast-grep linting rules for code style enforcement |
| [`tools`](tools/) | Development tool dependencies (protoc plugins, sqlc) |
| [`scripts`](scripts/) | Build and verification scripts |

## Key Dependency Flows

```
Server (darepo, root orchestrator)
├── rounds ──────────┐
│   ├── batch        │ (tx building, MuSig2 coordination)
│   ├── batchwatcher │ (confirmation monitoring)
│   ├── clientconn   │ (outbound events to clients)
│   └── vtxo         │ (VTXO locking during rounds)
├── oor              │
│   ├── clientconn   │ (outbound events to clients)
│   ├── db           │ (OOR session persistence)
│   └── vtxo         │ (VTXO locking during transfers)
├── indexer          │
│   ├── clientconn   │ (per-client query dispatch)
│   ├── db           │ (wallet-scoped queries)
│   └── rounds       │ (round event subscription)
├── clientconn       │
│   └── mailbox      │ (envelope store & delivery)
├── batchsweeper     │
│   └── batchwatcher │ (sweep eligible batches)
├── lndbackend       │
│   └── rounds       │ (chain queries, wallet ops)
├── bitcoind (opt)   │ (direct RPC for boarding UTXO validation)
└── db               │
    └── (PostgreSQL | SQLite)
```

## Dispatch Pipeline

Client requests follow one of two dispatch models:

**Fire-and-Forget (EventRouter)** — Used by rounds and OOR RPCs:
```
1. Client sends KIND_REQUEST envelope to server mailbox
   ↓
2. clientconn Ingress Loop (extract {Service, Method}, lookup DispatcherMap)
   ↓
3. EnvelopeDispatcher (from AddEnvelopeRoute)
   - Unmarshal body → typed proto
   - Adapt(env, proto) → actor message (extracts ClientID from env.Sender)
   ↓
4. actorKey.Ref(system).Tell(ctx, actorMsg) — durable commit
   ↓
5. Actor processes event → state transition → outbox messages
   ↓
6. FSM transitions execute side effects inline (DB, wallet, client notify via bridge)
```

**Synchronous Request-Response (Operator)** — Used by indexer and ArkService:
```
1–2. Same ingress path as above
   ↓
3. EnvelopeDispatcher (operator makeDispatcher closure)
   - Injects ClientID, calls ServeMux.ServeRPC
   - Builds KIND_RESPONSE envelope, sends via Edge.Send
```

See [`docs/dispatch_pipeline.md`](docs/dispatch_pipeline.md) for full details.

## Architectural Patterns

### Protofsm State Machines
Business logic lives in pure FSM transition functions that take `(State, Event)
→ (State, []OutboxEvent)`. Side effects are separated into outbox messages
dispatched by the actor runtime. See `baselib/protofsm` in the client submodule.

### Actor System
Concurrent, message-driven components communicate via `Tell` (fire-and-forget)
and `Ask` (request-response). Actors are registered with a `Receptionist` for
service discovery. See `baselib/actor` in the client submodule.

### Durable Actors
Crash-safe actors persist FSM state + outbox atomically. On restart, undelivered
outbox messages are replayed. At-least-once delivery with deduplication ensures
exactly-once semantics.

### RPC-over-Mailbox
All client communication flows through `clientconn`, which implements a 1:N
durable mailbox bridge. Each client gets a dedicated `ClientRuntime` with
persistent egress and ingress loops. See
[`docs/clientconn_architecture.md`](docs/clientconn_architecture.md).

### Outbox Pattern
FSMs emit messages as data (outbox events). The actor runtime dispatches them
after state is persisted. This ensures no message is sent without the
corresponding state transition being durable.

## Key Types and Interfaces

| Type | Package | Purpose |
|------|---------|---------|
| `Server` | darepo | Main orchestrator: wires subsystems, runs gRPC servers |
| `Actor` | rounds | Round FSM driver with durable state |
| `RoundID` | rounds | UUID-based round identifier |
| `Actor` | oor | OOR transfer coordinator with durable state |
| `SessionID` | oor | OOR transfer session identifier |
| `Terms` | batch | Round parameters (sweep delay, max VTXOs, fee rates) |
| `TxSignerCoordinator` | batch | MuSig2 nonce/signature coordination |
| `ClientsConnBridge` | clientconn | 1:N router multiplexing by ClientID |
| `ClientRuntime` | clientconn | Per-client state container (egress, ingress) |
| `EnvelopeDispatcher` | clientconn | Request routing closure per service/method |
| `DispatcherMap` | clientconn | Map of service/method → dispatcher |
| `EventRouter` | clientconn | Collects typed dispatch routes, returns `DispatcherMap` |
| `Locker` | vtxo | Thread-safe VTXO mutual exclusion |
| `LeaseLocker` | vtxo | Time-bounded VTXO locks |
| `Store` | vtxo | VTXO record persistence |
| `Service` | indexer | Wallet-scoped query service |
| `Operator` | indexer | RPC dispatcher factory for indexer |
| `Store` | db | Main persistence layer (wraps Postgres/SQLite) |
| `Store` | mailbox | Durable envelope store |

## State Machines

### Round FSM
```
Created → Registration → AwaitingJoinValidation → BatchBuilding →
AwaitingBatchBuild → BatchBuilt → AwaitingInputSigs →
AwaitingVTXONonces → AwaitingVTXOSignatures → ServerSigning →
AwaitingSignAndFinalize → AwaitingServerSignPersist → Finalized →
AwaitingConfirmPersist → Confirmed
                                          Any → Failed
```

### OOR FSM
```
Idle → AwaitingInputsLock → AwaitingSubmitValidation → Validated →
CoSigned → AwaitingFinalizeValidation → AwaitingRecipientsNotify →
Finalized
                                          Any → Failed
```

### VTXO Lifecycle
```
Live → InFlight → Spent
```

## Per-Package Agent Context

Each major package contains a `CLAUDE.md`/`AGENTS.md` with:
- Purpose, key types, relationships (imports/imported-by)
- Actor message flows and invariants
- Links to deeper documentation

These files form a navigable graph. Start here, then follow links into packages
relevant to your task.
