# metrics

## Purpose

Prometheus instrumentation for the wavelength daemon (`waved`). All
metrics are namespaced under `waved_`. Mirrors the lumosd **server** metrics
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
  `OORTransferSentMsg`, `ActorDeadLetterMsg`, `DeliveryPostponeMsg`,
  `BackgroundTaskErrorMsg`.
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
- `WithDeadLetterCounter` — wraps an `actor.TellRetryPolicy` so every
  delivery the policy gives up on increments `DeliveryDeadLettersTotal` for a
  coarse actor family (`ActorFamilyCreditOp`, `ActorFamilyIncomingVTXO`). A
  policy returning `false` is exactly the framework's dead-letter decision, so
  the wrapper instruments the call sites that own a sink without teaching the
  actor framework about metrics. It is a lower bound: attempt-ceiling and
  poison-message dead letters never consult the policy.
- `EmitDeliveryPostpone` — the one path a durable behavior reports a postpone
  or expired-horizon decision through, so the counters mean the same thing in
  every subsystem. Best-effort: an unconfigured sink does nothing and a Tell
  failure is only logged.
- `DeliveryPostponedTotal` / `DeliveryPostponeExpiredTotal` — postpone
  decisions and expired-horizon decisions, one per redrive rather than one per
  delivery: a delivery that keeps waiting counts once on every redrive, and
  past its behavior-owned horizon each attempt of the ordinary retry policy
  counts once more, so one abandoned delivery shows up several times.
  Labelled by coarse `PostponeCause` (`missing`, `chain-unconfirmed`,
  `indexer-error`, `server-error`, else `other`). The horizon is per cause, not
  per family: `vtxo` bounds an indexer wait at ten minutes while commitment
  confirmation waits have no wall-clock deadline. A slow block cannot prove a
  receive is invalid. These counters report retry decisions, not elapsed time
  or progress toward a deadline.

## Relationships

- **Depends on**: `baselib/actor` (actor framework, `Sink`), `btclog`,
  `lnd/fn/v2`, `client_golang/prometheus`,
  `go-grpc-middleware/providers/prometheus`.
- **Depended on by**: `waved` (spawns the actor pool, owns `SystemCollector`
  adapter and HTTP server, emits `BoardingEventMsg`/`OORTransferSentMsg` etc.
  directly), `round` (`RoundClientConfig.MetricsSink` emits
  `RoundJoinedMsg`/`RoundCompletedMsg`), `wallet` (`WithMetricsSink` emits
  `BackgroundTaskErrorMsg` from the boarding-sweep watcher), `vtxo`
  (`IncomingVTXOHandlerConfig.MetricsSink` emits `OORTransferReceivedMsg`,
  `DeliveryPostponeMsg`, and wraps its Tell retry policy with
  `WithDeadLetterCounter`), `credit` (`RegistryConfig.MetricsSink` reaches
  every per-operation child, which emits `DeliveryPostponeMsg` and wraps its
  Tell retry policy with `WithDeadLetterCounter`).
- **Messages to/from**: Receives `Msg` (`BoardingEventMsg`, `RoundJoinedMsg`,
  `RoundCompletedMsg`, `OORTransferReceivedMsg`, `OORTransferSentMsg`,
  `ActorDeadLetterMsg`, `DeliveryPostponeMsg`, `BackgroundTaskErrorMsg`) <-
  `round`, `wallet`, `vtxo`, `credit`, `waved`.

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
- Metric labels must come from a closed set: `normalizeActorFamily` and
  `normalizePostponeCause` bucket anything unrecognized, so a caller-supplied
  identifier can never become a label value.
- Emission never blocks or fails the operation being recorded: emit sites
  Tell through an `fn.Option[Sink]` and only debug-log a Tell error.

## Deep Docs

- [README.md](README.md) — Full metrics reference table (names, types,
  labels, status enum values) for building Grafana dashboards.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
