# fraud

## Purpose

Recipient-side OOR ancestry fraud detector. Monitors the on-chain spend of
every ancestor outpoint in the commitment tree for locally-owned OOR VTXOs and
triggers a durable unroll job via `unroll.EnsureUnroll(..., TriggerFraudSpend)`
when any watched ancestor is spent — indicating an operator is materializing the
commitment tree without the client's participation.

## Key Types

- `WatcherActor` — Main actor. Maintains a reference-counted map of ancestor
  outpoints → target VTXO set. Registers spend watches with `chainsource` on
  the first reference for each outpoint, and unregisters them when the ref
  count drops to zero. Implements `actor.ActorBehavior[Msg, Resp]`.
- `WatcherConfig` — Wiring: `ChainSource` ref for spend watch registration,
  `UnrollRef` for `EnsureUnrollRequest` dispatch, optional `Log`, and
  `MailboxSize` (default 64, sized for chain reorg bursts).
- `WatchPlan` — Passive watch set for one locally-owned OOR VTXO: the target
  outpoint plus the list of `WatchPoint`s derived from its ancestry tree.
- `WatchPoint` — One ancestor outpoint to watch: `Outpoint`, `PkScript`, and
  `HeightHint`.
- `BuildWatchPlan(desc *vtxo.Descriptor) (*WatchPlan, error)` — Derives the
  watch plan from a VTXO descriptor's `Ancestry` field. Traverses every tree
  path and collects all on-path tree inputs (detect materialization start) and
  leaf outputs (detect first OOR checkpoint spending the source VTXO).
- `Msg` / `Resp` — Sealed actor message and response surfaces.
- `TrackVTXOsRequest` / `TrackVTXOsResp` — Batch-admit descriptors for fraud
  watching. Per-descriptor failures are aggregated (not short-circuiting) so
  one bad descriptor cannot disarm watches for the rest.
- `UntrackRequest` / `UntrackResp` — Release watches for a single target VTXO.
  Idempotent: missing targets return success.
- `SpendObservedMsg` / `AckResp` — Internal notification from `chainsource`
  spend watch; fans out to `EnsureUnroll` per affected target.
- `ServiceKey()` — Returns the typed actor service key (`"recipient-fraud-watcher"`).
- `TrackVTXOs(ctx, ref, descs)` — Convenience wrapper: Tells a batch
  `TrackVTXOsRequest` to the watcher actor.

## Relationships

- **Depends on**: `baselib/actor` (actor runtime), `chainsource` (spend watch
  registration via `RegisterSpendRequest` / `UnregisterSpendRequest`),
  `unroll` (fraud-triggered unroll via `EnsureUnrollRequest`), `vtxo`
  (`Descriptor`, ancestry tree), `lib/tree` (tree node traversal).
- **Depended on by**: `darepod` (wires the fraud watcher at startup; calls
  `TrackVTXOs` after the VTXO manager populates live descriptors).
- **Sends**:
  - → `chainsource` (Ask): `RegisterSpendRequest` on first reference for each
    ancestor outpoint; `UnregisterSpendRequest` on last release.
  - → `unroll` (Ask): `EnsureUnrollRequest{Trigger: TriggerFraudSpend}` for
    each affected target when a watched ancestor is spent.
- **Receives**:
  - ← API / `darepod`: `TrackVTXOsRequest`, `UntrackRequest`
  - ← `chainsource` (via mapped spend notification): `SpendObservedMsg`

## Invariants

- Watches are reference-counted per outpoint across multiple target VTXOs that
  share ancestry. `chainsource` registration happens exactly on the first
  reference; deregistration happens exactly when the last reference drops.
- If watch registration fails mid-plan, already-registered watches are rolled
  back so no half-armed target is admitted.
- If per-watch release fails on `Untrack`, the target is already removed from
  the tracked set. Remaining watches are released best-effort; the aggregated
  error is returned but the target will never be re-tracked via a normal
  `Untrack` call.
- `OnStop` releases all active watches. Failures are aggregated, not
  short-circuited, so a single failed deregistration cannot strand the rest.
- `SpendObservedMsg` fans out to all targets that share the spent ancestor. A
  failure for one target does not block unroll for the others.
- The actor mailbox is sized at 64 by default to absorb a burst of spend
  notifications during a chain reorg without blocking the `chainsource`
  publisher.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
- [docs/durable_actor_architecture.md](../docs/durable_actor_architecture.md) —
  Actor and durable mailbox internals.
- [unroll/CLAUDE.md](../unroll/CLAUDE.md) — Durable unroll subsystem;
  `TriggerFraudSpend` is the trigger kind for fraud-induced exits.
- [vtxo/CLAUDE.md](../vtxo/CLAUDE.md) — VTXO descriptor and ancestry shape.
