# metrics

## Purpose

Prometheus instrumentation for the darepo client daemon (`darepod`). All
metrics are namespaced under `darepod_`. Mirrors the arkd **server** metrics
package (one directory up) in structure and collection strategy.

Two collection strategies:

1. **Event-driven** — `MetricsActor` receives typed fire-and-forget messages
   (`metrics.Msg`) and increments lifecycle counters. All instrumentation
   logic lives in the actor; no call site touches Prometheus directly.
2. **Scrape-driven** — `SystemCollector` implements `prometheus.Collector` and
   queries the VTXO store on each scrape so balance/inventory gauges stay
   fresh without a ticker.

Also provides the opt-in HTTP `/metrics` server and the shared
`GRPCClientMetrics` for client-side gRPC interceptors.

## Key Concepts

Use `go doc metrics.<Symbol>` for signatures.

- **`MetricsActor`** / **`ActorConfig`** — event-driven counters. Registered
  under `ActorKey`; producers hold a `Sink` (`actor.TellOnlyRef[Msg]`).
- **`Sink`** / **`NewSink`** — fire-and-forget reference, resolved from the
  actor system via the service key. Mirrors `ledger.Sink`.
- **`SystemCollector`** + **`VTXOStatsQuerier`** — scrape-driven VTXO gauges.
  The `darepod` `vtxoStatsAdapter` implements the querier (lists per status,
  aggregates count + value).
- **`Server`** / **`ServerConfig`** — opt-in HTTP scrape endpoint. Disabled
  unless `ListenAddr` is set.
- **`GRPCClientMetrics`** — shared
  `go-grpc-middleware/providers/prometheus` `ClientMetrics`, installed as
  interceptors on the operator gRPC connection in `darepod.dialServer`.
- **`RegisterAll`** — registers event-driven collectors; tolerates duplicate
  registration (multiple daemons in one test process).

## Relationships

- **Depends on**: `baselib/actor` (framework, sink), `btclog`,
  `lnd/fn/v2`, `client_golang/prometheus`,
  `go-grpc-middleware/providers/prometheus`.
- **Depended on by**: `darepod` (config field, server start/stop, collector
  adapter, actor spawn, emission sites, gRPC interceptors).

## Invariants

- All event-driven updates go through `MetricsActor.Receive`.
- Scrape-driven (VTXO) gauges are collected by `SystemCollector` at scrape
  time, not the actor.
- The metrics server is **opt-in**: empty `ServerConfig.ListenAddr` disables
  everything (no HTTP server, no actor spawn, no collector registration).
- `RegisterAll` uses `Register` + `AlreadyRegisteredError`, not `MustRegister`.
- Non-counter metrics must not use the `_total` suffix (promlinter enforced).
- Emission never blocks or fails the operation being recorded:
  `Server.emitMetric` Tells through an `fn.Option[Sink]` and only debug-logs a
  Tell error.

## Deep Docs

- [README.md](README.md) — Full metrics reference table (names, types, labels,
  status enum values) for building Grafana dashboards.
