# oor

## Purpose

Client-side out-of-round (OOR) VTXO transfer coordination without waiting for
normal rounds, preserving deterministic transaction construction and crash-safe
resume semantics.

## Key Types

- `SessionID` — Stable session identifier (Ark txid hash in v0).
- `Environment` — FSM environment providing SessionID and external system access.
- `OutboxHandler` — Interface for executing FSM outbox requests (RPC, signing, persistence).
- `ClientActorCfg` — Configuration for OORClientActor (OutboxHandler, ServerConn, PackageStore, DeliveryStore).
- `OORClientActor` — Durable actor wrapping per-session state machines.

## Relationships

- **Depends on**: `baselib/protofsm` (FSM engine), `baselib/actor` (durable actors), `serverconn` (durable transport).
- **Depended on by**: `darepod` (wiring).
- **Messages to/from**:
  - Sends durable transport messages → `serverconn` (to operator).
  - Receives checkpoint responses ← `serverconn` (from operator).

## Invariants

- Point-of-no-return: when server co-signs checkpoint transaction(s).
- After checkpoint signature, client must resume and obtain byte-identical co-signed PSBTs (deterministic construction).
- Outbox messages from transport layer bypass OutboxHandler and go directly to ServerConn for durable delivery.
- Package persistence tracks finalized outgoing packages and local input bindings for recovery.

## Deep Docs

- [oor/doc.go](doc.go) — Package overview.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
