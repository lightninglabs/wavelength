# coinselect

## Purpose

Coin-type-agnostic, largest-first coin selection algorithm shared across the
client. Deliberately holds no wallet, actor, or RPC dependencies so it can be
reused by any subsystem that needs to pick a subset of fungible inputs to cover
a target amount.

## Key Types

- `Request` — Selection parameters: `Target` (required amount), `MinChange`
  (dust-change floor; zero disables), `SweepAll` (select every candidate,
  ignoring `Target` and `MinChange`).
- `Result[T]` — Selection outcome: `Selected` slice, `Total` sum, and `Change`
  (zero for sweep-all).
- `AmountFunc[T]` — Caller-supplied extractor that maps a candidate `T` to its
  `btcutil.Amount`; keeps the algorithm generic over concrete VTXO types, RPC
  types, boarding intents, etc.
- `LargestFirst[T]` — The single exported selection function. Sorts a copy of
  candidates by descending amount (never mutates the caller's slice), then
  accumulates until `Target` is covered. Respects `MinChange` by continuing
  accumulation when the candidate after exact-fit would produce dust change;
  exact-fit selections (zero change) are always accepted regardless of
  `MinChange`.

## Relationships

- **Depends on**: `github.com/btcsuite/btcd/btcutil` only — no internal
  repo imports.
- **Depended on by**: `vtxo` (VTXO manager reservation paths),
  `swapwallet` (onchain send previews).
- **Sends**: nothing — pure function, no actor messaging.
- **Receives**: nothing — pure function, no actor messaging.

## Invariants

- **Non-mutating**: `LargestFirst` sorts a copy of the input slice; callers'
  candidate ordering is preserved.
- **Exact-fit bypass**: a selection whose `Change` would be zero is accepted
  even when `MinChange > 0`, preventing the algorithm from over-selecting just
  to avoid a dust floor that doesn't apply.
- **Error taxonomy**: `ErrSelectionShortfall` (insufficient total),
  `ErrChangeBelowMin` (dust change after best selection), `ErrNoCandidates`
  (empty input), `ErrInvalidTarget` (non-positive target on bounded runs).
  Callers distinguish locked-UTXO shortfall from true balance shortfall using
  these sentinels.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
