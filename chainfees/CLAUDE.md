# chainfees

## Purpose

Reusable `chainfee.Estimator` implementations and combinators for wallet and
daemon chain backends: an HTTP-based estimator backed by mempool.space, an
estimator that proxies to an lndclient WalletKit, and a combinator that
queries multiple providers and returns the lowest successful estimate.

## Key Types

- `MempoolSpaceEstimator` / `MempoolSpaceConfig` — Fetches recommended fees
  from mempool.space's REST endpoint, cached with a TTL. Rejects plaintext
  `http` endpoints except on loopback (used by the local test server).
- `WalletKitEstimator` / `WalletKitEstimatorConfig` — Proxies
  `EstimateFeePerKW` to an `lndclient.WalletKitClient`. Fail-fast by default
  (`NewWalletKitEstimator`); `NewFallbackWalletKitEstimator` instead serves the
  last successful rate on error.
- `MinEstimator` / `NamedEstimator` — Combinator that queries named child
  estimators and returns the minimum successful rate, logging when estimates
  diverge by more than 20%. Falls back to the last successful rate if every
  child fails.
- `Subsystem` — Logging subsystem tag `"FEES"`.

All exported types implement `lnwallet/chainfee.Estimator`.

## Relationships

- **Depends on**: `github.com/lightningnetwork/lnd/lnwallet/chainfee`
  (`Estimator` interface, `FeePerKwFloor`), `github.com/lightninglabs/lndclient`
  (`WalletKitClient`).
- **Depended on by**: `chainbackends` (`lndclient_adapters.go` aliases
  `WalletKitEstimator` and uses `NewFallbackWalletKitEstimator`), `darepod`
  (`server.go` wires `WalletKitEstimator` + `MempoolSpaceEstimator` into a
  `MinEstimator`; `logging.go` registers the `Subsystem` tag).

## Invariants

- Never pass a fallback-mode `WalletKitEstimator`
  (`NewFallbackWalletKitEstimator` / `FallbackOnError: true`) as a
  `MinEstimator` child — a stale fallback rate can incorrectly win over
  another provider's live estimate. Only `NewWalletKitEstimator` (fail-fast)
  is safe to compose.
- Every estimate below `chainfee.FeePerKwFloor` is clamped up before it is
  returned or cached.
- `mempool.space` endpoints must be `https`; plaintext `http` is only
  accepted for a loopback host, otherwise a network attacker could tamper
  with fee data in transit.
- `mempool.space` response bodies are capped at 64 KiB regardless of the
  HTTP client timeout, to bound memory against a misbehaving endpoint.
