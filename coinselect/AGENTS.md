# coinselect

## Purpose

Coin-type-agnostic coin-selection algorithm shared across the client.
Provides a single `LargestFirst` selector generic over any coin type,
so the VTXO manager's reservation path and the swap wallet's send
preview select from the same code rather than growing parallel
implementations. Deliberately holds no wallet, actor, or RPC
dependencies.

## Key Types

- `LargestFirst[T]` — Greedy coin-selection function. Selects the
  smallest subset of candidates (ordered largest-first) that covers
  `req.TargetAmount + req.FeeHeadroom`. Returns a `Result[T]` with the
  chosen coins, total selected, change, and shortfall.
- `Request` — Selection parameters: target amount, fee headroom,
  and optional maximum number of coins.
- `Result[T]` — Selection outcome: chosen coins, total selected,
  change amount, and whether the selection was exact.
- `AmountFunc[T]` — Caller-supplied projection from a coin value of
  type `T` to `btcutil.Amount` so the selector remains generic.
- `ErrSelectionShortfall` — Returned when the full candidate set still
  cannot cover the target + headroom.

## Relationships

- **Depends on**: none (no internal package imports).
- **Depended on by**:
  - `vtxo` (VTXO reservation path uses LargestFirst to select VTXOs)
  - `swapwallet` (send preview path uses LargestFirst)
- **Sends**: nothing.
- **Receives**: nothing.

## Invariants

- No wallet, actor, or RPC dependencies — intentionally a pure
  algorithm package to keep all coin-selection logic in one place.
- `LargestFirst` is deterministic given a fixed candidate list order.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
