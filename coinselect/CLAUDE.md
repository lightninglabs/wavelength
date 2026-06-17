# coinselect

## Purpose

Provides a single, coin-type-agnostic largest-first coin-selection algorithm
shared across the client. Holds no wallet, actor, or RPC dependencies so
every layer that needs to pick a covering subset of coins — the VTXO manager's
reservation path and the swap wallet's send preview alike — selects through the
same code rather than growing parallel implementations.

## Key Types

- `Request` — Parameterizes a selection pass: `Target` (required, > 0 unless
  `SweepAll`), `MinChange` (reject non-zero change below this floor; zero
  disables), `SweepAll` (select every candidate, ignoring `Target` and
  `MinChange`).
- `Result[T]` — Outcome of a selection pass: `Selected` (chosen subset in
  selection order; largest-first for bounded, input order for sweep),
  `Total` (sum of Selected), `Change` (Total − Target for bounded; zero for
  sweep-all).
- `AmountFunc[T]` — Caller-supplied extractor `func(T) btcutil.Amount` so the
  selector stays agnostic to the concrete coin type (VTXO descriptor, RPC
  VTXO, boarding intent, etc.).
- `LargestFirst[T]` — Generic bounded or sweep-all selection. Sorts candidates
  by descending amount, accumulates until `Target` is covered. When
  `MinChange > 0`, rejects a covering set whose non-zero change falls below
  the floor and continues accumulating; an exact-fit (zero change) is always
  accepted regardless of `MinChange`.
- `ErrSelectionShortfall` — Candidate set cannot cover the target even when
  every candidate is selected; `Result.Total` carries the full candidate sum.
- `ErrChangeBelowMin` — A covering selection exists but its non-zero change
  falls below `MinChange`; `Result.Total` carries the total at the first
  rejection point.
- `ErrNoCandidates` — Selection requested over an empty candidate set.
- `ErrInvalidTarget` — Bounded selection requested with non-positive target.

## Relationships

- **Depends on**: standard library only (`sort`, `errors`,
  `github.com/btcsuite/btcd/btcutil`).
- **Depended on by**: `vtxo` (VTXO reservation coin selection),
  `swapwallet` (onchain send preview), `wallet` (onchain send path via
  `swapwallet`).
- **Sends**: nothing — pure function with no actor, RPC, or store coupling.
- **Receives**: nothing.

## Invariants

- The caller's input slice is never mutated; `LargestFirst` sorts a copy.
- `SweepAll` takes precedence over `Target` and `MinChange`; those fields
  are ignored for a sweep-all request.
- On failure, `Result.Total` carries the covered total at the first rejection
  point so callers can render precise diagnostics (e.g. distinguishing locked
  liquidity from a true shortfall).
- Errors are typed sentinels and are `errors.Is`-safe; callers MUST use
  `errors.Is`, not string comparison.
- An exact-fit selection (change == 0) is always accepted regardless of
  `MinChange`.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
