# fraud

## Purpose

Passive fraud-monitoring actor that arms ancestry spend watches on locally
owned OOR VTXOs. When a watched ancestor outpoint is spent on-chain, it
fans out one `unroll.EnsureUnrollRequest{Trigger: TriggerFraudSpend}` per
affected VTXO so the unroll registry begins an immediate unilateral exit.

## Key Types

- `WatcherActor` — Non-durable actor owning the full watch set for all
  tracked VTXOs. Created via `NewWatcherActor(WatcherConfig)`.
- `WatcherConfig` — Config: `ChainSource` ref for spend registration,
  `UnrollRef` for triggering exits, optional `Log`, and `MailboxSize`
  (default `64`, sized to absorb reorg bursts without blocking the
  chain-source publisher).
- `ServiceKey()` — Returns the receptionist key `"recipient-fraud-watcher"`
  used by `darepod` to look up the actor at runtime.
- `Msg` / `Resp` — Sealed actor message/response interfaces.
- `TrackVTXOsRequest` / `TrackVTXOsResp` — Tell the watcher to arm
  watches for a batch of `vtxo.Descriptor` entries. The actor builds a
  `WatchPlan` per descriptor and refcounts shared ancestor outpoints.
- `UntrackRequest` / `UntrackResp` — Release watches for a target
  outpoint; when refcount drops to zero the spend registration is
  cancelled.
- `SpendObservedMsg` / `AckResp` — Notification from `chainsource`; the
  watcher fans out an `EnsureUnrollRequest` for every VTXO that
  contributed to the spent ancestor.
- `WatchPlan` — Per-descriptor ancestry watch plan produced by
  `BuildWatchPlan(desc)`. Walks `desc.Ancestry`, derives the ancestor
  outpoints from each `TreePath`, and records which target VTXOs depend
  on each ancestor.
- `WatchPoint` — One entry in a `WatchPlan`: the ancestor outpoint and
  the target VTXO outpoint it guards.
- `TrackVTXOs(ctx, tracker, descs)` — Convenience wrapper that calls
  `TrackVTXOsRequest` on a `TellOnlyRef[Msg]`; returns
  `ErrWatchUnavailable` when the actor is not registered.

## Relationships

- **Depends on**: `chainsource` (passive spend watches per ancestor),
  `vtxo` (`Descriptor`, ancestry fields), `unroll` (registry for
  fraud-triggered exits), `baselib/actor` (actor system + service key).
- **Depended on by**: `darepod` (wires watcher at startup; calls
  `TrackVTXOs` from the OOR materialization path after incoming VTXOs
  are durably persisted).
- **Sends**:
  - → `chainsource`: `RegisterSpendRequest` per unique ancestor outpoint
    (first reference); `UnregisterSpendRequest` when refcount drops to
    zero.
  - → `unroll` registry: `EnsureUnrollRequest{Trigger: TriggerFraudSpend}`
    per affected VTXO on `SpendObservedMsg`.
- **Receives**:
  - ← `darepod` (via `TrackVTXOs`): `TrackVTXOsRequest`
  - ← `darepod` (on VTXO terminal or manual untrack): `UntrackRequest`
  - ← `chainsource` (spend notification Tell ref): `SpendObservedMsg`

## Invariants

- Ancestor watch registrations are refcounted: a shared ancestor outpoint
  that appears in multiple VTXOs' ancestry is registered only once with
  `chainsource`, and only cancelled when all depending VTXOs have been
  untracked.
- A spend notification fans out to all VTXOs that cited the spent
  ancestor, even if some of those VTXOs are already in a terminal state;
  the unroll registry dedup handles redundant `EnsureUnroll` calls.
- `BuildWatchPlan` rejects descriptors with an empty `Ancestry` slice
  (no tree path to watch) and returns a nil plan; callers skip tracking
  for such VTXOs rather than registering a zero-outpoint watch.

## Deep Docs

- [docs/durable_actor_architecture.md](../docs/durable_actor_architecture.md) — Actor system internals.
- [unroll/CLAUDE.md](../unroll/CLAUDE.md) — Unilateral-exit actor driven by fraud triggers.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
