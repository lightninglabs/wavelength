# coinselect

## Purpose

A single, coin-type-agnostic coin-selection algorithm shared across the
client, so every layer that must pick a covering subset of coins (the VTXO
manager's reservation path, the swap wallet's send preview) selects through
the same code instead of growing parallel implementations. It holds no
wallet, actor, or RPC dependencies.

## Key Types

- `LargestFirst[T]` — largest-first selection: sorts candidates by descending
  amount, accumulates until `Request.Target` is covered, honors `MinChange`
  and `SweepAll`.
- `Request` — selection parameters: `Target`, `MinChange`, `SweepAll`.
- `Result[T]` — outcome: `Selected`, `Total`, `Change`.
- `AmountFunc[T]` — caller-supplied extractor of a candidate's satoshi value,
  keeping the selector agnostic to the concrete coin type.
- `ErrSelectionShortfall` / `ErrChangeBelowMin` / `ErrNoCandidates` /
  `ErrInvalidTarget` — typed selection failures; the selector stays
  policy-free and leaves diagnostics to callers.

## Relationships

- **Depends on**: `btcutil` (`btcutil.Amount`) only; no internal repo
  dependencies.
- **Depended on by**: `vtxo` (reservation/admission coin selection over VTXO
  descriptors), `swapwallet` (send-preview routing over candidate coins).

## Invariants

- `LargestFirst` never mutates the caller's candidate slice; it sorts a copy.
- An exact-fit (zero-change) selection is always accepted regardless of
  `MinChange`; a zero `MinChange` disables the dust-change check entirely.
- `SweepAll` takes precedence over `Target`/`MinChange` and selects every
  candidate in input order (not sorted).

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map
