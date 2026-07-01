# chainfees

## Purpose

Reusable `chainfee.Estimator` (lnd's fee-rate interface) implementations and
combinators for chain-fee estimation. Provides a mempool.space HTTP-based
estimator, an lndclient WalletKit-proxying estimator, and a "pick the minimum
of N" combinator, so wallet/daemon backends can compose fee-estimation
strategies (fail-fast vs. fallback-on-error, single-source vs. multi-source)
without duplicating estimator logic.

## Key Types

- `MempoolSpaceEstimator` (`NewMempoolSpaceEstimator(cfg)`) — Queries
  mempool.space's `recommended-fee` REST endpoint. Caches responses (default
  TTL 30s) and bounds the response read to 64 KiB.
- `MempoolSpaceConfig` — `URL`, `Params`, `Log`, `Timeout`, `CacheTTL`.
  `DefaultMempoolSpaceURL(params)` resolves the network-specific default
  (mainnet/testnet3/testnet4/signet); errors on unsupported nets (regtest,
  simnet).
- `WalletKitEstimator` — Proxies to `lndclient.WalletKitClient.EstimateFeeRate`.
  `NewWalletKitEstimator` is fail-fast (default); `NewFallbackWalletKitEstimator`
  returns the last-good rate on error instead of propagating it.
  `NewWalletKitEstimatorWithConfig(cfg WalletKitEstimatorConfig)` exposes the
  full config (`WalletKit`, `Log`, `Timeout`, `FallbackOnError`).
- `MinEstimator` (`NewMinEstimator(log, children ...NamedEstimator)`) —
  Combinator that queries N named children and returns the lowest successful
  rate.
- `NamedEstimator` — `{Name string; Estimator chainfee.Estimator}` pair used
  to label a `MinEstimator` child for logging.
- `Subsystem = "FEES"` — logging subsystem tag.

## Relationships

- **Depends on**: none repo-internal — only stdlib and third-party
  (`btcsuite/btcd/chaincfg`, `btcsuite/btclog/v2`, `lightninglabs/lndclient`,
  `lightningnetwork/lnd/lnwallet/chainfee`).
- **Depended on by**: `chainbackends` (`LndClientFeeEstimator` type-aliases
  `WalletKitEstimator`, built via `NewFallbackWalletKitEstimator`),
  `darepod` (composes a fail-fast `WalletKitEstimator` and a
  `MempoolSpaceEstimator` into a `MinEstimator` when the mempool.space fee
  provider is enabled; registers `Subsystem` for logging).
- **Sends/Receives**: none — pure synchronous library exposing the
  `chainfee.Estimator` interface (`EstimateFeePerKW`, `Start`, `Stop`,
  `RelayFeePerKW`). No actor or message-passing involvement.

## Invariants

- Never compose a `NewFallbackWalletKitEstimator` (fallback-on-error) as a
  child of `MinEstimator`. A stale fallback rate could incorrectly "win" (be
  lower) against another provider's live estimate — use the fail-fast
  `NewWalletKitEstimator` inside any `MinEstimator` composition.
- `WalletKitEstimator` in fallback mode fails closed before any successful
  estimate: `cachedRate()` treats any cached rate below
  `chainfee.FeePerKwFloor` as "no cached rate" (successful estimates are
  always clamped to the floor before caching), so a cold-start fallback
  estimator errors rather than silently returning the relay floor.
- Every estimator clamps any raw rate below `chainfee.FeePerKwFloor` up to
  the floor before returning/caching it, logging a `WarnS`.
- If every child of a `MinEstimator` errors, it does not propagate the
  error — it returns the last successfully selected rate (or the floor, if
  none yet) with a `WarnS` log. Callers relying on `MinEstimator` will not
  see a hard failure from `EstimateFeePerKW` once any child has succeeded
  at least once.
- `validateMempoolSpaceURL` rejects any non-HTTPS URL except loopback hosts
  (used only by the local httptest server); relaxing this would let fee data
  be tampered with by a network MITM.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
