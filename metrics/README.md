# metrics — waved Prometheus Instrumentation

All wavelength **client** (`waved`) Prometheus metrics are namespaced under
`waved_`. The namespace matches the daemon binary name and the `WAVED`
config/env prefix, so client metrics group under the same identifier operators
already use. This mirrors the lumosd **server** metrics package (`lumosd_`), one
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
| `--metrics.listen` | `metrics.listen` | `WAVED_METRICS_LISTEN` | `""` (disabled) | Address for the Prometheus `/metrics` HTTP server (e.g. `127.0.0.1:9092`). Empty disables metrics. |

When disabled, no collectors are registered, the metrics actor is not spawned,
and the gRPC client interceptors accumulate samples that nobody scrapes (a
harmless no-op).

## Scrape-Driven Metrics (`SystemCollector`)

Populated on each scrape by the `systemStatsAdapter` in `waved`, which reads
the VTXO store, the on-chain wallet backend, the chain backend, and the live
OOR/round actors. Each source is queried independently: one that is not ready
(e.g. wallet still locked, no chain backend) is skipped for that scrape rather
than failing the endpoint. Statuses/states with zero entries are omitted, so
label cardinality tracks live inventory.

| Metric | Type | Labels | Source | Description |
|--------|------|--------|--------|-------------|
| `waved_vtxos` | gauge | `status` | scrape (VTXO store) | Number of VTXOs by status. |
| `waved_vtxos_value_satoshis` | gauge | `status` | scrape (VTXO store) | Total VTXO value by status, in satoshis. |
| `waved_spendable_balance_satoshis` | gauge | — | scrape (VTXO store) | Total value in satoshis of spendable (`live`) VTXOs. |
| `waved_wallet_confirmed_satoshis` | gauge | — | scrape (wallet backend) | Confirmed on-chain wallet balance in satoshis (boarding deposits, change, swept outputs). |
| `waved_wallet_unconfirmed_satoshis` | gauge | — | scrape (wallet backend) | Unconfirmed on-chain wallet balance in satoshis. |
| `waved_block_height` | gauge | — | scrape (chain backend) | Best block height seen by the client's chain backend. |
| `waved_oor_sessions_by_state` | gauge | `state` | scrape (OOR actor) | Currently-tracked (live) OOR sessions by state, e.g. `pending`. Lifetime totals live in `oor_transfers_*_total`. |
| `waved_rounds_by_status` | gauge | `status` | scrape (round actor) | Currently-live rounds by status, e.g. `joined`, `confirmed`. Lifetime totals live in `rounds_*_total`. |
| `waved_mailbox_backlog` | gauge | — | scrape (delivery store) | Total messages pending across all durable mailboxes. Emits an explicit `0` when every mailbox is drained, so "all clear" and "scrape broke" are distinguishable. |
| `waved_mailbox_depth` | gauge | `mailbox_id` | scrape (delivery store) | Pending messages in one durable mailbox, leased (in-flight) rows included. Only mailboxes currently holding messages are reported, so per-session actor IDs never accumulate as permanent series. A mailbox that sits near its hard watermark (default 10000) is refusing new sends with `ErrMailboxSaturated`; the soft watermark (default 1000) logs a warning first. |

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

`waved_spendable_balance_satoshis` sums only the `live` status.

## Event-Driven Metrics (`MetricsActor`)

Updated when the daemon `Tell`s the metrics actor through the `Sink`. All are
counters. Outcomes are observed at the `waved` RPC / event-routing boundary,
so they reflect **submission/acceptance** outcomes, not asynchronous on-chain
settlement confirmation.

