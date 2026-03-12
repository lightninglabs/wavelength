# clientconn

## Purpose

Server-side 1:N durable mailbox bridge for communicating with Ark clients.
Multiplexes per-client connections, each with persistent egress (server-to-client
events) and ingress (client-to-server requests) loops, dispatching inbound RPCs
to the appropriate service operator.

## Key Types

- `ClientsConnBridge` — 1:N router that multiplexes by ClientID.
- `ClientConnectionActor` — Per-client durable actor behavior.
- `ClientRuntime` — Per-client state container (egress actor, ingress loop, response registry).
- `ClientID` — String type alias identifying a connected client.
- `ClientConnMsg` — Messages sent to the bridge actor.
- `EnvelopeDispatcher` — `func(ctx, *Envelope) error` routing closure per service/method.
- `DispatcherMap` — `map[ServiceMethod]EnvelopeDispatcher` for request routing.

## Relationships

- **Depends on**: `mailbox` (envelope store and delivery primitives), client submodule's `baselib/actor`.
- **Depended on by**: `rounds` (outbound round events), `oor` (outbound OOR events), `indexer` (per-client queries), root `darepo` (wiring).
- **Messages to/from**:
  - Receives envelopes from clients via mailbox ingress.
  - Sends envelopes to clients via durable egress actors.
  - Dispatches requests to `rounds`, `oor`, `indexer` operators.

## Invariants

- Each client gets exactly one `ClientRuntime`; runtime creation is idempotent.
- Egress is durable: messages persist before delivery, replayed on restart.
- Ingress dispatchers must be registered before the client runtime starts.
- The bridge must handle client disconnection gracefully without losing queued egress messages.

## Deep Docs

- [clientconn/README.md](README.md) — Quickstart and API reference.
- [docs/clientconn_architecture.md](../docs/clientconn_architecture.md) — Full architecture deep dive.
- [docs/dispatch_pipeline.md](../docs/dispatch_pipeline.md) — Request routing pipeline.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
