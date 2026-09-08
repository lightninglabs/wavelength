# chainfees

## Purpose

Reusable `chainfee.Estimator` implementations and combinators used to price
on-chain transactions from wallet and daemon chain backends.

## Key Types

- `WalletKitEstimator` — proxies fee estimates to an `lndclient.WalletKitClient`;
  fail-fast by default, optional `FallbackOnError` to serve the last
  successful rate instead of propagating errors.
- `MempoolSpaceEstimator` — fetches recommended fees from a mempool.space-style
  `/api/v1/fees/recommended` endpoint, with a TTL cache and network-default
  endpoint selection. Every network's default is a Lightning Labs-operated
  mempool instance, mirroring the `lwwallet` Esplora defaults; set
  `MempoolSpaceConfig.URL` to point at another instance.
- `MinEstimator` — composes multiple `NamedEstimator` children and returns the
  lowest successful estimate, logging when providers diverge.
- `NamedEstimator` — pairs a `chainfee.Estimator` with a stable name for logs.

## Relationships

- **Depends on**: `lndclient` (WalletKit RPC client), `lnd/lnwallet/chainfee`
  (the `Estimator` interface and `SatPerKWeight`/`FeePerKwFloor` types).
- **Depended on by**: `chainbackends` (wires these estimators into chain
  backend adapters), `waved` (server wiring and logging subsystem
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
- **Log severity in `MinEstimator` tracks whether an operator must act.** A
  single provider failing is logged at info: the estimator queries several
  providers precisely so it can continue past one, and a later provider can
  still supply the selected rate, so this is degraded input diversity rather
  than an incident. The all-providers-failed fallback stays a warning. A
  provider whose rate had to be clamped up to `chainfee.FeePerKwFloor` also
  stays a **warning**, even though clamping already made the returned rate
  relay-valid: clamping repairs the value but not the selection, so the clamped
  provider becomes the minimum candidate and can pin the aggregate estimator at
  the relay floor indefinitely. Persistent fee underpayment needs a human, so
  do not demote that one along with the others.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map
