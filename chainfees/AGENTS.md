# chainfees

## Purpose

Provides `chainfee.Estimator` implementations and combinators used to price
on-chain transactions: an HTTP client for mempool.space's recommended-fee
endpoint, a proxy over an lndclient WalletKit, and a selector that queries
several estimators and returns the lowest live rate.

## Key Types

- `MempoolSpaceEstimator` — fetches and caches recommended fees from
  mempool.space's public REST API, keyed by network (mainnet/testnet/
  testnet4/signet).
- `WalletKitEstimator` — proxies fee estimates to an `lndclient.WalletKitClient`;
  built fail-fast by default (`NewWalletKitEstimator`) or with stale-rate
  fallback (`NewFallbackWalletKitEstimator`).
- `MinEstimator` — queries multiple `NamedEstimator` children and returns the
  minimum successful estimate, logging when providers diverge.
- `NamedEstimator` — pairs a `chainfee.Estimator` with a name for logging.
- `Subsystem` — logging subsystem tag (`"FEES"`).

## Relationships

- **Depends on**: `github.com/lightninglabs/lndclient` (WalletKit RPC client
  interface), `github.com/lightningnetwork/lnd/lnwallet/chainfee` (the
  `Estimator` interface and rate types being implemented).
- **Depended on by**: `chainbackends` (aliases `WalletKitEstimator` as
  `LndClientFeeEstimator` and wraps `NewFallbackWalletKitEstimator`),
  `darepod` (wires `MempoolSpaceEstimator` + `WalletKitEstimator` into a
  `MinEstimator` at startup and registers the `FEES` logging subsystem).

## Invariants

- Only compose fail-fast estimators (`NewWalletKitEstimator`,
  `MempoolSpaceEstimator`) as `MinEstimator` children. A fallback estimator
  (`NewFallbackWalletKitEstimator`) can silently win the minimum with a stale
  or floor rate instead of surfacing that all live providers failed — never
  pass one into `NewMinEstimator`.
- All estimators clamp successful rates to `chainfee.FeePerKwFloor` before
  returning or caching them, so a cached/fallback rate below the floor always
  means "no successful estimate yet" (see `WalletKitEstimator.cachedRate`).
- `validateMempoolSpaceURL` rejects plaintext `http` except for loopback
  hosts — fee data must not be tamperable by a network attacker.
- `MempoolSpaceEstimator` bounds the response body it reads
  (`maxMempoolSpaceResponseBytes`) since `http.Client.Timeout` only bounds
  wall-clock time, not bytes read.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map