| Metric | Type | Labels | Source | Description |
|--------|------|--------|--------|-------------|
| `waved_rounds_joined_total` | counter | — | event (round actor) | Rounds the client attempted to join. Emitted from the round actor's `createNewRound`, so it counts both manual `JoinNextRound` and eager/automatic joins — symmetric with `rounds_completed_total`. |
| `waved_rounds_completed_total` | counter | `status` | event (round actor) | Settlement rounds completed by outcome. `status`: `confirmed`, `failed`. |
| `waved_oor_transfers_sent_total` | counter | `status` | event (`SendOOR` RPC) | Outgoing out-of-round transfers by outcome. `status`: `submitted`, `failed`. |
| `waved_oor_transfer_duration_seconds` | histogram | `status` | event (`SendOOR` RPC) | Wall-clock duration of outgoing OOR transfers from `SendOOR` entry to terminal outcome, by `status`. Measured at the call site; idempotent replays are not observed. |
| `waved_oor_transfers_received_total` | counter | `status` | event (incoming VTXO handler) | Incoming out-of-round transfers by outcome. `status`: `materialized` (persisted), `failed` (relevant receive that could not be persisted, or a malformed push at the routing boundary). |
| `waved_boarding_events_total` | counter | `status` | event (`Board` RPC) | Boarding (on-chain → VTXO) events by outcome. `status`: `submitted`, `skipped`, `failed`. |
| `waved_background_task_errors_total` | counter | `task` | event (subsystem actors) | Background-task errors by task name. Current tasks: `boarding_sweep_watcher`, `server_grpc_listen`. |

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
| `waved_server_connection_up` | gauge | — | daemon (connection watcher) | `1` when the direct gRPC connection to the ark operator is `Ready`, `0` otherwise (transient failure, idle, shutdown). |
| `waved_server_sync_timestamp_seconds` | gauge | — | daemon (connection watcher) | Unix timestamp of the last poll that observed the operator connection in the `Ready` **transport** state. This is transport liveness, not a completed application round-trip — an idle-but-`Ready` link keeps the stamp fresh. A stale value signals lost transport contact. |

## Ingress Liveness (`serverconn`)

Set directly by the connector's ingress loop, for the same reason the two gauges
above are set by the daemon: the connector owns no metrics sink, and the ingress
goroutine is precisely the one that must not take a new actor edge to report that
it is alive.

The loop is the process's only consumer of the server mailbox, so when it stalls
the client goes deaf to the operator while read-only RPCs keep answering — a pod
that looks healthy and has stopped hearing about rounds. Nothing else in the
process observes that: the connector's other timestamp is outbound-only, and
`GetInfo`'s `server_connected` is stamped once at startup and never cleared on a
stall.

| Metric | Type | Labels | Source | Description |
|--------|------|--------|--------|-------------|
| `waved_serverconn_last_ingress_poll_timestamp_seconds` | gauge | — | connector (ingress loop) | Unix timestamp of the last `Pull` that returned to the ingress loop, including an empty long-poll. Fresh at the long-poll cadence on an idle client, so staleness means the loop goroutine itself is gone. |
| `waved_serverconn_last_ingress_event_timestamp_seconds` | gauge | — | connector (ingress loop) | Unix timestamp of the last pulled batch the loop delivered and committed, including a partial commit made while backpressure held the rest. Only advances on real traffic, so on its own it cannot tell an idle client from a wedged one; a fresh poll stamp with a stale event stamp says the loop is running but dispatch is not getting through. |
| `waved_serverconn_ingress_dispatch_deferred_total` | counter | `service`, `method` | connector (ingress loop) | Redrives a target actor turned away for want of room — a full in-memory mailbox, or a durable mailbox past its hard backlog watermark — by the route of the envelope that was refused. One increment per re-pull that could not deliver, **not** one per queued envelope: the loop meets the refusal once and stops there, so the envelopes behind the first are never attempted. Nothing is lost — the refused envelope is unacknowledged and re-pulled after a short backoff — but a rate that does not fall back to zero means a local actor has stopped draining. Cross-reference `waved_mailbox_depth` to tell the two refusals apart: a durable refusal shows the target's mailbox pinned at its hard watermark. |

### Alerting

There are two distinct failures here and they need different expressions.

**The loop is gone** (panic, or a park somewhere the fix below does not cover):

```
time() - waved_serverconn_last_ingress_poll_timestamp_seconds > 300
  and waved_server_connection_up == 1
```

