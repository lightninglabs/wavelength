# fraud

## Purpose

Recipient-side fraud defense for OOR VTXOs. Passively watches ancestor
outpoints in each VTXO's materialization tree and, when a spend is
observed, triggers a durable unilateral-exit job so the operator cannot
claim the VTXO against a stale ancestor.

## Key Types

- `WatcherActor` — Actor managing the set of passive ancestry spend
  watches. One actor per daemon; starts via `NewWatcherActor`. Service
  key: `ServiceKeyName` (`"recipient-fraud-watcher"`), retrieved via
  `ServiceKey()`.
- `WatcherConfig` — Wiring: `ChainSource` (spend watch registration),
  `UnrollRef` (starts unroll jobs on fraud detection), optional `Log`,
  `MailboxSize` (defaults to 64 to absorb reorg-burst spend bursts).
- `WatchPlan` — Per-VTXO passive watch set: a `TargetOutpoint` plus a
  slice of `WatchPoint`s (ancestor outpoints to arm in chainsource).
- `WatchPoint` — One ancestor outpoint to watch: `Outpoint`,
  `PkScript`, `HeightHint`.
- `TrackVTXOsRequest` / `TrackVTXOsResp` — Batch-arm watches for a
  slice of OOR VTXO descriptors. Skips non-OOR and non-live descriptors.
- `UntrackRequest` / `UntrackResp` — Release watches for one VTXO
  (idempotent; silent on unknown targets).
- `SpendObservedMsg` / `AckResp` — Internal: chainsource spend event
  routed back to the watcher's own mailbox.
- `Msg` / `Resp` — Sealed actor message interfaces.
- `BuildWatchPlan(desc *vtxo.Descriptor) (*WatchPlan, error)` — Pure
  function that walks the ancestry tree and collects all on-path tree
  inputs and leaf outputs as watch points.
- `TrackVTXOs(ctx, tracker, descs)` — Fire-and-forget helper used by
  darepod at startup to arm watches for all live descriptors.

## Relationships

- **Depends on**: `baselib/actor` (actor framework), `chainsource`
  (RegisterSpendRequest / UnregisterSpendRequest), `unroll`
  (EnsureUnrollRequest with TriggerFraudSpend), `vtxo` (Descriptor,
  Status), `lib/tree` (ancestry graph traversal).
- **Depended on by**: `darepod` (creates actor, calls `TrackVTXOs` at
  startup, wires `TerminalVTXOObserver` to issue `UntrackRequest`).
- **Sends**:
  - → `chainsource` (Ask): `RegisterSpendRequest` per ancestor
    (first reference), `UnregisterSpendRequest` (last reference).
  - → `unroll` (Tell): `EnsureUnrollRequest{Trigger: TriggerFraudSpend}`
    per target when a watched ancestor is confirmed spent.
  - → self (Tell): `SpendObservedMsg` (chainsource spend callback mapped
    back into the actor mailbox for sequential dispatch).
- **Receives**:
  - ← API: `TrackVTXOsRequest`, `UntrackRequest` (from `darepod`).
  - ← chainsource (via mapped Tell): `SpendObservedMsg`.

## Invariants

- Ancestor watches are reference-counted by target VTXO set. The watch
  is armed exactly once (on first target) and unregistered when the
  last target releases it.
- `UntrackRequest` is idempotent: missing targets produce
  `UntrackResp{Removed: false}` without error.
- Only OOR descriptors with `ChainDepth > 0` are tracked; boarding and
  round-direct VTXOs have no preconfirmed ancestry to defend against.
- Spend fan-out is per-target: one ancestor spend can trigger unroll jobs
  for every VTXO that depends on it, each independently idempotent via
  the unroll registry's dedup guard.
- `BuildWatchPlan` is pure and synchronous; actor state mutations happen
  only inside `Receive`.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
- [unroll/CLAUDE.md](../unroll/CLAUDE.md) — Unroll registry that handles
  fraud-triggered jobs.
- [chainsource/CLAUDE.md](../chainsource/CLAUDE.md) — Spend watch API.
