# coinselect

## Purpose

Provides a single, coin-type-agnostic coin-selection algorithm (largest-first)
shared across the client. It deliberately holds no wallet, actor, or RPC
dependencies so every layer that needs to pick a covering subset of coins
selects through the same code.

## Key Types

- `Request` — parameterizes a selection pass: target amount, minimum change
  floor, or sweep-all mode
- `Result[T]` — outcome of a selection pass: the chosen subset, their summed
  total, and computed change
- `AmountFunc[T]` — caller-supplied extractor that yields a
  `btcutil.Amount` from a candidate, keeping the selector agnostic to
  concrete coin types (VTXO descriptors, RPC VTXOs, boarding intents, etc.)
- `ErrSelectionShortfall` — candidate set cannot cover the target even when
  fully selected; `Result.Total` carries the candidate sum
- `ErrChangeBelowMin` — a covering selection exists but its change falls
  below `MinChange` and no exact-fit was found
- `ErrNoCandidates` — selection requested over an empty candidate set
- `ErrInvalidTarget` — bounded selection requested with a non-positive target

## Relationships

- **Depends on**: `btcd/btcutil` (amount type only; no internal repo
  dependencies)
- **Depended on by**: `vtxo` (VTXO manager reservation path uses
  `LargestFirst` to select VTXOs covering a target), `swapwallet`
  (send-preview router uses `LargestFirst` to select live VTXOs for
  outbound sends)

## Invariants

- The caller's candidate slice is never mutated; sorting is done on an
  internal copy.
- An exact-fit selection (change == 0) is always accepted regardless of
  `MinChange`.
- `SweepAll` takes precedence over `Target` and `MinChange`; it selects
  every candidate and returns them in input order.
- On any error, `Result.Total` carries the covered total at the rejection
  point so callers can build precise diagnostics without repeating the
  selection logic.
- The package has no wallet, actor, or RPC imports by design; all
  layer-specific policy belongs in the caller.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map
