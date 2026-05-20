# fraud

## Purpose

Detects recipient-side OOR ancestry materialization (operator fraud) by
passively watching on-chain spends of ancestor outpoints for locally-owned
OOR VTXOs. When a watched ancestor is spent on-chain, the actor triggers a
`TriggerFraudSpend` unilateral exit for the affected VTXO.

## Key Types

- `WatcherActor` — Durable actor that maintains passive spend watches for a
  set of locally-owned OOR VTXOs. Calls `unroll.EnsureUnroll(...,
  TriggerFraudSpend)` when any watched ancestor outpoint is spent.
- `WatcherConfig` — Wiring: `ChainSource` (spend watch provider),
  `UnrollRef` (unroll registry actor), optional `Log`, and `MailboxSize`
  override.
- `WatchPlan` — The passive ancestry watch set for one OOR VTXO:
  `TargetOutpoint` (VTXO to unroll if fraud is detected) and `Watches`
  (ancestor outpoints to monitor).
- `WatchPoint` — One ancestor outpoint to watch: `Outpoint`, `PkScript`, and
  `HeightHint` (earliest plausible spend height for chain backend filter
  efficiency).
- `TrackVTXOs(ctx, tracker, descs)` — Helper that tells the watcher to arm
  passive fraud watches for a slice of `vtxo.Descriptor`s.
- `TrackVTXOsRequest` / `TrackVTXOsResp` — Ask-request and response for
  arming a batch of passive fraud watches.
- `UntrackRequest` / `UntrackResp` — Ask-request and response for releasing
  watches for a VTXO that is no longer live (e.g. after cooperative exit or
  unroll completion).
- `SpendObservedMsg` — Tell-message from `chainsource` when a watched ancestor
  outpoint is spent; carries `Outpoint`, `SpendingTxid`, and `Height`.
- `AckResp` — Generic acknowledgement response.
- `ServiceKey() actor.ServiceKey[Msg, Resp]` — Returns the actor receptionist
  key for the fraud watcher; callers should use this rather than constructing
  the key themselves.
- `ErrWatchUnavailable` — Returned when local ancestry state is insufficient
  to build a fraud watch plan.
- `BuildWatchPlan(desc) (*WatchPlan, error)` — Constructs the passive fraud
  watch set from a `vtxo.Descriptor`. Returns `ErrWatchUnavailable` when the
  descriptor has no ancestry information.

## Relationships

- **Depends on**: `baselib/actor` (actor system), `chainsource` (spend
  notifications), `unroll` (EnsureUnroll on fraud detection), `vtxo`
  (Descriptor for building watch plans).
- **Depended on by**: `darepod` (wiring — started on wallet unlock alongside
  the OOR actor).
- **Sends**:
  - → `unroll` registry: `EnsureUnrollRequest{Trigger: TriggerFraudSpend}`
    when a watched ancestor spend is observed.
- **Receives**:
  - ← `chainsource`: `SpendObservedMsg` (per registered spend watch)
  - ← API: `TrackVTXOsRequest`, `UntrackRequest`

## Invariants

- `OnStop` releases every active spend watch; a failure on one outpoint must
  not abandon the rest — errors are aggregated and all watches are attempted.
- The actor is unkeyed: `WatcherActor` is not a durable actor and holds no
  persistent state. Watches are re-armed on restart via `TrackVTXOs` from
  the OOR actor's live session set.
- `BuildWatchPlan` fails with `ErrWatchUnavailable` when ancestry is missing;
  watch arming is best-effort — a VTXO without ancestry data simply has no
  fraud watch rather than blocking the caller.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
