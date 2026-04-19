# chainbackends

## Purpose

Concrete implementations of the `chainsource.ChainBackend` interface. Currently
provides `LNDBackend` wrapping lnd's chainntnfs for real-time chain
notifications and fee estimation.

## Key Types

- `LNDBackend` — Full-node backend wrapping lnd's chain notification and fee
  estimation interfaces. Optionally holds a `PackageSubmitter` for V3 package
  relay support; without one, `SubmitPackage` returns an error.
- `PackageSubmitter` — Interface for submitting BIP 331 parent+child
  transaction packages: `SubmitPackage(ctx, parents, child, maxFeeRate) (*btcjson.SubmitPackageResult, error)`. Implemented by `harness.BitcoindPackageSubmitter`.
- `LNDBackendFromLndClientConfig` — Constructor config; the optional
  `PackageSubmitter` field wires package relay capability into the backend.

## Relationships

- **Depends on**: `chainsource` (implements `ChainBackend` interface).
- **Depended on by**: `darepod` (instantiates backend), `harness` (implements
  `PackageSubmitter`).

## Invariants

- `LNDBackend` requires an lnd instance (local or remote via lndclient).
- Provides real-time notifications via lnd's chainntnfs package.
- Log messages use canonical txid strings (not reversed byte slices).
- `LndClientChainNotifier.RegisterConfirmationsNtfn` is wrapped with a 15-second
  timeout to prevent hangs when LND is slow under heavy block processing load.

## Deep Docs

- [chainbackends/doc.go](doc.go) — Package overview.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
