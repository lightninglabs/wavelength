# fraud

## Purpose

Detects and responds to recipient-side OOR ancestry materialization attacks.
Watches ancestor VTXO outpoints on-chain; when any ancestor is spent,
automatically triggers unilateral exit for all affected recipient VTXOs.

## Key Types

- `WatcherActor` — Durable actor managing passive fraud detection. Registered
  under service key `"recipient-fraud-watcher"` via `ServiceKey()`. Tracks
  a `WatchPlan` per live VTXO; on spend notification, asks the VTXO manager
  to force each affected target into unilateral exit via
  `actormsg.ForceUnrollRequest` under `UnrollTriggerFraudSpend`.
- `WatcherConfig` — Wiring: `ChainSource` (spend monitor), `VTXOManagerRef`
  (VTXO manager handle that owns the exit transition and starts the durable
  unroll job), `Log`, `MailboxSize` (default 64).
- `WatchPlan` — Passive watch set for one VTXO. Contains a target
  outpoint and a list of `WatchPoint` ancestors to monitor.
- `WatchPoint` — Single outpoint to watch: `Outpoint`, `PkScript`,
  `HeightHint`.
- `Msg` / `Resp` — Sealed message interfaces (fraud-only).
  - `TrackVTXOsRequest` / `TrackVTXOsResp` — Admit OOR VTXO descriptors
    for fraud monitoring.
  - `UntrackRequest` / `UntrackResp` — Release all watches for a target
    outpoint.
  - `SpendObservedMsg` / `AckResp` — Spend event fanout to the VTXO
    manager as force-unroll requests.

## Relationships

- **Depends on**: `baselib/actor` (actor framework), `chainsource` (spend
  event monitoring), `vtxo` (VTXO manager message types and descriptor types),
  `lib/actormsg` (`ForceUnrollRequest` / `ForceUnrollResponse`,
  `UnrollTriggerFraudSpend`), `lib/tree` (ancestry tree walk for watch-point
  assembly).
- **Depended on by**: `waved` (wires up on startup).
- **Sends**:
  - → `chainsource`: `RegisterSpendRequest` per ancestor outpoint on
    `TrackVTXOsRequest`; `UnregisterSpendRequest` on `UntrackRequest` /
    when the last target referencing a watch point is removed.
  - → `vtxo` manager (via `VTXOManagerRef.Ask`): `actormsg.ForceUnrollRequest`
    with `UnrollTriggerFraudSpend` when a watched ancestor is spent. The
    manager transitions the target into `UnilateralExitState` (persisting it
    out of the live set) and starts the durable unroll job through its
    chain-resolver seam, so fraud escalation converges on the same admission
    gate as manual and critical-expiry exits. A declined transition (the coin
    is already terminal, or the wallet no longer tracks it) is logged as a
    warning rather than surfaced as an error.
- **Receives**:
  - ← `waved`: `TrackVTXOsRequest`, `UntrackRequest`
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
- [vtxo/CLAUDE.md](../vtxo/CLAUDE.md) — VTXO manager that owns the exit
  transition and admits the unroll job for a fraud-forced target.
- [unroll/CLAUDE.md](../unroll/CLAUDE.md) — Unilateral-exit registry the VTXO
  manager drives on behalf of the fraud watcher.
