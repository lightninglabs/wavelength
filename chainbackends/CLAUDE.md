# chainbackends

## Purpose

Concrete implementations of the `chainsource.ChainBackend` interface. Provides
`LNDBackend` wrapping lnd's chainntnfs for real-time chain notifications, fee
estimation, and optional v3 package relay via a pluggable `PackageSubmitter`.

## Key Types

- `LNDBackend` — Full-node backend wrapping lnd's chain notification and fee
  estimation interfaces. Accepts an optional `PackageSubmitter` for v3 CPFP
  package relay (set via `SetPackageSubmitter`).
- `TxBroadcaster` — Interface over transaction broadcasting (wraps
  lndclient.WalletKitClient or in-process lnd).
- `PackageSubmitter` — Optional interface for v3 package relay:
  `SubmitPackage(ctx, parents, child, maxFeeRate)`. Used by backends that need
  a direct bitcoind path for atomic parent+child submission; absent in
  environments that do not support package relay.
- `LndClientTxBroadcaster` — Implements `TxBroadcaster` using
  `lndclient.WalletKitClient`.
- `LndClientFeeEstimator` — Implements `chainfee.Estimator` using
  `lndclient.WalletKitClient` with a 30-second per-call timeout.
- `LndClientChainNotifier` / `LndClientChainNotifierConfig` — Implements
  `chainntnfs.ChainNotifier` using lndclient. Uses a 15-second registration
  timeout and goroutine-based forwarding to bridge lndclient's height-only
  block events to the full `chainntnfs` interface.
- `LNDBackendFromLndClientConfig` — Config struct for building an `LNDBackend`
  from lndclient services (notifier, wallet kit, chain kit).
- `NewLNDBackendFromLndClient(cfg)` — Factory constructing a full `LNDBackend`
  from an `LNDBackendFromLndClientConfig`.
- `PackageTxError` — Diagnostic wrapper for per-tx errors returned by
  `SubmitPackage`. Maps raw reject-reason strings to typed btcd/bitcoind
  sentinels via `rpcclient.MapRPCErr` so callers can `errors.Is` against
  typed errors instead of substring-matching. Exposes `Wtxid`, `Txid`,
  `Reason` (raw string), and an `Unwrap()` returning the mapped sentinel.
- `WalkPackageTxErrors(err, fn)` — Tree-walker that visits every
  `PackageTxError` in a nested error chain, allowing callers to inspect or
  classify all per-tx rejections from a single `SubmitPackage` response.

## Relationships

- **Depends on**: `chainsource` (implements `ChainBackend` interface).
- **Depended on by**: `darepod` (instantiates backend and wires a
  `PackageSubmitter` from operator config: production uses
  `chainbackends/bitcoindrpc.PackageSubmitter` directly, itests inject the
  same submitter from the harness).

## Invariants

- `LNDBackend` requires an lnd instance (local or remote via lndclient).
- Provides real-time notifications via lnd's chainntnfs package.
- `PackageSubmitter` is optional; package-capable backends return an error
  from `SubmitPackage` when no submitter is set. In production `cmd/darepod`
  injects
  `chainbackends/bitcoindrpc.PackageSubmitter` when bitcoind flags are
  configured; the itest harness injects the same type via
  `darepod.Config.PackageSubmitter`.
- `LndClientChainNotifier` enforces a 15-second timeout on registration to
  prevent hanging under LND block load.
- Log messages use canonical txid strings (not reversed byte slices).

## Deep Docs

- [chainbackends/doc.go](doc.go) — Package overview.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
