# chainfees

## Purpose

Provides reusable `chainfee.Estimator` implementations and combinators for
wallet and daemon chain backends. Bundles three concrete estimators —
`WalletKitEstimator` (queries lnd WalletKit), `MempoolSpaceEstimator` (queries
mempool.space API), and `MinEstimator` (selects the lowest successful estimate
across a set of child estimators) — so backends can compose fee estimation
strategies without re-implementing the interface.

## Key Types

- `WalletKitEstimator` — proxies fee estimates to an lndclient WalletKitClient
  with configurable timeout and optional degraded-mode fallback on error.
- `WalletKitEstimatorConfig` — config for `WalletKitEstimator`: client,
  logger, timeout, fallback flag.
- `MempoolSpaceEstimator` — queries the mempool.space recommended-fee endpoint
  with configurable URL, cache TTL, and chain params. Caches the last
  successful response to avoid hammering the API on every block.
- `MempoolSpaceConfig` — config for `MempoolSpaceEstimator`.
- `MinEstimator` — queries multiple child estimators and returns the minimum
  successful relay fee per KW.
- `NamedEstimator` — wraps a child estimator with a stable name for logging.

## Relationships

- **Depends on**: `lnd/lnwallet/chainfee` (Estimator interface),
  `lndclient` (WalletKitClient), `btcd/btcutil`, `btclog`.
- **Depended on by**: `darepod` (daemon fee estimation),
  `chainbackends` (lnd-backed fee estimation adapter).
- **Sends**: nothing.
- **Receives**: nothing.

## Invariants

- All three exported estimator types implement
  `github.com/lightningnetwork/lnd/lnwallet/chainfee.Estimator`.
- `MempoolSpaceEstimator` caches the last successful response at the
  configured TTL; concurrent callers share the cached estimate without
  issuing duplicate HTTP requests.
- `WalletKitEstimator` with the fallback flag returns a static relay fee
  rather than propagating errors, so fee estimation can degrade gracefully
  when lnd is temporarily unreachable.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
