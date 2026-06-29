# chainfees

## Purpose

Reusable `chainfee.Estimator` implementations and combinators for wallet and
daemon chain backends. Provides an lndclient-backed WalletKit estimator, an
HTTP-backed mempool.space estimator, and a min-selector combinator that picks
the lowest live estimate from multiple providers.

## Key Types

- `WalletKitEstimator` — `chainfee.Estimator` backed by an
  `lndclient.WalletKitClient`. Two modes: fail-fast (default, safe to
  compose inside a selector) and fallback (returns last cached rate on
  error, for standalone use only).
- `WalletKitEstimatorConfig` — config for `WalletKitEstimator`. Fields:
  `WalletKit`, `Log`, `Timeout`, `FallbackOnError`.
- `MempoolSpaceEstimator` — HTTP-backed `chainfee.Estimator` that polls
  mempool.space's recommended-fee endpoint with response caching.
- `MempoolSpaceConfig` — config for `MempoolSpaceEstimator`. Fields:
  `URL` (optional override), `Params` (selects default endpoint),
  `Log`, `Timeout`, `CacheTTL`.
- `MinEstimator` — `chainfee.Estimator` combinator that returns the minimum
  successful estimate from a set of named child estimators. Falls back to
  the last successful rate if all children fail.
- `NamedEstimator` — attaches a stable log name to a child estimator.

## Relationships

- **Depends on**: `lndclient` (WalletKitClient), `lnd/lnwallet/chainfee`
  (Estimator interface + SatPerKWeight), `chaincfg` (network params for
  mempool.space URL selection).
- **Depended on by**: `chainbackends` (wires WalletKitEstimator for the
  lndclient chain backend), `darepod` (wires MinEstimator with optional
  mempool.space provider).
- **Sends**: nothing (all calls are synchronous; no actor or RPC messages).
- **Receives**: nothing.

## Invariants

- `NewWalletKitEstimator` and `NewWalletKitEstimatorWithTimeout` produce
  fail-fast estimators. Never pass a `FallbackOnError=true` estimator into
  `MinEstimator` — a stale fallback floor can incorrectly win over another
  provider's live estimate.
- `MinEstimator` falls back to its own last successful rate when all children
  fail; it never returns an error after receiving at least one prior success.
- Sub-floor rates from any provider are clamped to `chainfee.FeePerKwFloor`
  before selection or caching.
- `MempoolSpaceEstimator` response bodies are capped at 64 KiB to guard
  against unbounded reads from a misbehaving endpoint.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
