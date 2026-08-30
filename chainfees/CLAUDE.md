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

## Log Severity

This package is deliberate about which fee conditions page a human (see
[`docs/structured-logging.md`](../docs/structured-logging.md)).

- An individual child-provider failure inside `MinEstimator` is `Info`.
  Failing over to the remaining providers is the design, not an incident.
- Providers whose estimates diverge is `Info`, and the selected provider is
  `Info` — both are routine observability, not an alert.
- A **sub-floor** provider rate — one clamped up to `chainfee.FeePerKwFloor` —
  stays at `Warn`. Clamping makes the returned rate relay-valid but does not
  repair *selection*: the clamped provider becomes the minimum candidate and
  can pin the whole aggregate at the relay floor indefinitely, so persistent
  fee underpayment needs operator action.
- `Warn` is otherwise reserved for the case where *every* provider failed and
  the estimator falls back to its last successful rate.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map
