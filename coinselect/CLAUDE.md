# coinselect

## Purpose

Coin-type-agnostic coin-selection algorithm shared across the client. It
holds no wallet, actor, or RPC dependencies so every layer that needs to
pick a covering subset of coins — the VTXO manager's reservation path and
the swap wallet's send preview alike — selects through the same code
instead of growing parallel implementations.

## Key Types

- `Request` — selection parameters: `Target` (bounded selection) or
  `SweepAll` (select everything, ignoring `Target`/`MinChange`), plus
  optional `MinChange` dust floor.
- `Result[T]` — outcome: `Selected` subset (largest-first order, or input
  order for a sweep), `Total`, and `Change` (`Total - Target`, zero for a
  sweep).
- `AmountFunc[T]` — caller-supplied extractor of a candidate's satoshi
  value, keeping the selector agnostic to the concrete coin type (VTXO
  `Descriptor`, RPC VTXO, boarding intent, etc).
- `LargestFirst[T]` — generic largest-first selection function: sorts a
  copy of the candidates descending, accumulates until the target is
  covered, and rejects dust change in favor of continuing to accumulate
  toward a dust-safe or exact-fit selection.

## Relationships

- **Depends on**: `btcsuite/btcd/btcutil` (amount type) only — no
  wallet, actor, or RPC imports by design.
- **Depended on by**: `vtxo` (manager reservation path selects live VTXOs
  to cover a spend), `swapwallet` (router's send preview selects live
  VTXOs for a swap quote).
- **Sends**: none — pure function package, no actor/RPC message flow.
- **Receives**: none — called directly as a library function.

## Invariants

- The caller's candidate slice is never mutated; sorting happens on a
  copy, so concurrent callers can safely pass shared slices.
- An exact-fit (zero-change) selection is always accepted regardless of
  `MinChange`; only non-zero change below `MinChange` is rejected.
- `SweepAll` takes precedence over `Target`/`MinChange` and always
  returns every candidate, even a single one.
- On the typed errors (`ErrSelectionShortfall`, `ErrChangeBelowMin`,
  `ErrNoCandidates`, `ErrInvalidTarget`) `Result.Selected` is empty —
  callers must check the error before trusting `Selected`, not just
  `Total`.
- The package stays policy-free: it reports why a pass failed and the
  covered total, and leaves layer-specific diagnostics (e.g.
  distinguishing locked liquidity from a true shortfall) to callers.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map
