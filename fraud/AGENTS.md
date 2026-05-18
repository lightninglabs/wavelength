# fraud

## Purpose

Passive on-chain ancestry fraud detector for OOR VTXOs. The `WatcherActor`
monitors ancestor outpoints of live OOR VTXOs (those with `ChainDepth > 0`)
and triggers a unilateral-exit job when any ancestor is spent on chain before
the expected confirmation path. This defends against a counterparty who
materializes an intermediate checkpoint to claim a VTXO before the honest
recipient does.

## Key Types

- `WatcherActor` — Actor behavior and public handle. Owns a
  `target → [watchedOutpoints]` bimap plus a `ancestor → targetSet` reverse
  index for reference counting. Registered with the actor system under
  `ServiceKey()`.
- `WatcherConfig` — Wiring: `ChainSource` (for spend-watch registration),
  `UnrollRef` (for triggering exits), logger, mailbox size.
- `WatchPlan` — Computed passive watch set for one VTXO: `TargetOutpoint` plus
  the `[]WatchPoint` ancestors to monitor.
- `WatchPoint` — One ancestor: `Outpoint`, `PkScript`, `HeightHint` (the
  VTXO's `CreatedHeight`).
- `TrackVTXOsRequest` / `TrackVTXOsResp` — Ask-message to arm watches for a
  batch of descriptors. Idempotent per descriptor; partial failures are
  aggregated (not short-circuited) and rolled back per descriptor.
- `UntrackRequest` / `UntrackResp` — Ask-message to disarm watches for a
  target VTXO. No-op if the target is unknown.
- `SpendObservedMsg` — Internal Tell mapped from a `chainsource` spend event.
  Triggers `unroll.EnsureUnrollRequest{Trigger: TriggerFraudSpend}`.
- `AckResp` — Empty acknowledgement for administrative Tells.
- `ServiceKey()` — Returns the actor system receptionist key; service key name
  is `ServiceKeyName = "recipient-fraud-watcher"`.
- `BuildWatchPlan(desc *vtxo.Descriptor) (*WatchPlan, error)` — Constructs the
  passive fraud watch set from a VTXO descriptor. Recursively walks the tree
  path (commitment tree inputs) and leaf outputs. Only descriptors with
  `ChainDepth > 0` produce a non-empty plan.
- `TrackVTXOs(ctx, ref, descs)` — Helper that Tells the watcher to arm watches
  for a batch of descriptors; returns on first error.

## Relationships

- **Depends on**: `baselib/actor` (actor system), `chainsource` (spend-watch
  registration), `unroll` (exit triggering), `vtxo` (descriptor type and status
  constants).
- **Depended on by**: `darepod` (starts the watcher actor during daemon
  startup; arms watches for live VTXOs via `vtxo.Manager` →
  `ListLiveDescriptorsRequest`).
- **Sends**:
  - → `chainsource` (Ask): `RegisterSpendRequest` (arm), `UnregisterSpendRequest`
    (disarm) — refcounted per ancestor outpoint.
  - → `unroll` (Ask): `EnsureUnrollRequest{Trigger: TriggerFraudSpend}` on
    each fraud-detected spend.
- **Receives**:
  - ← `darepod`: `TrackVTXOsRequest` (at startup, per new materialized VTXO),
    `UntrackRequest` (when VTXO goes terminal via `TerminalVTXOObserver`).
  - ← `chainsource` (mapped Tell): `SpendObservedMsg`.

## Invariants

- **Refcounting:** A chainsource spend watch is armed exactly once per ancestor
  outpoint regardless of how many target VTXOs share it. The watch is disarmed
  only when the last referencing target is untracked.
- **Only OOR VTXOs are tracked.** `shouldTrackDescriptor` filters for
  `VTXOStatusLive` and `ChainDepth > 0`; round-direct VTXOs (depth=0) carry no
  ancestor checkpoints and cannot be defrauded by ancestor spend.
- **Rollback on partial failure.** If registering one watch within a
  `trackDescriptor` call fails, all previously registered watches for that
  descriptor are rolled back before returning an error; the target is not
  partially tracked.
- **Error aggregation on batch.** A single bad descriptor cannot disarm fraud
  defenses for the rest of the batch; per-item failures are collected and
  returned as a combined error.
- **Idempotent untrack.** `UntrackRequest` is a safe no-op if the target was
  never tracked or was already removed.
- **OnStop cleanup.** Shutdown releases every active spend watch; per-watch
  errors are logged and aggregated but do not abort the stop sequence.
- **Watch-point ordering is deterministic.** Watch points are sorted by
  outpoint before storage so actor I/O is reproducible in tests.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
