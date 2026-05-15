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
| [`ledger`](ledger/) | Client-side durable ledger actor for double-entry fee accounting |
| [`lib`](lib/) | Shared domain utilities: tree paths, BIP-322, arkscript policy, types |
| [`lib/arkscript`](lib/arkscript/) | Tapscript AST compiler and policy system for Ark taproot outputs |
| [`lib/bip322`](lib/bip322/) | BIP-322 intent-bound message authentication |
| [`lib/tx/arktx`](lib/tx/arktx/) | Canonical Ark transaction ordering and validation |
| [`lib/tx/checkpoint`](lib/tx/checkpoint/) | Checkpoint PSBT construction for OOR transfers |
| [`lib/tx/oor`](lib/tx/oor/) | OOR submit/finalize package builders and validators |
| [`lib/tx/psbtutil`](lib/tx/psbtutil/) | PSBT encoding, decoding, and signature attachment helpers |
| [`lib/recovery`](lib/recovery/) | Immutable recovery proof graph, session state machine, TLV codec for unilateral exit |
| [`unrollplan`](unrollplan/) | Pure dependency-resolution planner driving unilateral-exit broadcast/sweep ordering |

### Layer 2: Infrastructure (Chain, Storage, Messaging)

| Package | Purpose |
|---------|---------|
| [`baselib`](baselib/) | Actor framework (`baselib/actor`) and protofsm state machine engine (`baselib/protofsm`) |
| [`chainsource`](chainsource/) | `ChainBackend` interface: fee estimation, block/conf/spend notifications |
| [`chainbackends`](chainbackends/) | LND-backed `ChainBackend` implementation plus lndclient adapters (`TxBroadcaster`, `PackageSubmitter`) |
| [`chain`](chain/) | Bitcoind RPC utilities (package relay, `SubmitPackage`) |
| [`txconfirm`](txconfirm/) | Generic "broadcast + CPFP fee-bump + notify on confirm" actor with per-parent fee-input reservations and BIP-125 Rule 3/4 enforcement |
| [`unroll`](unroll/) | Durable per-target unilateral-exit actor + thin registry: owns proof assembly, materialization, CSV maturity, final sweep build, persist-before-broadcast, and control-plane record persistence |
| [`fraud`](fraud/) | Passive OOR ancestry fraud watch actor: arms per-ancestor spend watches and triggers `unroll.EnsureUnrollRequest{TriggerFraudSpend}` on detection |
| [`lndbackend`](lndbackend/) | `BoardingBackend` implementation via LND's wallet kit |
| [`lwwallet`](lwwallet/) | Lightweight in-process wallet (btcwallet + Esplora, no external LND) |
| [`btcwbackend`](btcwbackend/) | Neutrino-backed wallet backend (btcwallet + compact block filters) |
| [`walletcore`](walletcore/) | Shared wallet abstractions and boarding logic used by lwwallet and btcwbackend |
| [`proofkeys`](proofkeys/) | Interface for wallet-managed key derivation and indexer proof signing |
| [`db`](db/) | SQLite/PostgreSQL persistence: boarding, rounds, VTXOs, OOR artifacts, fee ledger |
| [`mailbox`](mailbox/) | Mailbox protocol primitives across three sub-packages (pb, rpc, conn) |
| [`serverconn`](serverconn/) | Unified server connector: durable egress, ingress polling, unary RPC facade |

### Layer 3: Application & Orchestration

| Package | Purpose |
|---------|---------|
| [`darepod`](darepod/) | Daemon orchestrator: wires all subsystems, exposes gRPC API |
| [`sdk/ark`](sdk/ark/) | Consumer-facing Go SDK facade: remote or embedded daemon access with typed models |
| [`sdk/swaps`](sdk/swaps/) | Lightning-to-Ark / Ark-to-Lightning atomic swap SDK with durable FSM flows |
| [`swapclientserver`](swapclientserver/) | Optional daemon-side swap subserver (build tag `swapruntime`): translates `swapclientrpc` RPCs into `sdk/swaps` operations and manages the daemon-local worker registry |
| [`cmd/darepod`](cmd/darepod/) | Daemon entry point |
| [`cmd/darepocli`](cmd/darepocli/) | CLI client |
| [`timeout`](timeout/) | Generic timeout scheduling actor |
| [`indexer`](indexer/) | Server indexing client for receive script registration |
| [`arkrpc`](arkrpc/) | Server-side gRPC service definitions (ArkService, IndexerService) |
| [`arkrpc/treeconv`](arkrpc/treeconv/) | Narrow re-export of tree-path conversion helpers without the full gRPC surface |
| [`rpc`](rpc/) | Client-side RPC message definitions (roundpb, oorpb, swapclientrpc) |
| [`daemonrpc`](daemonrpc/) | Daemon gRPC API definitions |

### Layer 4: Testing & Tooling

| Package | Purpose |
|---------|---------|
| [`harness`](harness/) | Docker-based Bitcoin/LND integration test environment |
| [`systest`](systest/) | System-level end-to-end tests |
| [`internal/actortest`](internal/actortest/) | Durable actor integration tests with real DB backends |
| [`internal/testutils`](internal/testutils/) | Deterministic key/signature generation for tests |
| [`internal/indexerlimits`](internal/indexerlimits/) | Client-side indexer pagination safety bounds (cursor size cap) |
| [`rules`](rules/) | ast-grep linting rules for code style enforcement |
| [`tools`](tools/) | Development tool dependencies (protoc plugins, sqlc) |
| [`cmd/protoc-gen-mailboxrpc`](cmd/protoc-gen-mailboxrpc/) | `protoc` plugin generating typed `mailbox/rpc` client/server stubs from `.proto` service definitions |
| [`scripts`](scripts/) | Build and verification scripts |

