# coinselect

## Purpose

A single, generic, coin-type-agnostic coin-selection algorithm
(largest-first) shared across the client, so every layer that needs to pick
a covering subset of coins — the VTXO manager's reservation path and the
swap wallet's send-preview path — selects through identical logic instead of
maintaining parallel greedy-selection implementations. Deliberately
dependency-free: it operates over a generic `[]T` via a caller-supplied
`AmountFunc[T]`, with no wallet/actor/RPC awareness.

## Key Types

- `LargestFirst[T any](candidates []T, amount AmountFunc[T], req Request)
  (Result[T], error)` — The sole algorithm entry point. Sorts a copy of
  candidates descending by amount, accumulates until the target is covered,
  applies dust/`MinChange` handling.
- `Request` — `{Target btcutil.Amount; MinChange btcutil.Amount; SweepAll
  bool}`. `SweepAll` takes precedence over `Target`/`MinChange` when true.
- `Result[T any]` — `{Selected []T; Total btcutil.Amount; Change
  btcutil.Amount}`. On error, only `Total` is populated (diagnostic).
- `AmountFunc[T any] func(T) btcutil.Amount` — caller-supplied amount
  extractor, keeps the selector agnostic to the concrete coin type (VTXO
  `Descriptor`, RPC VTXO, boarding intent, etc.).
- `ErrSelectionShortfall`, `ErrChangeBelowMin`, `ErrNoCandidates`,
  `ErrInvalidTarget` — sentinel errors for the respective failure modes.

## Relationships

- **Depends on**: none repo-internal — only stdlib (`errors`, `sort`) and
  `btcsuite/btcd/btcutil`.
- **Depended on by**: `vtxo` (`manager.go` reservation path — selected VTXOs
  are then reserved one-by-one through their per-VTXO actor, downstream of
  the plain-function call), `swapwallet` (`router.go` send-preview logic,
  mirrors the daemon's real selection so previews don't under-select).
- **Sends/Receives**: none — pure synchronous generic utility function. Any
  actor interaction (e.g. VTXO reservation) happens in the caller after
  `LargestFirst` returns, not inside this package.

## Invariants

- `LargestFirst` never mutates the caller's input slice — it sorts a copy.
  Ties preserve input order (`sort.SliceStable`) for determinism.
- Dust/`MinChange` handling is a "keep accumulating" search, not
  reject-and-fail: once a running total covers `Target` with change below
  `MinChange`, the algorithm remembers the first such rejection point and
  keeps adding candidates (more value can only grow change, possibly back
  above `MinChange`). Only if candidates are exhausted does it return
  `ErrChangeBelowMin`, with `Result.Total` set to the *first* rejection
  point's total.
- A zero-change (exact-fit) selection is always accepted immediately,
  regardless of `MinChange > 0` — dust-floor checks never block an exact
  match.
- `SweepAll: true` short-circuits before target validation: it ignores
  `Target`/`MinChange`, selects every candidate in input order (not sorted),
  and returns `Change: 0`. An empty candidate set under `SweepAll` still
  returns `ErrNoCandidates`.
- A non-positive `Target` (when not `SweepAll`) returns `ErrInvalidTarget`
  immediately, checked before the empty-candidates case.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
