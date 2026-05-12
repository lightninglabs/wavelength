# fraud

## Purpose

Server-side fraud response actor. Consumes batchwatcher spend notifications
and drives txconfirm to broadcast OOR checkpoint transactions, forfeit
transactions, checkpoint timeout sweeps, and forfeit penalty-output sweeps
before an attacker's unilateral spend can confirm.

## Key Types

- `Actor` — Durable actor that receives `FraudDetectorMsg`s from batchwatcher
  and coordinates multi-stage broadcast jobs via txconfirm. Maintains four
  dedup maps (`pending`, `checkpointsByTxid`, `sweepsByOutput`,
  `forfeitsByOutpoint`) to avoid duplicate submissions across reorgs and
  retries.
- `Config` — Actor configuration: `TxConfirmRef`, `Planner`,
  `CheckpointPlanner`, `CheckpointSweepStore`, `CheckpointPolicy`,
  `OperatorKey`, `Signer`, `NewSweepPkScript`, `BuildSweep`,
  `BuildForfeitSweep`, and optional `Log`.
- `ResponsePlan` — Ordered list of `Ancestors` (optional parent txns) plus a
  `ResponseTx` to broadcast. The `Planner` resolves this from an
  `UnexpectedSpendNotification`.
- `Planner` — Interface: `PlanResponse(ctx, *batchwatcher.UnexpectedSpendNotification)`
  returns a `*ResponsePlan`. `DefaultPlanner` is the production implementation
  (wraps `CheckpointPlanner`).
- `CheckpointPlanner` — Resolves `VTXOOnChainNotification`s into
  `CheckpointPlan`s. Checks VTXO state, looks up the finalized checkpoint tx,
  and validates that checkpoint output 0 binds to the expected pkScript.
- `CheckpointSweepStore` — Interface: `LoadCheckpointSweepInfoByInput` returns
  the metadata (tap tree, output script) needed to rebuild the operator timeout
  leaf sweep after checkpoint output CSV maturity.
- `SweepBuilder` / `ForfeitSweepBuilder` — Function types for constructing
  checkpoint-timeout and forfeit penalty-output sweep transactions.
  `BuildCheckpointTimeoutSweep` and `BuildForfeitOutputSweep` are the
  production implementations.
- `VTXOStore` / `CheckpointLookup` / `ForfeitLookup` — Narrow interfaces used
  by `CheckpointPlanner` to read VTXO state and look up finalized transactions.

## Relationships

- **Depends on**: `batchwatcher` (message types: `VTXOOnChainNotification`,
  `UnexpectedSpendNotification`, `CheckpointSweepNotification`), `txconfirm`
  (broadcast and confirmation tracking via `TxConfirmRef`), `oor`
  (indirectly via `CheckpointSweepStore` seam wired from root), `rounds`
  (indirectly via `ForfeitLookup` seam wired from root), `db`
  (indirectly via lookup seams wired from root), `arkscript`
  (policy/spend path helpers for sweep construction).
- **Depended on by**: root `darepo` (wired in `server_fraud.go`).
- **Messages to/from**:
  - Receives `VTXOOnChainNotification` <- `batchwatcher` (trigger checkpoint
    broadcast when an in-flight leaf is spent on-chain).
  - Receives `UnexpectedSpendNotification` <- `batchwatcher` (trigger forfeit
    or connector-ancestor broadcast for classified spend).
  - Receives `CheckpointSweepNotification` <- `batchwatcher` (trigger
    timeout sweep when checkpoint output matures past CSV).
  - Sends `EnsureConfirmedReq` -> txconfirm (submit each response/sweep tx
    and await confirmation callbacks).
  - Receives `TxConfirmed` / `TxFailed` <- txconfirm (advance or retry job
    chain).

## Invariants

- Dedup entries (`checkpointsByTxid`, `sweepsByOutput`, `forfeitsByOutpoint`)
  are added **after** txconfirm has accepted the submission. A synchronous
  Ask failure must not leave a stale entry that prevents future re-notification.
- For checkpoint jobs, `pending[checkpointTxid]` and
  `checkpointsByTxid[checkpointTxid]` are populated and cleared together.
- For sweep jobs, `pending[sweepTxid]` and `sweepsByOutput[outpoint]` are
  populated and cleared together.
- For forfeit jobs, `forfeitsByOutpoint[outpoint]` is populated when the first
  ancestor is accepted and remains until the penalty sweep confirms or any
  response/sweep `TxFailed` clears it for retry.
- `pending` maps a txid to a **set** of jobs (not a single pointer) because
  connector-tree ancestors are shared across multiple forfeit jobs from the
  same batch; a single-valued map would strand the earlier job's chain.
- `BuildForfeitOutputSweep` validates that forfeit output 0 is the operator's
  BIP86 taproot output before signing.
- `validateCheckpointSweepTx` enforces: single input, correct outpoint, CSV
  sequence, 3-item witness, 2 outputs (non-anchor + anchor).

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
