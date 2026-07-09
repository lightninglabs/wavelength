# coinselect

## Purpose

Single, coin-type-agnostic largest-first coin-selection algorithm shared
across the client, so the VTXO manager's reservation path and the swap
wallet's send preview select through one implementation instead of growing
parallel ones.

## Key Types

- `Request` — selection parameters: a `Target` to cover, an optional
  `MinChange` dust floor, or `SweepAll` to select every candidate.
- `Result[T]` — outcome of a pass: the `Selected` subset, its `Total`, and
  the resulting `Change`.
- `AmountFunc[T]` — caller-supplied extractor of a candidate's
  `btcutil.Amount`, the seam that keeps the selector agnostic to the
  concrete coin type (VTXO descriptor, RPC VTXO, boarding intent, ...).

## Relationships

- **Depends on**: nothing in-repo — only `btcsuite/btcd/btcutil/v2` and the
  standard library. Deliberately holds no wallet, actor, or RPC
  dependencies.
- **Depended on by**: `vtxo` (reservation coin selection in
  `manager.go`), `swapwallet` (send-preview coin selection in
  `router.go`).

## Invariants

- `LargestFirst` never mutates the caller's candidate slice; it sorts a
  copy.
- `SweepAll` takes precedence over `Target`/`MinChange` and selects every
  candidate regardless of their value.
- An exact-fit (zero-change) selection is always accepted even when
  `MinChange` is set; only a non-zero change below `MinChange` is
  rejected in favor of continuing to accumulate (`ErrChangeBelowMin`).
- On failure the returned `Result.Total` still carries the relevant
  covered/rejected total so callers can build precise diagnostics from
  the typed errors (`ErrSelectionShortfall`, `ErrChangeBelowMin`,
  `ErrNoCandidates`, `ErrInvalidTarget`).

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map
