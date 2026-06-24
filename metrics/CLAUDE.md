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
   queries client system state on each scrape (VTXO inventory, on-chain wallet
   balance, chain tip, live OOR/round actor state) so balance/inventory/health
   gauges stay fresh without a ticker. Each source is collected independently;
   a not-ready source skips only its own gauges.

Also provides the opt-in HTTP `/metrics` server and the shared
`GRPCClientMetrics` for client-side gRPC interceptors.

## Key Concepts

Use `go doc metrics.<Symbol>` for signatures.

- **`MetricsActor`** / **`ActorConfig`** — event-driven counters. The daemon
  spawns a small **pool** of these (`metricsActorWorkers`, currently 4) all
  registered under `ActorKey`. The actor is stateless (it only `Inc()`s
  concurrency-safe Prometheus counters), so workers drain events in parallel.
- **`Sink`** / **`NewSink`** — fire-and-forget reference, resolved from the
  actor system via the service key. `ActorKey.Ref` returns a **round-robin
  router** over every actor registered under the key, so a single `Sink` fans
  Tells across the worker pool with no change at producer call sites. Mirrors
  `ledger.Sink`.
- **`SystemCollector`** + **`SystemStatsQuerier`** — scrape-driven gauges
  (VTXO inventory + value, on-chain `wallet_confirmed/unconfirmed_satoshis`,
  `block_height`, live `oor_sessions_by_state`, live `rounds_by_status`). The
  `darepod` `systemStatsAdapter` implements the querier, delegating to the VTXO
  store, the wallet backend, the chain backend, and the live OOR/round actors.
  The `*_by_state`/`*_by_status` gauges read only live actors (bounded);
  lifetime totals live in the `_total` counters.
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
  adapter, actor-pool spawn, emission sites, connection watcher, gRPC
  interceptors), `round` (`RoundClientConfig.MetricsSink`, emits
  `RoundCompletedMsg` at terminal round outcomes), `wallet`
  (`WithMetricsSink`, emits `BackgroundTaskErrorMsg` from the boarding-sweep
  watcher), `vtxo` (`IncomingVTXOHandlerConfig.MetricsSink`, emits
  `OORTransferReceivedMsg` from the incoming-VTXO materialization path).

## Invariants

- All event-driven updates go through `MetricsActor.Receive`. The actor holds
  no mutable state (counters live in package-level Prometheus vectors), so the
  worker pool is safe: any worker may handle any message.
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
