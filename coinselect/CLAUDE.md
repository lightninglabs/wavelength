# coinselect

## Purpose

Provides a single, coin-type-agnostic largest-first coin-selection algorithm
shared across the client. It deliberately holds no wallet, actor, or RPC
dependencies so that every layer needing to select a covering subset of coins
uses the same implementation rather than growing parallel ones.

## Key Types

- `Request` — parameterizes a selection pass: target amount, minimum change
  floor, and an optional sweep-all flag
- `Result[T]` — outcome of a selection pass: the chosen subset, their summed
  total, and the change amount
- `AmountFunc[T]` — caller-supplied extractor that maps a concrete coin type to
  its `btcutil.Amount`, keeping the selector agnostic to coin representation
- `LargestFirst[T]` — the single exported function; sorts candidates
  descending, accumulates until covered, and enforces the `MinChange` floor

## Relationships

- **Depends on**: `github.com/btcsuite/btcd/btcutil` (amount type only)
- **Depended on by**: `vtxo` (manager reservation path), `swapwallet` (send
  preview / router)
- **Sends**: (none)
- **Receives**: (none)

## Invariants

- The caller's candidate slice is never mutated; `LargestFirst` sorts an
  internal copy.
- `SweepAll` takes precedence over `Target` and `MinChange` — those fields are
  silently ignored when `SweepAll` is set.
- An exact-fit selection (change == 0) is always accepted regardless of
  `MinChange`.
- On any typed error (`ErrSelectionShortfall`, `ErrChangeBelowMin`), the
  returned `Result.Total` carries the relevant covered sum for caller
  diagnostics; `Result.Selected` is nil.
- The package must never gain wallet, actor, or RPC imports — its value is
  being dependency-free so all coin-holding layers can share it.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map
