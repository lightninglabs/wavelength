# fraud

## Purpose

Detects and responds to recipient-side OOR ancestry materialization attacks.
Watches ancestor VTXO outpoints on-chain; when any ancestor is spent,
automatically triggers unilateral exit for all affected recipient VTXOs.

## Key Types

- `WatcherActor` — Durable actor managing passive fraud detection. Registered
  under service key `"recipient-fraud-watcher"` via `ServiceKey()`. Tracks
  a `WatchPlan` per live VTXO; on spend notification, asks the unroll
  registry to `EnsureUnroll` with `TriggerFraudSpend` for each affected
  target.
- `WatcherConfig` — Wiring: `ChainSource` (spend monitor), `UnrollRef`
  (durable unroll job trigger), `Log`, `MailboxSize` (default 64).
- `WatchPlan` — Passive watch set for one VTXO. Contains a target
  outpoint and a list of `WatchPoint` ancestors to monitor.
- `WatchPoint` — Single outpoint to watch: `Outpoint`, `PkScript`,
  `HeightHint`.
- `Msg` / `Resp` — Sealed message interfaces (fraud-only).
  - `TrackVTXOsRequest` / `TrackVTXOsResp` — Admit OOR VTXO descriptors
    for fraud monitoring.
  - `UntrackRequest` / `UntrackResp` — Release all watches for a target
    outpoint.
  - `SpendObservedMsg` / `AckResp` — Spend event fanout to unroll.

## Relationships

- **Depends on**: `baselib/actor` (actor framework), `chainsource` (spend
  event monitoring), `unroll` (durable unroll job registry), `vtxo` (VTXO
  descriptor types), `lib/tree` (ancestry tree walk for watch-point assembly).
- **Depended on by**: `darepod` (wires up on startup).
- **Sends**:
  - → `chainsource`: `RegisterSpendRequest` per ancestor outpoint on
    `TrackVTXOsRequest`; `UnregisterSpendRequest` on `UntrackRequest` /
    when the last target referencing a watch point is removed.
  - → `unroll` registry (via `UnrollRef.Ask`): `EnsureUnrollRequest` with
    `TriggerFraudSpend` when a watched ancestor is spent.
- **Receives**:
  - ← `darepod`: `TrackVTXOsRequest`, `UntrackRequest`
  - ← `chainsource`: spend notifications (re-dispatched internally as
    `SpendObservedMsg` per affected target)

## Invariants

- **Refcounting.** The `ancestorWatches` map uses target-set cardinality as
  a reference count per ancestor outpoint. A `chainsource` spend watch is
  registered on first reference and unregistered when all targets that need
  it are removed.
- **Atomic watch setup.** On `TrackVTXOs`, all ancestor watches are
  registered before the target is committed. If any watch fails mid-setup,
  already-registered watches are released so no orphan registrations leak.
- **Idempotent untrack.** `UntrackRequest` for an unknown target silently
  succeeds; per-watch release errors are aggregated rather than
  short-circuited.
- **Filter at admission.** Only VTXO descriptors with `Status=Live` and
  `ChainDepth > 0` are tracked; already-terminal or non-OOR VTXOs are
  skipped.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
- [unroll/CLAUDE.md](../unroll/CLAUDE.md) — Unilateral-exit registry that
  fraud triggers.
