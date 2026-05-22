# metrics

## Purpose

Centralized Prometheus instrumentation for the arkd operator. Two collection
strategies:

1. **Event-driven** — `MetricsActor` receives typed fire-and-forget messages
   from subsystem actors (rounds, OOR, clientconn) and updates counters,
   gauges, and histograms. All mutable aggregation state lives in the actor.
2. **Scrape-driven** — `SystemCollector` implements `prometheus.Collector`
   and queries DB + LND wallet on each Prometheus scrape so DB-derived gauges
   stay fresh without a periodic ticker.

Also provides the HTTP `/metrics` endpoint and `InstrumentedLocker` for
transparent VTXO lock timing.

## Key Concepts

Use `go doc metrics.<Symbol>` for signatures.

- **`MetricsActor`** — Event-driven counters/histograms + event-driven
  gauges (`RoundsActive`, `ConnectedClients`).
- **`SystemCollector`** + **`SystemStatsQuerier`** — Scrape-driven DB/wallet
  gauges (VTXOs, rounds, OOR sessions, wallet balance).
  `systemStatsAdapter` (root) implements the querier.
- **`Server`** / **`ServerConfig`** — HTTP scrape endpoint, configurable
  timeouts. **Opt-in by default** — starts only when explicitly enabled.
- **`InstrumentedLocker`** — Decorates `vtxo.Locker` with lock-duration
  and failure reporting.
- **`GRPCServerMetrics`** — Shared
  `go-grpc-middleware/providers/prometheus` instance for gRPC per-method
  request counts + handling time.
- **Operator-alert counters**:
  - `RoundTicksTotal` (`result` label: `sealed`, `skipped_empty`,
    `skipped_predicate`) — sustained `skipped_empty` rate flags stuck
    rounds.
  - `RoundChangeRequiredForBoardingTotal` — incremented when `FundPsbt`
    produces no change for a boarding round (witness-weight delta
    couldn't apply; round overpaid miners rather than failing).
    Sustained rate means hot-wallet liquidity gap.
  - `BatchWatcherRegisterFailures` — **alert on any non-zero rate**.
    Affected batches are not monitored on-chain; no automatic redrive;
    affected VTXOs may be swept without fraud detection.

## Relationships

- **Depends on**: `vtxo` (Locker interface), `actor` (framework),
  `btclog`, `go-grpc-middleware/providers/prometheus`.
- **Depended on by**: root `darepo` (wiring, adapter), `rounds`, `oor`,
  `clientconn`, `adminrpcserver`/`rpcserver` (interceptors).
- **Messages** (all fire-and-forget Tell):
  - ← `rounds`: `RoundCreatedMsg`, `ClientJoinedRoundMsg`,
    `RoundSealedMsg`, `PhaseStartedMsg`/`PhaseEndedMsg`,
    `RoundTickFiredMsg`, `RoundCompletedMsg`,
    `BatchWatcherRegisterFailedMsg`.
  - ← `oor`: `OORTransferStartedMsg`/`OORTransferCompletedMsg`.
  - ← `InstrumentedLocker`: `VTXOLockResultMsg`.
  - ← `clientconn` ingress: `DispatchCompletedMsg`.
  - ← root `darepo`: `ClientStatusChangedMsg`.

## Invariants

- All event-driven Prometheus updates go through `MetricsActor.Receive`;
  no subsystem calls Prometheus directly.
- DB-derived gauges are collected by `SystemCollector` at scrape time,
  not the actor.
- `RegisterAll` tolerates duplicate registration (integration tests run
  multiple servers in one process) — uses
  `Register` + `AlreadyRegisteredError` check, not `MustRegister`.
- `InstrumentedLocker` is safe to use before the metrics actor is spawned;
  `SetMetricsRef` can be called later and the decorator silently skips
  reporting until then.
- Non-counter metrics must not use the `_total` suffix (promlinter
  enforced).

## Deep Docs

- [README.md](README.md) — Full metrics reference table.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide map.