## Key Dependency Flows

```
darepod (orchestrator)
├── round ──────────┐
│   ├── vtxo        │ (bidirectional: forfeit requests/confirmations)
│   ├── serverconn  │ (outbound RPCs to operator)
│   ├── timeout     │ (scheduling)
│   ├── wallet      │ (boarding intents)
│   ├── ledger      │ (VTXOReceivedMsg / FeePaidMsg via ledger.Sink)
│   └── lib         │ (tree, types, arkscript, bip322)
├── vtxo            │
│   ├── chainsource │ (block epoch events)
│   ├── ledger      │ (ExitCostMsg via ledger.Sink — emission planned)
│   └── db          │ (vtxo store)
├── wallet          │
│   ├── chainsource │ (UTXO confirmation monitoring)
│   ├── ledger      │ (UTXOCreatedMsg via ledger.Sink)
│   └── db          │ (boarding store)
├── oor             │
│   ├── db          │ (oor artifact store)
│   ├── ledger      │ (VTXOSentMsg / VTXOReceivedMsg via ledger.Sink)
│   └── lib/tx      │ (arktx, checkpoint, oor, psbtutil)
├── unroll          │ (unilateral-exit registry + per-target actor)
│   ├── txconfirm   │ (CPFP / confirmation tracking)
│   ├── lib/recovery│ (recovery proof graph)
│   ├── unrollplan  │ (pure dependency-resolution planner)
│   └── db          │ (unilateral_exit_jobs store)
├── txconfirm       │ (shared tx confirmation + CPFP actor; wired by darepod)
├── ledger          │
│   ├── baselib/actor (durable mailbox, TLV codec)
│   └── db          │ (LedgerStoreDB + UTXOAuditStoreDB adapters)
├── serverconn      │
│   ├── mailbox     │ (protocol primitives)
│   └── db          │ (durable delivery store)
├── proofkeys       │ (wallet key derivation for indexer proofs)
│   └── walletcore / lndbackend (implementations)
├── chainsource     │
│   └── chainbackends (pluggable: LND, lwwallet, or btcwbackend)
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
Inbound events are dispatched via `EventRouter`. Registered routes currently
include OOR lifecycle pushes, round progress pushes, and `MethodIncomingVTXO`
(which delivers `arkrpc.IncomingVTXOEvent` notifications to the
`vtxo.IncomingVTXOHandler` actor for local materialization of round-produced
VTXOs owned by the local wallet). See `docs/mailbox_architecture.md`.

### Data-Driven Script Ownership
Local wallet ownership of a VTXO is resolved at round confirmation time by
looking up its pkScript in a persistent "owned receive scripts" table (the OOR
artifact store). The round FSM calls `OwnedScriptChecker.IsOwnedScript` for
every VTXO in a completed round and only persists the ones the wallet
recognizes. The round actor populates this table via `OwnedScriptRegistrar`
when it builds change/refresh intents and when it accepts a `RegisterIntentMsg`
whose owner key has a non-zero `KeyLocator`. Directed-send recipient keys
intentionally carry a zero `KeyLocator` so they are not registered on the
sender side — the recipient materializes those VTXOs via the incoming VTXO
push path instead.

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
| `ClientWallet` | round | MuSig2 signing + key derivation interface for round participation |
| `OwnedScriptChecker` | round | Data-driven pkScript ownership lookup used by the round FSM at confirmation time (replaces the old `IsOwner` flag) |
| `OwnedScriptRegistrar` | round | Persists locally-owned pkScripts when the round actor builds/accepts VTXO intents so the checker recognizes them on confirmation |
| `IncomingVTXOHandler` | vtxo | Materializes round-produced VTXOs from indexer push notifications when the local wallet owns the receive script |
| `OwnedScriptLookup` | vtxo | Read-only view of the owned receive scripts store used by `IncomingVTXOHandler` |
| `VTXOReader` | wallet | Read-only VTXO descriptor access (breaks import cycle) |
| `SelectedVTXO` | wallet | Locked VTXO descriptor for transfer inputs (breaks import cycle) |
| `TxInfo` | wallet | Confirmed transaction with block hash and height |
| `Backend` | proofkeys | Wallet key derivation and proof signing interface |
| `Node` | lib/arkscript | Sealed AST node interface for tapscript compilation |
| `VTXOPolicy` | lib/arkscript | Compiled VTXO taproot policy with collab/exit spend paths |

## State Machines

### Round FSM
```
Idle → PendingRoundAssembly → RegistrationSent → RoundJoined →
CommitmentTxReceived → CommitmentTxValidated → NoncesSent →
NoncesAggregated → PartialSigsSent → [ForfeitSignaturesCollecting] →
InputSigSent → Confirmed → Idle
```
ForfeitSignaturesCollecting is entered only when `len(ForfeitMappings) > 0`
(i.e., the round includes refresh or leave VTXOs). Boarding-only rounds
skip directly from PartialSigsSent to InputSigSent. Forfeit collection
happens after VTXO tree signing to ensure clients only forfeit old VTXOs
once new ones are confirmed signed.

### VTXO FSM
```
Live → PendingForfeit → Forfeiting → Forfeited
Live → Forfeiting → Forfeited  (fast path: ForfeitRequestEvent in LiveState)
PendingForfeit → UnilateralExit  (critical expiry while pending)
Live → UnilateralExit  (critical expiry)
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
