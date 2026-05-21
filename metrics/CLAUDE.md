# metrics

## Purpose

Centralized Prometheus instrumentation for the arkd operator daemon. The
package has two collection strategies:

1. **Event-driven** — The `MetricsActor` receives typed fire-and-forget
   messages from subsystem actors (rounds, OOR, clientconn) and updates
   counters, gauges, and histograms. All mutable aggregation state (round
   timers, phase durations, client counts, OOR session lifetimes) lives
   exclusively in the actor.

2. **Scrape-driven** — The `SystemCollector` implements `prometheus.Collector`
   and queries the database and LND wallet on each Prometheus scrape. This
   keeps DB-derived gauges (VTXO stats, round counts, OOR session counts,
   wallet balance) always fresh without a periodic ticker.

The package also provides the HTTP `/metrics` scrape endpoint and an
`InstrumentedLocker` decorator for transparent VTXO lock timing.

## Key Types

- `MetricsActor` — Actor for event-driven metrics (counters, histograms,
  event-driven gauges like `RoundsActive` and `ConnectedClients`).
- `SystemCollector` — `prometheus.Collector` for scrape-driven DB/wallet
  gauges (VTXOs, rounds, OOR sessions, wallet balance).
- `SystemStatsQuerier` — Interface for scrape-time data sources (DB +
  wallet). Implemented by `systemStatsAdapter` in the root package.
- `Msg` — Sealed message interface for metric events sent to the actor.
- `Server` — HTTP server exposing the `/metrics` Prometheus scrape endpoint,
  with configurable timeouts via functional options. **Opt-in by default**:
  the metrics server only starts when explicitly enabled via config/CLI flag.
- `ServerConfig` — Configuration for the HTTP server (listen address, logger).
- `InstrumentedLocker` — Decorator wrapping `vtxo.Locker` with transparent
  lock-duration and failure reporting to the metrics actor.
- `GRPCServerMetrics` — Shared `go-grpc-middleware/providers/prometheus`
  instance for per-method gRPC request counting and handling time histograms.
- `RoundTickFiredMsg` — Fire-and-forget actor message sent by the rounds actor
  on every periodic `TickEvent` fire. Carries `RoundID` and `Result` (one of
  `"sealed"`, `"skipped_empty"`, `"skipped_predicate"`). The metrics actor
  increments `RoundTicksTotal` with the `result` label.
- `RoundTicksTotal` — `prometheus.CounterVec` labelled by `result` counting
  periodic round-tick outcomes. Operators alert on a sustained
  `skipped_empty` rate to detect stuck rounds.
- `RoundChangeRequiredForBoardingTotal` — Counter incremented when LND's
  `FundPsbt` produces no change output while boarding inputs are present (the
  witness-weight delta cannot be applied). Sustained rate indicates an operator
  hot-wallet liquidity gap; each increment means a boarding round proceeded
  with overpay to miners rather than failing.
- `BatchWatcherRegisterFailedMsg` — Fire-and-forget actor message sent by the
  rounds actor when `RegisterBatchRequest` fails to enqueue with the batch
  watcher at round confirmation. Carries `RoundID string` and `BatchCount int`.
- `BatchWatcherRegisterFailures` — `prometheus.Counter` incremented by
  `BatchWatcherRegisterFailedMsg`. **Operator must alert on any non-zero rate.**
  Batches that failed registration are NOT monitored on-chain; there is no
  automatic redrive. A non-zero value means affected VTXOs may be swept by
  the operator without fraud detection.

## Relationships

- **Depends on**: `vtxo` (Locker interface for decorator), `actor` (framework
  for ActorBehavior/TellOnlyRef), `btclog` (structured logging),
  `go-grpc-middleware/providers/prometheus` (gRPC server metrics).
- **Depended on by**: root `darepo` (server wiring, adapter), `rounds` (sends
  round lifecycle events), `oor` (sends transfer events), `clientconn` (sends
  dispatch latency events), `adminrpcserver`/`rpcserver` (gRPC interceptors).
- **Messages to/from**:
  - Receives `RoundCreatedMsg` <- `rounds`
  - Receives `ClientJoinedRoundMsg` <- `rounds`
  - Receives `RoundSealedMsg` <- `rounds`
  - Receives `PhaseStartedMsg`/`PhaseEndedMsg` <- `rounds`
  - Receives `RoundTickFiredMsg` <- `rounds` (periodic tick outcome)
  - Receives `RoundCompletedMsg` <- `rounds`
  - Receives `OORTransferStartedMsg`/`OORTransferCompletedMsg` <- `oor`
  - Receives `VTXOLockResultMsg` <- `InstrumentedLocker` (wrapping vtxo.Locker)
  - Receives `DispatchCompletedMsg` <- `clientconn` ingress loop
  - Receives `ClientStatusChangedMsg` <- root `darepo` (status tracker callback)
  - Receives `BatchWatcherRegisterFailedMsg` <- `rounds` (batch watcher enqueue failure at confirmation)

## Invariants

- All event-driven Prometheus updates go through `MetricsActor.Receive`; no
  subsystem may call Prometheus directly for counters or histograms.
- DB-derived gauges are collected by `SystemCollector` at scrape time, not
  by the actor. The collector queries the DB independently.
- `RegisterAll` must tolerate duplicate registration (integration tests run
  multiple servers in one process). It uses `Register` + `AlreadyRegisteredError`
  check instead of `MustRegister`.
- The `InstrumentedLocker` is safe to use before the metrics actor is spawned;
  `SetMetricsRef` can be called later and the decorator silently skips reporting
  until then.
- Non-counter metrics must not use the `_total` suffix (promlinter enforced).

## Deep Docs

- [README.md](README.md) — Full metrics reference table
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map
