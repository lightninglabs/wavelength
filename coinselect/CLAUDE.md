# coinselect

## Purpose

Provides a single, coin-type-agnostic coin-selection algorithm shared across
the client. The package holds no wallet, actor, or RPC dependencies so every
layer that needs a covering subset — the VTXO manager's reservation path and
the swap wallet's send preview alike — selects through the same generic code
rather than growing parallel implementations.

## Key Types

- `Request` — selection parameters: `Target` amount, `MinChange` floor,
  `SweepAll` flag. `SweepAll` takes precedence and ignores `Target` and
  `MinChange`.
- `Result[T]` — selection outcome: `Selected` subset, `Total`, `Change`.
- `AmountFunc[T]` — callback to extract a `btcutil.Amount` from a candidate
  of type `T`.

## Selection Errors

- `ErrSelectionShortfall` — candidate set cannot cover target; `Result.Total`
  carries the full candidate sum so callers can render a precise message.
- `ErrChangeBelowMin` — covering selection exists but change is below the
  requested minimum and no exact-fit set was found.
- `ErrNoCandidates` — empty candidate set passed to the selector.
- `ErrInvalidTarget` — non-positive target in a bounded selection.

## Relationships

- **Depends on**: `btcd/btcutil` (amount type).
- **Depended on by**: `vtxo` (VTXO manager reservation path),
  `swapwallet` (send-preview coin selection).
- **Sends**: nothing.
- **Receives**: nothing.

## Invariants

- `SweepAll` takes precedence: when set, `Target` and `MinChange` are ignored
  and every candidate is selected.
- The selector is policy-free: it reports why a pass failed via typed errors
  and leaves layer-specific diagnostics (e.g. locked liquidity vs. true
  shortfall) to callers.
- Error values carry no total on `ErrNoCandidates` and `ErrInvalidTarget`;
  only `ErrSelectionShortfall` and `ErrChangeBelowMin` populate `Result.Total`.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
