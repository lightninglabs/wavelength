# metrics

## Purpose

Prometheus instrumentation for the wavelength daemon (`waved`). All
metrics are namespaced under `waved_`. Mirrors the arkd **server** metrics
package (one directory up) in structure and collection strategy: an
event-driven actor for lifecycle counters, plus a scrape-time collector for
gauges that must stay fresh (VTXO inventory, wallet balance, chain tip, live
OOR/round state).

## Key Types

- `MetricsActor` / `ActorConfig` — event-driven counters. `waved` spawns a
  small pool of these (`metricsActorWorkers = 4`, in `waved/metrics.go`)
  registered under `ActorKey`; the actor holds no mutable state, so any
  worker can handle any message.
- `Msg` — sealed interface for actor messages: `BoardingEventMsg`,
  `RoundJoinedMsg`, `RoundCompletedMsg`, `OORTransferReceivedMsg`,
  `OORTransferSentMsg`, `BackgroundTaskErrorMsg`.
- `Sink` (`= actor.TellOnlyRef[Msg]`) / `NewSink` — fire-and-forget handle
  resolved from the actor system via `ActorKey`, which round-robins Tells
  across the worker pool.
- `SystemCollector` / `SystemStatsQuerier` — `prometheus.Collector` that
  queries live client state on each scrape (VTXO inventory/value, wallet
  balance, block height, `oor_sessions_by_state`, `rounds_by_status`). Each
  querier method is collected independently; an error only suppresses that
  method's gauges for the scrape.
- `Server` / `ServerConfig` — opt-in HTTP `/metrics` endpoint; disabled
  unless `ServerConfig.ListenAddr` is set.
- `GRPCClientMetrics` — shared `go-grpc-middleware/providers/prometheus`
  `ClientMetrics`, installed as interceptors on the operator gRPC connection.

## Relationships

- **Depends on**: `baselib/actor` (actor framework, `Sink`), `btclog`,
  `lnd/fn/v2`, `client_golang/prometheus`,
  `go-grpc-middleware/providers/prometheus`.
- **Depended on by**: `waved` (spawns the actor pool, owns `SystemCollector`
  adapter and HTTP server, emits `BoardingEventMsg`/`OORTransferSentMsg` etc.
  directly), `round` (`RoundClientConfig.MetricsSink` emits
  `RoundJoinedMsg`/`RoundCompletedMsg`), `wallet` (`WithMetricsSink` emits
  `BackgroundTaskErrorMsg` from the boarding-sweep watcher), `vtxo`
  (`IncomingVTXOHandlerConfig.MetricsSink` emits `OORTransferReceivedMsg`).
- **Messages to/from**: Receives `Msg` (`BoardingEventMsg`, `RoundJoinedMsg`,
  `RoundCompletedMsg`, `OORTransferReceivedMsg`, `OORTransferSentMsg`,
  `BackgroundTaskErrorMsg`) <- `round`, `wallet`, `vtxo`, `waved`.

## Invariants

- All event-driven updates go through `MetricsActor.Receive`; counters live
  in package-level Prometheus vectors, not actor state, so the worker pool is
  safe for concurrent delivery.
- Scrape-driven gauges are collected by `SystemCollector` at scrape time, not
  by the actor.
- The metrics server is opt-in: empty `ServerConfig.ListenAddr` disables the
  HTTP server, actor spawn, and collector registration.
- `RegisterAll` uses `Register` + tolerates `AlreadyRegisteredError`, not
  `MustRegister` (multiple daemons may share a process in tests).
- Non-counter metrics must not use the `_total` suffix (promlinter enforced).
- Emission never blocks or fails the operation being recorded: emit sites
  Tell through an `fn.Option[Sink]` and only debug-log a Tell error.

## Deep Docs

- [README.md](README.md) — Full metrics reference table (names, types,
  labels, status enum values) for building Grafana dashboards.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
