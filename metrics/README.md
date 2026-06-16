# metrics — darepod Prometheus Instrumentation

All darepo **client** (`darepod`) Prometheus metrics are namespaced under
`darepod_`. The namespace matches the daemon binary name and the `DAREPOD`
config/env prefix, so client metrics group under the same identifier operators
already use. This mirrors the arkd **server** metrics package (`arkd_`), one
directory up, in both structure and collection strategy.

Two collection strategies, same as the server:

1. **Event-driven** (`MetricsActor`) — subsystem call sites and the daemon
   `Tell` typed messages to a single metrics actor, which owns every
   lifecycle counter. No site touches Prometheus directly.
2. **Scrape-driven** (`SystemCollector`) — implements `prometheus.Collector`
   and queries the VTXO store on each scrape, so balance/inventory gauges are
   always fresh without a background ticker.

The `/metrics` HTTP endpoint is **opt-in / disabled by default**. It starts
only when a listen address is configured.

## Configuration

| Flag | Config key | Env | Default | Description |
|------|-----------|-----|---------|-------------|
| `--metrics.listen` | `metrics.listen` | `DAREPOD_METRICS_LISTEN` | `""` (disabled) | Address for the Prometheus `/metrics` HTTP server (e.g. `127.0.0.1:9092`). Empty disables metrics. |

When disabled, no collectors are registered, the metrics actor is not spawned,
and the gRPC client interceptors accumulate samples that nobody scrapes (a
harmless no-op).

## Scrape-Driven Metrics (`SystemCollector`)

Populated on each scrape by the `systemStatsAdapter` in `darepod`, which reads
the VTXO store, the on-chain wallet backend, the chain backend, and the live
OOR/round actors. Each source is queried independently: one that is not ready
(e.g. wallet still locked, no chain backend) is skipped for that scrape rather
than failing the endpoint. Statuses/states with zero entries are omitted, so
label cardinality tracks live inventory.

| Metric | Type | Labels | Source | Description |
|--------|------|--------|--------|-------------|
| `darepod_vtxos` | gauge | `status` | scrape (VTXO store) | Number of VTXOs by status. |
| `darepod_vtxos_value_satoshis` | gauge | `status` | scrape (VTXO store) | Total VTXO value by status, in satoshis. |
| `darepod_spendable_balance_satoshis` | gauge | — | scrape (VTXO store) | Total value in satoshis of spendable (`live`) VTXOs. |
| `darepod_wallet_confirmed_satoshis` | gauge | — | scrape (wallet backend) | Confirmed on-chain wallet balance in satoshis (boarding deposits, change, swept outputs). |
| `darepod_wallet_unconfirmed_satoshis` | gauge | — | scrape (wallet backend) | Unconfirmed on-chain wallet balance in satoshis. |
| `darepod_block_height` | gauge | — | scrape (chain backend) | Best block height seen by the client's chain backend. |
| `darepod_oor_sessions_by_state` | gauge | `state` | scrape (OOR actor) | Currently-tracked (live) OOR sessions by state, e.g. `pending`. Lifetime totals live in `oor_transfers_*_total`. |
| `darepod_rounds_by_status` | gauge | `status` | scrape (round actor) | Currently-live rounds by status, e.g. `joined`, `confirmed`. Lifetime totals live in `rounds_*_total`. |

The on-chain wallet balance complements the off-chain VTXO value: together they
give a full picture of client funds. The `*_by_state` / `*_by_status` gauges
read only the **live** actors (cheap, bounded), so they answer "what is in
flight / stuck right now," while the cumulative `_total` counters track lifetime
history.

### `status` label values

These are the exact `vtxo.VTXOStatus.String()` values (use them verbatim in
PromQL):

| Label value | Enum |
|-------------|------|
| `live` | `VTXOStatusLive` (spendable) |
| `pending_forfeit` | `VTXOStatusPendingForfeit` |
| `forfeiting` | `VTXOStatusForfeiting` |
| `forfeited` | `VTXOStatusForfeited` |
| `spent` | `VTXOStatusSpent` |
| `unilateral_exit` | `VTXOStatusUnilateralExit` |
| `failed` | `VTXOStatusFailed` |
| `spending` | `VTXOStatusSpending` |

`darepod_spendable_balance_satoshis` sums only the `live` status.

## Event-Driven Metrics (`MetricsActor`)

Updated when the daemon `Tell`s the metrics actor through the `Sink`. All are
counters. Outcomes are observed at the `darepod` RPC / event-routing boundary,
so they reflect **submission/acceptance** outcomes, not asynchronous on-chain
settlement confirmation.

