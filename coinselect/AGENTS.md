# coinselect

## Purpose

A single, coin-type-agnostic coin-selection algorithm shared across the
client. It holds no wallet, actor, or RPC dependencies so every layer that
needs to pick a covering subset of coins — the VTXO manager's reservation
path and the swap wallet's send preview alike — selects through the same
code rather than growing parallel implementations.

## Key Types

- `Request` — Selection parameters: `Target` (amount to cover), `MinChange`
  (dust floor), `SweepAll` (select everything, ignoring the other two).
- `Result[T]` — Outcome: `Selected` subset, `Total`, `Change`.
- `AmountFunc[T]` — Caller-supplied amount extractor, keeps the selector
  generic over the concrete coin type (VTXO descriptor, RPC VTXO, boarding
  intent, etc.).
- `LargestFirst[T]` — Largest-first selection algorithm; the package's only
  entry point.
- `ErrSelectionShortfall`, `ErrChangeBelowMin`, `ErrNoCandidates`,
  `ErrInvalidTarget` — Typed errors; `Result.Total` still carries the covered
  amount on the first two so callers can build precise diagnostics.

## Relationships

- **Depends on**: `github.com/btcsuite/btcd/btcutil` (`Amount`) only.
- **Depended on by**: `vtxo` (`manager.go` — reservation-path selection),
  `swapwallet` (`router.go` — send-preview selection).

## Invariants

- `LargestFirst` never mutates the caller's candidate slice; it sorts a
  copy.
- An exact-fit (zero-change) selection is always accepted regardless of
  `MinChange`.
- `SweepAll` takes precedence over `Target`/`MinChange` when set.
- Once a covering selection is found with dust change (below `MinChange`),
  the algorithm keeps accumulating rather than returning immediately: change
  only grows from there, so a deeper selection may clear `MinChange`, though
  it can never return to an exact zero-change fit.
