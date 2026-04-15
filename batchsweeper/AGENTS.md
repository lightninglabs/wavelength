# batchsweeper

## Purpose

Expired batch recovery via sweep transactions. When batch tree outputs expire
past their CSV timelock, this actor builds and broadcasts sweep transactions to
reclaim funds.

## Key Types

- `Actor` — Durable actor that monitors for sweep-eligible batches and executes
  sweeps. Wired into the production server via `server.go`.
- `Msg` / `Resp` — Sealed message/response interfaces for the actor protocol.
- `BatchExpiredEvent` — Notification from batchwatcher that a batch's CSV
  timelock has expired.
- `TreeStateChangedEvent` — Notification that a batch tree's on-chain state
  changed (e.g., partial spends detected).
- `SweepRetryEvent` — Self-scheduled retry for failed sweep attempts.
- `SweepConfirmedEvent` — Confirmation that a sweep transaction was mined.
- `BatchSweptEvent` — Terminal event indicating all outputs in a batch have been
  swept.
- `SweepTxBuilder` — Constructs sweep transactions for expired tree outputs.
  Uses `arkscript.SpendInfo` (wrapping `WitnessScript`/`ControlBlock`) and
  `arkscript.VTXOTimeoutSpendWitness` from the renamed `lib/arkscript` package
  (was `lib/scripts`). Includes script engine verification tests.

## Relationships

- **Depends on**: `batchwatcher` (sweep-eligible batch notifications). VTXO
  status updates are performed via an `OnBatchSwept` callback injected at
  wiring time in the root package; there is no direct `db` import.
- **Depended on by**: root `darepo` (production wiring in `server.go`).
- **Messages to/from**:
  - Receives `BatchExpiredEvent` <- `batchwatcher`.
  - Receives `TreeStateChangedEvent` <- `batchwatcher`.
  - Self-sends `SweepRetryEvent` for failed sweep retries.
  - Receives `SweepConfirmedEvent` on sweep tx confirmation.
  - Emits VTXO status updates (Expired) via injected `OnBatchSwept` callback
    (satisfied by the root package at wiring time, not a direct `db` import).

## Invariants

- Sweep transactions must only be broadcast after CSV timelock expiry.
- Sweep must be idempotent; re-sweeping an already-swept output is a no-op.
- Watcher self-unregisters batches after successful sweep notification to
  sweeper, preventing duplicate monitoring.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
