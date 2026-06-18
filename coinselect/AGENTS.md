# coinselect

## Purpose

Provides a single, coin-type-agnostic largest-first coin-selection algorithm
shared across the client. Holds no wallet, actor, or RPC dependencies so every
layer that needs to pick a covering subset of coins — the VTXO manager's
reservation path and the swap wallet's send preview alike — selects through the
same code rather than growing parallel implementations.

## Key Types

- `Request` — Parameters for a selection pass. `Target` (required, positive)
  drives a bounded selection; `SweepAll` selects every candidate regardless of
  Target. `MinChange` rejects a covering selection whose non-zero change falls
  below the floor, continuing the search for an exact-fit set.
- `Result[T]` — Outcome of a selection pass: `Selected []T` (chosen subset),
  `Total` (summed amount), `Change` (`Total - Target`; zero for sweeps).
- `AmountFunc[T]` — Caller-supplied function extracting the satoshi value of a
  candidate. Keeps the selector agnostic to the concrete coin type (VTXO
  descriptor, RPC VTXO, boarding intent, etc.).
- `LargestFirst[T]` — The single exported algorithm. Sorts a copy of candidates
  by descending amount (caller's slice is never mutated) and accumulates until
  the target is covered; respects `MinChange` by continuing past a dust-change
  covering set.
- Error sentinels: `ErrSelectionShortfall`, `ErrChangeBelowMin`,
  `ErrNoCandidates`, `ErrInvalidTarget`.

## Relationships

- **Depends on**: `btcutil` (amount type only).
- **Depended on by**: `vtxo` (manager's reservation coin selection), `swapwallet`
  (send preview via `router.prepareOnchain`).

## Invariants

- The caller's candidate slice is never mutated; sorting happens on an internal
  copy.
- An exact-fit selection (change == 0) is always accepted regardless of
  `MinChange`.
- `SweepAll` takes precedence over `Target` and `MinChange`.
- On typed error returns (`ErrSelectionShortfall`, `ErrChangeBelowMin`),
  `Result.Selected` is nil and `Result.Total` carries the covered total so
  callers can render precise diagnostics.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
