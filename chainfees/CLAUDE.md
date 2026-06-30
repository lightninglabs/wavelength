# chainfees

## Purpose

Reusable `chainfee.Estimator` implementations and combinators for
wallet and daemon chain backends. Provides a mempool.space HTTP
estimator, an LND WalletKit estimator, and a `MinEstimator` combinator
that takes the minimum of N named estimators — giving callers a
single fee surface without coupling to a specific backend.

## Key Types

- `MempoolSpaceEstimator` — Polls mempool.space's fee API for real-time
  fee estimates. Configured by `MempoolSpaceConfig` (URL, network,
  timeout, poll interval).
- `WalletKitEstimator` — Wraps `lndclient.WalletKitClient` to provide
  fee estimates via LND's `EstimateFee` RPC. Several constructors cover
  the common configurations: plain, fallback (returns zero on error),
  with-timeout, and fully configured.
- `WalletKitEstimatorConfig` — Configuration for `WalletKitEstimator`:
  wallet kit client, logger, timeout, and fallback behavior.
- `MinEstimator` — Combinator that returns the minimum fee estimate
  across a set of named child estimators. Used by chain backends that
  want to pick the cheapest reliable estimate.
- `NamedEstimator` — Name + `chainfee.Estimator` pair used by
  `MinEstimator`.
- `DefaultMempoolSpaceURL` — Returns the canonical mempool.space URL
  for a given Bitcoin network.

## Relationships

- **Depends on**: no internal packages (only external: `lndclient`,
  `btcwallet/chain/chainfee`, `btcwallet/chaincfg`, `btclog`).
- **Depended on by**:
  - `chainbackends` (uses WalletKitEstimator and MempoolSpaceEstimator
    when constructing fee estimators for chain backends)
  - `darepod` (wires chain backend fee estimators into daemon config)
- **Sends**: nothing.
- **Receives**: nothing.

## Invariants

- All estimators implement `chainfee.Estimator`; callers should use
  the interface, not concrete types, except at construction sites.
- `MinEstimator` requires at least one child estimator; `NewMinEstimator`
  returns an error if the slice is empty.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