| Metric | Type | Labels | Source | Description |
|--------|------|--------|--------|-------------|
| `darepod_rounds_joined_total` | counter | — | event (`JoinNextRound` RPC) | Rounds the client attempted to join. |
| `darepod_rounds_completed_total` | counter | `status` | event (round actor) | Settlement rounds completed by outcome. `status`: `confirmed`, `failed`. |
| `darepod_oor_transfers_sent_total` | counter | `status` | event (`SendOOR` RPC) | Outgoing out-of-round transfers by outcome. `status`: `submitted`, `failed`. |
| `darepod_oor_transfer_duration_seconds` | histogram | `status` | event (`SendOOR` RPC) | Wall-clock duration of outgoing OOR transfers from `SendOOR` entry to terminal outcome, by `status`. Measured at the call site; idempotent replays are not observed. |
| `darepod_oor_transfers_received_total` | counter | `status` | event (incoming VTXO handler) | Incoming out-of-round transfers by outcome. `status`: `materialized` (persisted), `failed` (relevant receive that could not be persisted, or a malformed push at the routing boundary). |
| `darepod_boarding_events_total` | counter | `status` | event (`Board` RPC) | Boarding (on-chain → VTXO) events by outcome. `status`: `submitted`, `skipped`, `failed`. |
| `darepod_background_task_errors_total` | counter | `task` | event (subsystem actors) | Background-task errors by task name. Current tasks: `boarding_sweep_watcher`, `server_grpc_listen`. |

> Emission seams: `rounds_completed_total` is emitted by the `round` actor as
> each round reaches `ConfirmedState` (`confirmed`) or `ClientFailedState`
> (`failed`) — terminal outcomes surface in the actor's
> `RoundCompletedNotification` / `RoundFailedNotification` handlers, with no RPC
> boundary to observe them. Both the `round` actor and the `wallet` actor hold
> an optional `metrics.Sink` threaded in the same way `ledger.Sink` already is;
> when metrics are disabled the sink is `None` and emission is a no-op.

## Sync / Liveness Gauges

Set directly by the daemon (not the actor). The bootstrap point stamps both on
the first successful direct `GetInfo`; a connection watcher
(`monitorOperatorConnection`) then keeps them live for the daemon's lifetime by
polling the direct gRPC connection's transport state every 15s.

| Metric | Type | Labels | Source | Description |
|--------|------|--------|--------|-------------|
| `darepod_server_connection_up` | gauge | — | daemon (connection watcher) | `1` when the direct gRPC connection to the ark operator is `Ready`, `0` otherwise (transient failure, idle, shutdown). |
| `darepod_server_sync_timestamp_seconds` | gauge | — | daemon (connection watcher) | Unix timestamp of the last poll that found the operator connection `Ready`. A stale value signals lost contact. |

## gRPC Client Metrics

Per-method **client-side** metrics for calls `darepod` makes to the ark
operator, via `go-grpc-middleware/providers/prometheus` `ClientMetrics`,
installed as unary + stream interceptors on the operator connection
(`dialServer`). Namespaced under `darepod_`.

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `darepod_grpc_client_started_total` | counter | `grpc_type`, `grpc_service`, `grpc_method` | Client RPCs started. |
| `darepod_grpc_client_handled_total` | counter | `grpc_type`, `grpc_service`, `grpc_method`, `grpc_code` | Client RPCs completed, by status code. |
| `darepod_grpc_client_msg_received_total` | counter | `grpc_type`, `grpc_service`, `grpc_method` | Stream messages received from the operator. |
| `darepod_grpc_client_msg_sent_total` | counter | `grpc_type`, `grpc_service`, `grpc_method` | Stream messages sent to the operator. |
| `darepod_grpc_client_handling_seconds` | histogram | `grpc_type`, `grpc_service`, `grpc_method` | Client-observed RPC latency (request → response). Buckets: exponential from 1ms, 16 buckets. |

(The exact `grpc_client_*` metric names are produced by the middleware; the
`darepod_` namespace prefix is applied.)

## Adding New Metrics

- **Event-driven**: add a message type to `messages.go`, handle it in
  `actor.go:Receive`, define the metric in `metrics.go`, register it in
  `allCollectors()`, and `Tell` it from the call site via `Server.emitMetric`.
- **Scrape-driven**: add a method to `SystemStatsQuerier` in `collector.go`,
  implement it in the `darepod` `systemStatsAdapter`, add a descriptor, and emit
  it from a `Collect` sub-method (each group is queried independently so a
  not-ready source skips only its own gauges) plus `Describe`.
