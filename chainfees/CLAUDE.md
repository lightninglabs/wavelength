# chainfees

## Purpose

Reusable `chainfee.Estimator` implementations and combinators used to price
on-chain transactions from wallet and daemon chain backends.

## Key Types

- `WalletKitEstimator` — proxies fee estimates to an `lndclient.WalletKitClient`;
  fail-fast by default, optional `FallbackOnError` to serve the last
  successful rate instead of propagating errors.
- `MempoolSpaceEstimator` — fetches recommended fees from the mempool.space
  HTTP API, with a TTL cache and network-default endpoint selection.
- `MinEstimator` — composes multiple `NamedEstimator` children and returns the
  lowest successful estimate, logging when providers diverge.
- `NamedEstimator` — pairs a `chainfee.Estimator` with a stable name for logs.

## Relationships

- **Depends on**: `lndclient` (WalletKit RPC client), `lnd/lnwallet/chainfee`
  (the `Estimator` interface and `SatPerKWeight`/`FeePerKwFloor` types).
- **Depended on by**: `chainbackends` (wires these estimators into chain
  backend adapters), `darepod` (server wiring and logging subsystem
  registration).

## Invariants

- Only `NewWalletKitEstimator` (fail-fast) belongs inside `MinEstimator`;
  `NewFallbackWalletKitEstimator` must only back a standalone estimator, since
  a stale/floor fallback could otherwise incorrectly beat another provider's
  live estimate.
- All estimates are clamped to `chainfee.FeePerKwFloor` before being cached or
  returned; a cached rate below the floor is the sentinel for "no successful
  estimate yet" (see `WalletKitEstimator.cachedRate`).
- `MempoolSpaceEstimator` rejects non-HTTPS URLs except for loopback hosts, to
  avoid tampering with fee data in transit.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map
