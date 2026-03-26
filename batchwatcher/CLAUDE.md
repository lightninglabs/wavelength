# batchwatcher

## Purpose

On-chain batch transaction monitoring and VTXO spend detection. Watches
confirmed batch transactions for sweeps, spends, and expiry, reporting state
changes to the round and sweep subsystems.

## Key Types

- `Actor` — Durable actor monitoring batch transactions on-chain.
- `ActorConfig` — Configuration container for actor initialization.
- `BatchID` — Identifier for a confirmed batch.
- `Output` — Tracked output within a batch tree.
- `BatchTreeState` — Aggregate state of a batch's VTXO tree on-chain.
- `StateStore` — In-memory runtime state for tracked batch trees (rebuilt on actor restart).
- `BatchWatcherMsg` / `BatchWatcherResp` — Sealed message/response interfaces for actor protocol.
- `RegisterBatchRequest` / `GetTreeStateRequest` / `UnregisterBatchRequest` — Inbound actor messages.
- `FraudDetectorMsg` — Interface for messages sent to fraud detector (`VTXOOnChainNotification`).
- `BatchSweeperMsg` — Interface for messages sent to batch sweeper (`BatchExpiredNotification`, `TreeStateChangedNotification`).

## Relationships

- **Depends on**: no internal dependencies (receives chain events from external sources).
- **Depended on by**: `rounds` (confirmation monitoring), `batchsweeper` (sweep eligibility).
- **Messages to/from**:
  - Receives `RegisterBatchRequest` <- `rounds` (register confirmed batch for monitoring).
  - Receives `GetTreeStateRequest` <- `rounds` (query on-chain tree state).
  - Sends `VTXOOnChainNotification` -> fraud detector.
  - Receives `UnregisterBatchRequest` <- `rounds` (stop monitoring a batch).
  - Sends `BatchExpiredNotification`, `TreeStateChangedNotification` -> `batchsweeper`.

## Invariants

- Must detect all spends of tracked outputs; missed spends can cause incorrect VTXO state.
- Batch state must be persisted before notifications are sent.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
