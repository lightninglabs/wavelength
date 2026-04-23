# adminrpc

## Purpose

Generated gRPC stub definitions for the admin service (TriggerBatch, ListRounds,
fee schedule management, treasury status, fee event history, etc.). Do not edit
manually; regenerate with `make rpc`.

## Key Types

- `FeeScheduleParams` — All fee schedule parameters: `AnnualRate`, `BaseMarginSat`,
  congestion thresholds (`UtilizationThresholdBps`, `UtilizationSpreadDelta0Bps`,
  `UtilizationSpreadDelta1Bps`), `MinViablePolicy`, `MinViablePct`,
  `MinRefreshDeltaBlocks`.
- `GetFeeScheduleRequest` / `GetFeeScheduleResponse` — Read the active fee schedule.
- `UpdateFeeScheduleRequest` / `UpdateFeeScheduleResponse` — Hot-reload a new fee
  schedule at runtime; persisted via `db.FeeScheduleStoreDB` so it survives restarts.
- `GetTreasuryStatusRequest` / `GetTreasuryStatusResponse` — Current operator capital
  position: `DeployedCapitalSat`, `WalletBalanceSat`, `KMaxSat`, `Utilization`,
  `LiveVtxoCount`.
- `ListFeeEventsRequest` / `ListFeeEventsResponse` — Paginated double-entry ledger
  history. `FeeEvent` carries per-row fields (entry ID, event type, debit/credit
  accounts, amount, timestamp, round/session ID).
- `FeeEvent` — Single double-entry ledger row as returned by `ListFeeEvents`.
- `TriggerBatchRequest` / `TriggerBatchResponse` — Force-seal the current round.
- `ListRoundsRequest` / `ListRoundsResponse` / `RoundSummary` — Round history.
- `InfoRequest` / `InfoResponse` — Operator daemon metadata.
- `ListClientsRequest` / `ListClientsResponse` / `ClientInfo` — Connected client list.
- `ListVTXOsRequest` / `ListVTXOsResponse` — VTXO inventory.
- `GetVTXOStatsRequest` / `GetVTXOStatsResponse` — VTXO lifecycle aggregate counts.

## Relationships

- **Depends on**: nothing (generated protobuf code).
- **Depended on by**: root `darepo` (`adminrpcserver.go` implements all handlers),
  `harness` (`TakeLedgerSnapshot` calls `ListFeeEvents` via `OperatorAdminClient`),
  `itest` (fee assertion tests call fee schedule / treasury status RPCs).
