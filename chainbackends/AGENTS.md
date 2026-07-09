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
  `SubmitPackage(ctx, parents, child, maxFeeRate)`. Pluggable: implementations
  exist for a direct bitcoind path (`chainbackends/bitcoindrpc`) and for
  relaying through lnd's own `WalletKit.SubmitPackage` RPC
  (`chainbackends/lndsubmitter`); absent in environments that do not support
  package relay.
- `LndClientTxBroadcaster` — Implements `TxBroadcaster` using
  `lndclient.WalletKitClient`.
- `LndClientFeeEstimator` — Type alias for
  `chainfees.WalletKitEstimator`, backed by `lndclient.WalletKitClient` with
  a 15-second per-call timeout and last-good fallback semantics.
- `LndClientChainNotifier` / `LndClientChainNotifierConfig` — Implements
  `chainntnfs.ChainNotifier` using lndclient. Uses a 15-second registration
  timeout and goroutine-based forwarding to bridge lndclient's height-only
  block events to the full `chainntnfs` interface.
- `LNDBackendFromLndClientConfig` — Config struct for building an `LNDBackend`
  from lndclient services (notifier, wallet kit, chain kit).
- `NewLNDBackendFromLndClient(cfg)` — Factory constructing a full `LNDBackend`
  from an `LNDBackendFromLndClientConfig`.
- `PackageTxError` — Per-tx result error from a `SubmitPackage` response.
  Carries `Wtxid`, `Txid`, and raw `Reason`; unwraps to the mapped
  `rpcclient`-sentinel (via `rpcclient.MapRPCErr`) so callers can use
  `errors.Is` against typed sentinels (e.g. `rpcclient.ErrTxAlreadyKnown`,
  `rpcclient.ErrInsufficientFee`) instead of substring-matching reject
  strings.
- `NewPackageTxError(wtxid, txid, reason)` — Eagerly maps the reject reason to
  a typed sentinel at construction time.
- `WalkPackageTxErrors(err, fn)` — Walks both `Unwrap() error` and
  `Unwrap() []error` shapes to invoke `fn` for every `*PackageTxError` in a
  joined error tree. Use this instead of `errors.As` when all per-tx entries
  must be inspected (e.g. to distinguish parent-known vs. child-fee
  classification).

## Relationships

- **Depends on**: `chainsource` (implements `ChainBackend` interface).
- **Depended on by**: `darepod` (instantiates backend and wires a
  `PackageSubmitter`: an explicitly configured submitter — production's
  `chainbackends/bitcoindrpc.PackageSubmitter` when bitcoind flags are set, or
  the itest harness's injected submitter via `darepod.Config.PackageSubmitter`
  — takes precedence; otherwise `darepod` falls back to
  `chainbackends/lndsubmitter.New(lndSvc.WalletKit)` so an lnd-wallet-backed
  daemon can relay packages through lnd's own chain connection with no
  separate bitcoind RPC or Esplora endpoint); `chainbackends/lndsubmitter`
  (sibling sub-package that implements `PackageSubmitter` against lnd's
  `WalletKit.SubmitPackage` RPC).

## Invariants

- `LNDBackend` requires an lnd instance (local or remote via lndclient).
- Provides real-time notifications via lnd's chainntnfs package.
- `PackageSubmitter` is optional at the `LNDBackend` level;
  `SubmitPackage` returns an error when no submitter is set. In practice
  `cmd/darepod` always wires one for the lnd wallet backend: an explicit
  submitter (bitcoind flags / itest harness) takes precedence, otherwise it
  defaults to `chainbackends/lndsubmitter` backed by lnd's own WalletKit.
- When the configured submitter reports lnd's neutrino
  `"broadcast-unverified"` sentinel (`lndNeutrinoBroadcastMsg`) with no
  per-tx errors, `LNDBackend.SubmitPackage` treats it as a successful
  best-effort broadcast rather than a rejection — a light client has no
  mempool and cannot return a real package-accept verdict, so it broadcasts
  each tx individually over P2P and relies on peer relay/confirmation to
  decide the outcome.
- `LndClientChainNotifier` enforces a 15-second timeout on registration to
  prevent hanging under LND block load.
- Log messages use canonical txid strings (not reversed byte slices).

## Deep Docs

- [chainbackends/doc.go](doc.go) — Package overview.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
