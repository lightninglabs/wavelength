# unrollplan

## Purpose

Pure dependency-resolution planner for unilateral-exit recovery. Given an
immutable `recovery.Proof`, a caller-owned `State`, and a current block
height, the planner answers: which proof transactions are ready to broadcast,
which are blocked, and when is the target CSV-mature for sweeping? No I/O,
no actors — callers own the durable state and the planner re-derives
everything from first principles on each call.

## Key Types

- `Planner` — Constructor-validated wrapper around one immutable
  `recovery.Proof`; only method is `Plan(height, state)`.
- `State` — Durable caller-owned progress: `ConfirmedTxids`, `InFlightTxids`,
  optional `TargetConfirmHeight`, and the final `Sweep` lifecycle. Validated
  against the proof graph before planning.
- `Snapshot` — Caller-facing planning view: `Ready`, `InFlight`, `Blocked`
  frontiers plus CSV info and the `NeedSweep` / `Done` flags.
- `TxFrontier` / `BlockedTx` — Sorted (layer, txid) entries carrying the
  immutable proof node and any missing parents.
- `SweepState` / `SweepStatus` — Pending / Broadcasted / Confirmed lifecycle
  for the final sweep, with optional txid + confirm height as `fn.Option`.
- `CSVInfo` — Maturity view at a height (target confirm height, maturity
  height, blocks remaining, ready flag).

## Relationships

- **Depends on**: `lib/recovery` (immutable `Proof` + `Node` +
  `ComputeMaturityHeight`), `github.com/lightningnetwork/lnd/fn/v2`
  (`Option`, `MapOptionZ`), `github.com/lightningnetwork/lnd/tlv` (state
  codec).
- **Depended on by**: `unroll` (`VTXOUnrollActor` drives `Planner` on every
  block height event; `EncodeState` / `DecodeState` are used by the actor's
  TLV checkpoint codec in `snapshot.go`).

## Invariants

- `State.Validate` is run on every `Plan` call; callers may pass mutable state
  structs and expect a fresh validation each time.
- `ConfirmedTxids` and `InFlightTxids` must be disjoint, duplicate-free, and
  contained in the proof's node set.
- A confirmed child requires all parents confirmed (topological invariant
  mirrored from `lib/recovery.validateSessionState`).
- `TargetConfirmHeight` must be non-negative and may be set only when the
  target appears in `ConfirmedTxids`; inversely, a confirmed target requires
  `TargetConfirmHeight` to be set.
- `SweepStatusBroadcasted` requires the target to be in `ConfirmedTxids` —
  the sweep tx spends the target via a CSV-timelocked path and could not
  land in a mempool otherwise.
- `SweepStatusConfirmed` requires a non-negative confirm height that is
  at or past `target_confirm_height + csv_delay` (via
  `recovery.ComputeMaturityHeight`).
- `Sweep.Txid` must never collide with a proof node txid; a collision would
  make the planner treat the same hash as both a confirmed proof node and a
  sweep step.
- CSV maturity is computed via the overflow-safe `recovery.ComputeMaturityHeight`
  rather than inline `+ int32(...)` math, so bogus inputs fail loudly instead
  of reporting `Ready=true` after an int32 wrap.
- The TLV state codec is canonical (sorted hash lists, single-value
  optionals), carries a version byte, and rejects duplicate keys eagerly.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
- [doc.go](doc.go) — Package overview comment.
- [lib/recovery/CLAUDE.md](../lib/recovery/CLAUDE.md) — Upstream proof model.
