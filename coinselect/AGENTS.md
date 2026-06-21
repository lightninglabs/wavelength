# coinselect

## Purpose

Coin-type-agnostic largest-first coin-selection algorithm shared across the
client. Deliberately holds no wallet, actor, or RPC dependencies so every
layer that needs to pick a covering subset of coins — VTXO manager reservation,
swap wallet send preview, boarding selection — selects through the same code
rather than growing parallel implementations.

## Key Types

- `Request` — Selection parameters: `Target` (required for bounded mode,
  must be positive), `MinChange` (rejects covering selections whose
  non-zero change falls below this floor; zero disables), `SweepAll`
  (selects every candidate, takes precedence over Target and MinChange).
- `Result[T]` — Selection outcome: `Selected` (chosen subset), `Total`
  (summed amount of selected), `Change` (Total - Target for bounded;
  zero for sweep-all). On typed errors, `Total` carries the covered amount
  at the rejection point so callers can build precise diagnostics.
- `AmountFunc[T]` — Caller-supplied closure extracting `btcutil.Amount`
  from a candidate. Keeps the selector agnostic to the concrete coin type
  (`vtxo.Descriptor`, RPC VTXO, boarding intent, etc.).
- `LargestFirst[T]` — Bounded coin selection: sorts a copy of candidates
  by descending amount and accumulates until target is covered, honoring
  `MinChange`. An exact-fit (zero-change) selection is always accepted.
  A `SweepAll` request delegates to `selectAll`.
- Error sentinels: `ErrSelectionShortfall` (candidate total < target),
  `ErrChangeBelowMin` (covering selection exists but leaves dust change),
  `ErrNoCandidates` (empty set), `ErrInvalidTarget` (non-positive target).

## Relationships

- **Depends on**: `btcsuite/btcd/btcutil` (Amount type only).
- **Depended on by**: `vtxo` (VTXO reservation selection), `wallet`
  (boarding and OOR spend selection), `sdk/walletdk` (send preview
  coin selection).
- No actor, wallet, or RPC dependencies by design.

## Invariants

- Caller's slice is never mutated — sorting happens on a copy.
- On `ErrChangeBelowMin`, `Result.Total` is the total at the first
  dust-rejection point; accumulating further can only grow change, not
  return to zero, so the first rejection is the definitive bound.
- Exact-fit (zero-change) selections are always accepted regardless of
  `MinChange`.
- `LargestFirst` is stable-sorted by descending amount, making it
  deterministic for distinct amounts.
- `SweepAll` preserves input order (no sorting); bounded mode sorts a copy.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
