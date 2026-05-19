# fraud

## Purpose

Detects recipient-side OOR ancestry materialization by maintaining passive
spend watches on ancestor outpoints of locally-owned OOR VTXOs. When any
watched ancestor is observed spent on-chain, the watcher triggers a durable
unroll job so the owner can recover their funds before an adversary completes
materialization.

## Key Types

- `WatcherActor` — Durable actor implementing the fraud watcher behavior.
  Both the public handle (`Ref`/`Stop`) and the `actor.ActorBehavior`
  implementation; driven through the embedded actor runtime.
- `WatcherConfig` — Wiring: `ChainSource` (passive spend watch provider),
  `UnrollRef` (durable unroll job registry), optional `Log` and mailbox size.
- `WatchPlan` — Per-VTXO passive watch set: target outpoint + list of
  ancestor `WatchPoint`s whose spend signals materialization.
- `WatchPoint` — One watched ancestor: outpoint, pkScript, and height hint.
- `TrackVTXOsRequest` / `TrackVTXOsResp` — Ask the watcher to arm watches
  for a batch of live OOR VTXO descriptors.
- `UntrackRequest` / `UntrackResp` — Release watches for one VTXO that is
  no longer live (settled, forfeited, etc.).
- `SpendObservedMsg` — Internal notification from `chainsource` that a
  watched ancestor outpoint was spent on-chain.

## Relationships

- **Depends on**: `chainsource` (passive spend registration), `unroll`
  (EnsureUnroll on fraud trigger), `baselib/actor` (actor runtime), `vtxo`
  (Descriptor shape for BuildWatchPlan)
- **Depended on by**: `darepod` (creates the watcher during wallet startup,
  arms watches for live OOR VTXOs after each round or OOR completion)
- **Sends**:
  - → `unroll`: `EnsureUnrollRequest{Trigger: TriggerFraudSpend}` when a
    watched ancestor spend is observed
  - → `chainsource`: spend registration requests (via `WatcherConfig.ChainSource`)
- **Receives**:
  - ← `darepod`: `TrackVTXOsRequest`, `UntrackRequest` (via actor mailbox)
  - ← `chainsource`: `SpendObservedMsg` (spend notifications)

## Invariants

- `WatcherActor.OnStop` releases every active spend watch; partial failure
  on one outpoint is logged and aggregated but does not abandon the rest.
- `BuildWatchPlan` returns `ErrWatchUnavailable` when local state is
  insufficient to arm watches (e.g. missing ancestry); callers must tolerate
  this without treating it as a hard error.
- The actor mailbox is sized to absorb burst spend notifications during a
  chain reorganization without blocking the `chainsource` publisher.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
