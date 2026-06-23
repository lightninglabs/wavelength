# coinselect

## Purpose

Coin-type-agnostic coin-selection algorithm. Provides a single
`LargestFirst` selector that callers parameterize with an amount
extractor (`AmountFunc[T]`) so every layer that needs a covering
subset of coins (VTXOs, boarding intents, backing-wallet UTXOs) reuses
the same implementation instead of maintaining parallel custom selectors.

## Key Types

- `AmountFunc[T]` — `func(T) btcutil.Amount`; extracts the satoshi
  value of a candidate. Caller-supplied so the selector stays agnostic
  to concrete coin types.
- `Request` — parameterizes one coin-selection pass:
  `Target btcutil.Amount` (required unless `SweepAll`),
  `MinChange btcutil.Amount` (rejects non-zero change below this
  floor; zero disables), `SweepAll bool` (select every candidate,
  ignore Target/MinChange).
- `Result[T]` — outcome: `Selected []T` (chosen subset in selection
  order), `Total btcutil.Amount` (sum of Selected), `Change
  btcutil.Amount` (Total − Target; zero for sweep).
- `LargestFirst[T any](candidates, amount, req)` — main exported
  function. Sorts candidates by descending amount and accumulates
  until Target covered. When `MinChange > 0`, rejects selections
  whose non-zero change falls below the floor. Exact (zero-change)
  fits are always accepted. Returns typed errors on failure.

## Error Sentinels

- `ErrSelectionShortfall` — candidate set cannot cover Target.
- `ErrChangeBelowMin` — covering selection exists but change is
  non-zero and below `MinChange`.
- `ErrNoCandidates` — selection requested over empty candidate set.
- `ErrInvalidTarget` — bounded selection with non-positive Target.

## Relationships

- **Depends on**: `github.com/btcsuite/btcd/btcutil` (Bitcoin amount
  types); standard library only.
- **Depended on by**: `vtxo` (VTXO manager reservation path),
  `swapwallet` (swap wallet send preview and routing).

## Invariants

- Caller's candidate slice is never mutated; sorting operates on a
  copy.
- Selection is deterministic for distinct amounts (stable sort).
- On failure, the returned `Result` still carries the partial
  `Total` so callers can report a shortfall amount.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
