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

Populated by querying the client VTXO store on each scrape. The querier
(`vtxoStatsAdapter` in `darepod`) lists VTXOs per status and aggregates the
count and summed value. Statuses with zero VTXOs are omitted from a scrape, so
label cardinality tracks live inventory.

| Metric | Type | Labels | Source | Description |
|--------|------|--------|--------|-------------|
| `darepod_vtxos` | gauge | `status` | scrape (VTXO store) | Number of VTXOs by status. |
| `darepod_vtxos_value_satoshis` | gauge | `status` | scrape (VTXO store) | Total VTXO value by status, in satoshis. |
| `darepod_spendable_balance_satoshis` | gauge | — | scrape (VTXO store) | Total value in satoshis of spendable (`live`) VTXOs. |

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
| `darepod_rounds_completed_total` | counter | `status` | event | Settlement rounds completed by outcome. `status`: `confirmed`, `failed`. |
| `darepod_oor_transfers_sent_total` | counter | `status` | event (`SendOOR` RPC) | Outgoing out-of-round transfers by outcome. `status`: `submitted`, `failed`. |
| `darepod_oor_transfers_received_total` | counter | `status` | event (incoming VTXO route) | Incoming out-of-round transfers by outcome. `status`: `materialized`, `failed`. |
| `darepod_boarding_events_total` | counter | `status` | event (`Board` RPC) | Boarding (on-chain → VTXO) events by outcome. `status`: `submitted`, `skipped`, `failed`. |
| `darepod_background_task_errors_total` | counter | `task` | event | Background-task errors by task name. |

> Note on `rounds_completed_total`: the `RoundCompletedMsg` message type and
> counter exist and are driven through the actor, but the client currently has
> no darepod-local seam observing terminal round confirmation/failure (that
> lives in the `round` subsystem FSM). Wiring it requires threading the
> `metrics.Sink` into the `round` package, the same way the arkd server plumbs
> its sink into subsystem actors. Until then this counter stays at zero.

## Sync / Liveness Gauges

Set directly by the daemon (not the actor) at the operator-connection point.

| Metric | Type | Labels | Source | Description |
|--------|------|--------|--------|-------------|
| `darepod_server_connection_up` | gauge | — | daemon | `1` when the direct gRPC connection to the ark operator is up, `0` otherwise. |
| `darepod_server_sync_timestamp_seconds` | gauge | — | daemon | Unix timestamp of the last successful sync (direct `GetInfo`) with the operator. |

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
- **Scrape-driven**: add a method to `VTXOStatsQuerier` (or a new querier) in
  `collector.go`, implement it in the `darepod` adapter, add a descriptor and
  emit it from `Collect`/`Describe`.
