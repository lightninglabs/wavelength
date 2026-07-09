# chainfees

## Purpose

Reusable `chainfee.Estimator` implementations and combinators for wallet and
daemon chain backends: an lnd `WalletKit`-backed estimator, a mempool.space
HTTP-backed estimator, and a `MinEstimator` selector that composes several
providers and picks the lowest live rate.

## Key Types

- `WalletKitEstimator` — Proxies `EstimateFeePerKW` to an
  `lndclient.WalletKitClient`. Fail-fast by default (`NewWalletKitEstimator`);
  `NewFallbackWalletKitEstimator` instead serves the last successful rate (or
  fails closed before any success) rather than propagating errors.
- `MempoolSpaceEstimator` — Queries mempool.space's recommended-fee HTTP
  endpoint, mapping `fastestFee`/`halfHourFee`/`hourFee`/`economyFee`/
  `minimumFee` buckets onto confirmation targets. Caches the response for
  `CacheTTL` (default 30s) and rejects non-loopback plaintext HTTP endpoints.
- `MinEstimator` — Wraps one or more `NamedEstimator` children and returns the
  minimum successful estimate per call; falls back to the last selected rate
  (or the relay floor) only when every child fails.
- `NamedEstimator` — Pairs a `chainfee.Estimator` child with a stable `Name`
  for logging inside `MinEstimator`.
- `DefaultMempoolSpaceURL(params)` — Resolves the network-specific
  mempool.space recommended-fee URL (mainnet/testnet3/testnet4/signet).

## Relationships

- **Depends on**: `lndclient` (`WalletKitClient` for `WalletKitEstimator`),
  `lnd/lnwallet/chainfee` (`Estimator` interface, `SatPerKWeight`,
  `FeePerKwFloor`), `btcd/chaincfg` and `btcd/wire` (network selection in
  `DefaultMempoolSpaceURL`), `btclog` (structured logging).
- **Depended on by**: `chainbackends` (`chainbackends/lndclient_adapters.go`
  aliases `LndClientFeeEstimator = chainfees.WalletKitEstimator` and builds
  the default lnd fee estimator via `NewFallbackWalletKitEstimator`),
  `darepod` (`darepod/server.go`'s `lndFeeEstimator` composes a fail-fast
  `WalletKitEstimator` and a `MempoolSpaceEstimator` under a `MinEstimator`
  when the mempool.space provider is enabled; `darepod/logging.go` registers
  `chainfees.Subsystem` as a log subsystem).

## Invariants

- A child estimator composed inside `MinEstimator` (or `WalletKitEstimator`
  used there) must be fail-fast, not fallback-on-error: a stale fallback rate
  could otherwise beat another provider's live estimate and win the minimum.
  Never pass a `NewFallbackWalletKitEstimator` into `NewMinEstimator`.
- Every estimator clamps successful rates up to `chainfee.FeePerKwFloor`
  before returning or caching them, so a cached value below the floor is the
  sentinel for "no successful estimate yet" (see `WalletKitEstimator.
  cachedRate`).
- `MempoolSpaceEstimator` requires an absolute `https` URL; plaintext `http`
  is only accepted for a loopback host, and the HTTP response body is capped
  at 64 KiB to bound memory from a misbehaving endpoint.
- `NewMinEstimator` and `NewWalletKitEstimatorWithConfig` validate inputs
  (non-empty names, non-nil estimators/clients) at construction so callers
  fail fast instead of panicking on first use of a malformed value boxed into
  the `chainfee.Estimator` interface.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
