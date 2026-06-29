# coinselect

## Purpose

Coin-type-agnostic coin-selection algorithm shared across all layers that
need to pick a covering subset of coins. Deliberately holds no wallet, actor,
or RPC dependencies so the VTXO manager's reservation path and the swap
wallet's send preview both select through the same code rather than growing
parallel implementations.

## Key Types

- `Request` — parameterizes a selection pass. `Target` (bounded) or
  `SweepAll` (selects everything, ignores `Target` and `MinChange`).
  `MinChange` rejects a covering selection whose non-zero change falls
  below the dust floor; an exact-fit (zero-change) result is always
  accepted regardless.
- `Result[T]` — outcome of a selection pass. `Selected` is the chosen
  subset; `Total` is their sum; `Change` is `Total - Target`.
- `AmountFunc[T]` — caller-supplied extractor that maps any candidate
  type to its `btcutil.Amount` so the selector stays agnostic to the
  concrete coin type (VTXO descriptor, boarding intent, etc.).
- `LargestFirst[T]` — main algorithm. Sorts candidates descending by
  amount and accumulates until covered. Does not mutate the caller's
  slice.
- Error sentinels: `ErrSelectionShortfall`, `ErrChangeBelowMin`,
  `ErrNoCandidates`, `ErrInvalidTarget`.

## Relationships

- **Depends on**: `btcutil` only.
- **Depended on by**: `vtxo` (VTXO manager reservation path),
  `swapwallet` (send preview and sweep-all selection).
- **Sends**: nothing.
- **Receives**: nothing.

## Invariants

- Caller's candidate slice is never mutated; sorting is done on a copy.
- On failure, `Result.Total` carries the covered total at the rejection
  point so callers can render precise diagnostics (e.g. distinguishing
  a true shortfall from a dust-change rejection).
- `SweepAll` selects every candidate in input order regardless of
  `Target` and `MinChange`; the caller owns the single output value.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
