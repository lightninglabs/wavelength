# metrics — Prometheus Instrumentation

All arkd Prometheus metrics are namespaced under `arkd_`. The package uses two
collection strategies: event-driven (via the `MetricsActor`) and scrape-driven
(via the `SystemCollector` implementing `prometheus.Collector`).

## Event-Driven Metrics (MetricsActor)

These metrics are updated in real-time as subsystem actors send typed messages
to the `MetricsActor`. Counters always increment; gauges track live state.

### Counters

| Metric | Labels | Description |
|--------|--------|-------------|
| `arkd_rounds_created_total` | — | Total rounds ever created. |
| `arkd_rounds_total` | `status` (confirmed, failed) | Total completed rounds by outcome. |
| `arkd_oor_transfers_total` | `status` (finalized, failed) | Total OOR transfers by outcome. |
| `arkd_vtxo_lock_failures_total` | `reason` (conflict, canceled, timeout) | Failed VTXO lock attempts by cause. |

### Gauges

| Metric | Labels | Description |
|--------|--------|-------------|
| `arkd_rounds_active` | — | Number of currently in-progress rounds. |
| `arkd_connected_clients` | — | Number of currently connected mailbox clients. |
| `arkd_block_height` | — | Latest block height seen at round confirmation. |

### Histograms

| Metric | Labels | Description |
|--------|--------|-------------|
| `arkd_round_duration_seconds` | `status` | Total round duration (creation → confirmation/failure). |
| `arkd_round_registration_duration_seconds` | `status` | Time from round creation to registration seal. |
| `arkd_round_batch_build_duration_seconds` | `status` | Time spent building the batch commitment tx. |
| `arkd_round_vtxo_nonce_exchange_duration_seconds` | — | Time waiting for VTXO nonce submissions. |
| `arkd_round_input_sig_collection_duration_seconds` | `status` | Time waiting for input signature submissions. |
| `arkd_round_clients_joined` | `status` | Number of clients per completed round. |
| `arkd_round_boarding_inputs` | `status` | Boarding inputs per completed round. |
| `arkd_round_leave_outputs` | `status` | Leave outputs per completed round. |
| `arkd_round_vtxos_generated` | `status` | VTXOs generated per completed round. |
| `arkd_oor_transfer_duration_seconds` | `status` | End-to-end OOR transfer duration. |
| `arkd_vtxo_lock_duration_seconds` | `owner` | Time spent acquiring VTXO locks. |
| `arkd_clientconn_dispatch_duration_seconds` | `service_method` | Per-method envelope dispatch latency. |

### gRPC Server Metrics

Per-method request counting and handling time histograms are provided by
`go-grpc-middleware/providers/prometheus` via `GRPCServerMetrics`. These are
registered once and shared by both admin (8081) and client (7070) RPC servers.

## Scrape-Driven Metrics (SystemCollector)

These gauges are populated by querying the database and LND wallet on each
Prometheus scrape via the `SystemCollector` (implements `prometheus.Collector`).
Values are always fresh at scrape time.

| Metric | Labels | Source | Description |
|--------|--------|--------|-------------|
| `arkd_vtxos` | `status` (pending, live, in_flight, forfeited, spent) | DB | Number of VTXOs by status. |
| `arkd_vtxos_value_satoshis` | `status` | DB | Total VTXO value by status in satoshis. |
| `arkd_rounds_by_status` | `status` (pending, confirmed) | DB | Number of rounds by status. |
| `arkd_oor_sessions_by_state` | `state` (cosigned, awaiting_notify, finalized, failed) | DB | Number of OOR sessions by state. |
| `arkd_wallet_confirmed_satoshis` | — | LND | Operator wallet confirmed balance. |
| `arkd_wallet_unconfirmed_satoshis` | — | LND | Operator wallet unconfirmed balance. |

## Architecture

```
Subsystem actors                  MetricsActor              Prometheus
(rounds, oor, clientconn)         (event-driven)            scrape
        │                              │                        │
        │  RoundCreatedMsg             │                        │
        ├─────Tell────────────────────>│                        │
        │  RoundCompletedMsg           │  RoundsTotal.Inc()     │
        ├─────Tell────────────────────>│  RoundsActive.Dec()    │
        │                              │                        │
        │                              │                        │
                                                                │
                                  SystemCollector               │
                                  (scrape-driven)               │
                                       │     ◄──── Collect() ───┤
                                       │                        │
                                       ├── DB: VTXO stats       │
                                       ├── DB: Round stats      │
                                       ├── DB: OOR stats        │
                                       └── LND: Wallet balance  │
```

## Configuration

The metrics HTTP server is configured via the `--metrics.listen` CLI flag
(env: `ARKD_METRICS_LISTEN`). Default: `0.0.0.0:9090`. Set to empty string
to disable.

## Adding New Metrics

- **Event-driven**: Add a message type to `messages.go`, handle it in
  `actor.go:Receive`, define the metric in `metrics.go`, register in
  `allCollectors()`.
- **Scrape-driven**: Add a method to `SystemStatsQuerier` in `collector.go`,
  implement in `metrics_adapter.go`, add a `collect*` method to
  `SystemCollector`, add descriptors to `Describe()`.