**A local actor has stopped draining**, which is the wedge this instrumentation
was added for. Poll staleness cannot see it: the loop keeps polling and keeps
this gauge fresh the whole time the target is wedged, deliberately, so that the
two failures stay separable. Key on the deferral counter instead:

```
rate(waved_serverconn_ingress_dispatch_deferred_total[10m]) > 0
```

for 5m, which distinguishes a target that briefly filled up (deferrals stop) from
one that has stopped entirely (they do not). The corroborating reading is a
**fresh** poll stamp with a **stale** event stamp:

```
time() - waved_serverconn_last_ingress_poll_timestamp_seconds < 60
  and time() - waved_serverconn_last_ingress_event_timestamp_seconds > 300
```

That pair is subject to false positives on a genuinely idle client, so it is a
diagnostic to read next to the counter rather than a page on its own.

Both gauges are unlabelled, which assumes one connector per process — true today
(`waved/server.go` constructs exactly one). A second one would silently make each
gauge the max of the two and the first alert blind; add a `mailbox_id` label
before that happens.

**A durable consumer is falling behind.** The depth gauges see the backlog long
before the hard watermark starts refusing sends. A warning at the soft
watermark gives the operator the same head start the log line does:

```
waved_mailbox_depth > 1000
```

for 10m, which filters the transient burst a healthy consumer drains on its
own. A mailbox pinned at the hard watermark (default 10000) is actively
shedding load — new sends fail with `ErrMailboxSaturated` — and warrants a
page, because at that depth something downstream has stopped, not slowed.

## gRPC Client Metrics

## gRPC Client Metrics

Per-method **client-side** metrics for calls `waved` makes to the ark
operator, via `go-grpc-middleware/providers/prometheus` `ClientMetrics`,
installed as unary + stream interceptors on the operator connection
(`dialServer`). Namespaced under `waved_`.

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `waved_grpc_client_started_total` | counter | `grpc_type`, `grpc_service`, `grpc_method` | Client RPCs started. |
| `waved_grpc_client_handled_total` | counter | `grpc_type`, `grpc_service`, `grpc_method`, `grpc_code` | Client RPCs completed, by status code. |
| `waved_grpc_client_msg_received_total` | counter | `grpc_type`, `grpc_service`, `grpc_method` | Stream messages received from the operator. |
| `waved_grpc_client_msg_sent_total` | counter | `grpc_type`, `grpc_service`, `grpc_method` | Stream messages sent to the operator. |
| `waved_grpc_client_handling_seconds` | histogram | `grpc_type`, `grpc_service`, `grpc_method` | Client-observed RPC latency (request → response). Buckets: exponential from 1ms, 16 buckets. |

(The exact `grpc_client_*` metric names are produced by the middleware; the
`waved_` namespace prefix is applied.)

## Go Runtime / Process Metrics

The daemon serves metrics from a per-instance registry (not the global
`DefaultRegisterer`), so the standard `client_golang` Go runtime and process
collectors are explicitly re-registered on it in `startMetricsServer`. The
endpoint therefore still exposes the usual `go_*` (goroutines, GC, heap) and
`process_*` (CPU, resident memory, open FDs) series alongside the `waved_*`
metrics.

## Adding New Metrics

- **Event-driven**: add a message type to `messages.go`, handle it in
  `actor.go:Receive`, define the metric in `metrics.go`, register it in
  `allCollectors()`, and `Tell` it from the call site via `Server.emitMetric`.
  The exception is a metric that reports on the health of a goroutine which must
  not acquire new dependencies to do so (the liveness gauges above): those are
  defined in `metrics.go`, registered in `allCollectors()`, and set directly.
- **Scrape-driven**: add a method to `SystemStatsQuerier` in `collector.go`,
  implement it in the `waved` `systemStatsAdapter`, add a descriptor, and emit
  it from a `Collect` sub-method (each group is queried independently so a
  not-ready source skips only its own gauges) plus `Describe`.
