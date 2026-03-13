# clientconn

## Purpose

Server-side 1:N durable mailbox bridge for communicating with Ark clients.
Multiplexes per-client connections, each with persistent egress (server-to-client
events) and ingress (client-to-server requests) loops, dispatching inbound RPCs
to the appropriate service operator.

## Key Types

- `ClientsConnBridge` — 1:N router that multiplexes by ClientID. Also provides `HandleInbound` for unknown client detection at the transport boundary.
- `ClientConnectionActor` — Per-client durable actor behavior.
- `ClientRuntime` — Per-client state container (egress actor, ingress loop, response registry).
- `ClientID` — String type alias identifying a connected client.
- `ClientConnMsg` — Messages sent to the bridge actor.
- `EnvelopeDispatcher` — `func(ctx, *Envelope) error` routing closure per service/method.
- `DispatcherMap` — `map[ServiceMethod]EnvelopeDispatcher` for request routing.
- `EventRouter` — Collects typed dispatch routes, returns `DispatcherMap` via `AsDispatcherMap()`.
- `EventRouteConfig` — Generic config for fire-and-forget routes with custom `Adapt` closures.
- `EnvelopeRouteConfig` — Like `EventRouteConfig` but passes the full envelope to `Adapt` (for extracting transport metadata like client ID).
- `InboundActorMessage` — Constraint combining `actor.Message` + `InboundClientMessage` for self-deserializing messages.
- `AddRoute` / `AddEnvelopeRoute` — Package-level generic functions registering typed routes on an `EventRouter`.
- `UnknownClientHandler` — Interface for handling envelopes from unregistered senders. The `Server` type in the root package implements this to auto-register external clients.
- `WithOnUnknownClient` — Bridge option that configures an `UnknownClientHandler`. Concurrent registrations for the same clientID are deduplicated via `singleflight.Group`.

## Relationships

- **Depends on**: `mailbox` (envelope store and delivery primitives), client submodule's `baselib/actor`.
- **Depended on by**: `rounds` (outbound round events), `oor` (outbound OOR events), `indexer` (per-client queries), root `darepo` (wiring).
- **Messages to/from**:
  - Receives envelopes from clients via mailbox ingress.
  - Sends envelopes to clients via durable egress actors.
  - Dispatches fire-and-forget requests to `rounds`, `oor` actors via `EventRouter` (`AddEnvelopeRoute`).
  - Dispatches synchronous request-response RPCs to `indexer` and `ArkService` via operator pattern (`ServeMux`).

## Dispatch Model Decision

The key question when choosing a dispatch model: **does the client expect a
response envelope?**
- **Yes** → use operator/`ServeMux` (synchronous request-response). Used by
  IndexerService and ArkService.
- **No** → use `EventRouter`/`AddRoute`/`AddEnvelopeRoute` (fire-and-forget
  `Tell`). Used by rounds and OOR RPCs.

## Invariants

- Each client gets exactly one `ClientRuntime`; runtime creation is idempotent.
- Egress is durable: messages persist before delivery, replayed on restart.
- Ingress dispatchers must be registered before the client runtime starts.
- The bridge must handle client disconnection gracefully without losing queued egress messages.
- `HandleInbound` uses `singleflight.Group` keyed by clientID to deduplicate concurrent registration attempts for the same client.

## Deep Docs

- [clientconn/README.md](README.md) — Quickstart and API reference.
- [docs/clientconn_architecture.md](../docs/clientconn_architecture.md) — Full architecture deep dive.
- [docs/dispatch_pipeline.md](../docs/dispatch_pipeline.md) — Request routing pipeline.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
