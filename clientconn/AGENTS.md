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
- `StatusTracker` — Interface for client liveness monitoring. Composes `ActivityMarker` and `ClientRegistrar`. Reports `ClientStatus` (Online/Offline/Unknown) and fires status-change callbacks.
- `ActivityMarker` — Interface for recording inbound client activity (e.g., mailbox pulls).
- `ClientRegistrar` — Interface for registering/unregistering clients with the status tracker.
- `PullActivityTracker` — `StatusTracker` implementation using mailbox pull timestamps. Transitions clients online/offline based on configurable activity and idle timeouts.
- `HeartbeatService` / `HeartbeatDispatcher` — Well-known service name and no-op dispatcher for client heartbeat envelopes. Heartbeats are treated as liveness signals without application logic.
- `ClientMessage.CorrelationKey() string` — Per-message FIFO key plumbed
  through the wrapper `sendEventMsg` (TLV record type 8) into the
  per-client durable mailbox's `correlation_key` column. Two outbox
  events with the same key (e.g. `(clientID, roundID)`) are claim-ordered
  by emission order regardless of retry backoff. Returning the empty
  string opts out of per-key FIFO (used by `ClientErrorResp` and the
  indexer event push paths). See `client/baselib/actor/CLAUDE.md` for the
  underlying claim invariant.
- `rounds.roundClientCorrelationKey(clientID, roundID)` — Canonical key
  shape for round outbox events: `"<clientID>/<roundID>"`. Implemented on
  every `ClientMessage` in `rounds/outbox_messages.go` that carries both
  a client and a round id.

## Relationships

- **Depends on**: `mailbox` (envelope store and delivery primitives), client submodule's `baselib/actor`.
- **Depended on by**: `rounds` (outbound round events), `oor` (outbound OOR events), `indexer` (per-client queries), `metrics` (dispatch latency events), root `darepo` (wiring).
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
- Envelope dispatch latency is instrumented via `DispatchCompletedMsg` sent to
  the metrics actor after each ingress dispatch.
- **Per-correlation-key FIFO across server-to-client outbox events.**
  `ClientMessage.CorrelationKey()` plumbs into the per-client durable
  mailbox so two same-key events (e.g. the join-ack and the seal-time
  quote for the same round) cannot reorder under transient `Edge.Send`
  failure. The framework-layer invariant lives in
  `client/baselib/actor` and is exercised end-to-end by
  `TestRoundJoinAckSurvivesTransientSendFailure` in systest with the
  `InstrumentedMailbox.FailNextSends` fault-injection hook.

## Deep Docs

- [clientconn/README.md](README.md) — Quickstart and API reference.
- [docs/clientconn_architecture.md](../docs/clientconn_architecture.md) — Full architecture deep dive.
- [docs/dispatch_pipeline.md](../docs/dispatch_pipeline.md) — Request routing pipeline.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
