# Darepo-Client Architecture

This document provides a top-level map of the darepo-client codebase: an Ark
protocol client daemon that manages VTXOs, participates in rounds, and
communicates with the Ark operator via a mailbox-based RPC protocol.

## Domain Layers

The codebase is organized into four layers. Dependencies flow downward; no
package may import from a higher layer.

### Layer 1: Core Domain (Business Logic)

| Package | Purpose |
|---------|---------|
| [`round`](round/) | Client-side Ark round participation FSM (boarding, refresh, leave) |
| [`vtxo`](vtxo/) | VTXO lifecycle FSM (live, forfeiting, forfeited, spent, expiring) |
| [`oor`](oor/) | Out-of-round transfer coordination FSM |
| [`wallet`](wallet/) | On-chain boarding wallet actor (address derivation, UTXO monitoring) |
| [`lib`](lib/) | Shared domain utilities: tree paths, BIP-322, tapscripts, types |

### Layer 2: Infrastructure (Chain, Storage, Messaging)

| Package | Purpose |
|---------|---------|
| [`baselib`](baselib/) | Actor framework (`baselib/actor`) and protofsm state machine engine (`baselib/protofsm`) |
| [`chainsource`](chainsource/) | `ChainBackend` interface: fee estimation, block/conf/spend notifications |
| [`chainbackends`](chainbackends/) | Concrete `ChainBackend` implementations (LND-backed) |
| [`chain`](chain/) | Bitcoind RPC utilities (package relay, `SubmitPackage`) |
| [`lndbackend`](lndbackend/) | `BoardingBackend` implementation via LND's wallet kit |
| [`lwwallet`](lwwallet/) | Lightweight in-process wallet (btcwallet + Esplora, no external LND) |
| [`db`](db/) | SQLite/PostgreSQL persistence: boarding, rounds, VTXOs, OOR artifacts |
| [`mailbox`](mailbox/) | Mailbox protocol primitives across three sub-packages (pb, rpc, conn) |
| [`serverconn`](serverconn/) | Unified server connector: durable egress, ingress polling, unary RPC facade |

### Layer 3: Application & Orchestration

| Package | Purpose |
|---------|---------|
| [`darepod`](darepod/) | Daemon orchestrator: wires all subsystems, exposes gRPC API |
| [`cmd/darepod`](cmd/darepod/) | Daemon entry point |
| [`cmd/darepocli`](cmd/darepocli/) | CLI client |
| [`timeout`](timeout/) | Generic timeout scheduling actor |
| [`indexer`](indexer/) | Server indexing client for receive script registration |
| [`arkrpc`](arkrpc/) | Server-side gRPC service definitions (ArkService, IndexerService) |
| [`rpc`](rpc/) | Client-side RPC message definitions (roundpb, oorpb) |
| [`daemonrpc`](daemonrpc/) | Daemon gRPC API definitions |

### Layer 4: Testing & Tooling

| Package | Purpose |
|---------|---------|
| [`harness`](harness/) | Docker-based Bitcoin/LND integration test environment |
| [`systest`](systest/) | System-level end-to-end tests |
| [`internal/actortest`](internal/actortest/) | Durable actor integration tests with real DB backends |
| [`internal/testutils`](internal/testutils/) | Deterministic key/signature generation for tests |
| [`rules`](rules/) | ast-grep linting rules for code style enforcement |
| [`tools`](tools/) | Development tool dependencies (protoc plugins, sqlc) |
| [`scripts`](scripts/) | Build and verification scripts |

## Key Dependency Flows

```
darepod (orchestrator)
├── round ──────────┐
│   ├── vtxo        │ (bidirectional: forfeit requests/confirmations)
│   ├── serverconn  │ (outbound RPCs to operator)
│   ├── timeout     │ (scheduling)
│   ├── wallet      │ (boarding intents)
│   └── lib         │ (tree, types, scripts, bip322)
├── vtxo            │
│   ├── chainsource │ (block epoch events)
│   └── db          │ (vtxo store)
├── wallet          │
│   ├── chainsource │ (UTXO confirmation monitoring)
│   └── db          │ (boarding store)
├── oor             │
│   └── db          │ (oor artifact store)
├── serverconn      │
│   ├── mailbox     │ (protocol primitives)
│   └── db          │ (durable delivery store)
├── chainsource     │
│   └── chainbackends (pluggable: LND or lwwallet)
└── db              │
    └── (SQLite | PostgreSQL)
```

## Architectural Patterns

### Protofsm State Machines
Business logic lives in pure FSM transition functions that take `(State, Event)
→ (State, []OutboxEvent)`. Side effects are separated into outbox messages
dispatched by the actor runtime. See `baselib/protofsm`.

### Actor System
Concurrent, message-driven components communicate via `Tell` (fire-and-forget)
and `Ask` (request-response). Actors are registered with a `Receptionist` for
service discovery. See `baselib/actor`.

### Durable Actors
Crash-safe actors persist FSM state + outbox atomically. On restart, undelivered
outbox messages are replayed. At-least-once delivery with deduplication ensures
exactly-once semantics. See `docs/durable_actor_architecture.md`.

### RPC-over-Mailbox
All server communication flows through `serverconn`, which implements unary RPCs
(low-latency) and durable event egress (crash-safe) over the mailbox protocol.
Inbound events are dispatched via `EventRouter`. See `docs/mailbox_architecture.md`.

### Outbox Pattern
FSMs emit messages as data (outbox events). The actor runtime dispatches them
after state is persisted. This ensures no message is sent without the
corresponding state transition being durable.

## Key Types and Interfaces

| Type | Package | Purpose |
|------|---------|---------|
| `ChainBackend` | chainsource | Fee estimation, block/conf/spend notifications |
| `BoardingBackend` | wallet | Key derivation, taproot import, UTXO enumeration |
| `VTXO.Descriptor` | vtxo | Canonical VTXO: outpoint, amount, tapscript, tree path, CSV expiry |
| `RoundClientActor` | round | Primary FSM actor for interactive round phases |
| `IntentPackage` | round | Accumulated boarding/VTXO/forfeit/leave pools |
| `BoardingAddress` | wallet | 2-of-2 multisig (client+operator) with CSV timeout |
| `Envelope` | mailbox/pb | Durable unit of mailbox transport |
| `RPCClient` | mailbox/rpc | SendRPC/AwaitRPC interface for generated stubs |
| `Message` | baselib/actor | Sealed interface for actor messages |
| `Ref[Msg, Resp]` | baselib/actor | Typed actor reference (Tell, Ask) |

## State Machines

### Round FSM
```
Idle → PendingRoundAssembly → RegistrationSent → RoundJoined →
CommitmentTxReceived → CommitmentTxValidated → NoncesSent →
NoncesAggregated → PartialSigsSent → [ForfeitSignaturesCollecting] →
InputSigSent → Confirmed → Idle
```

### VTXO FSM
```
Live → RefreshRequested → Forfeiting → Forfeited
Live → Expiring → (sweep or refresh)
Live → Spent
Any → Failed
```

### OOR FSM
Manages outgoing/incoming transfer state through checkpoint signing with
deterministic retry semantics. See `oor/README.md` (if present) or `oor/doc.go`.

## Per-Package Agent Context

Each major package contains a `CLAUDE.md`/`AGENTS.md` with:
- Purpose, key types, relationships (imports/imported-by)
- Actor message flows and invariants
- Links to deeper documentation

These files form a navigable graph. Start here, then follow links into packages
relevant to your task.
