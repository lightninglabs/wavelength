# chainbackends

## Purpose

Concrete implementations of the `chainsource.ChainBackend` interface. Currently
provides `LNDBackend` wrapping lnd's chainntnfs for real-time chain
notifications and fee estimation.

## Key Types

- `LNDBackend` — Full-node backend wrapping lnd's chain notification and fee
  estimation interfaces. Implements `chainsource.ChainBackend` including
  `SubmitPackage` via an optional `PackageSubmitter`. Call `SetPackageSubmitter`
  to enable package relay; without it `SubmitPackage` returns an unsupported
  error.
- `PackageSubmitter` — Interface for atomic parent+child package submission:
  `SubmitPackage(ctx, parents, child, maxFeeRate) (*btcjson.SubmitPackageResult, error)`.
  Satisfied by `harness.BitcoindPackageSubmitter` (direct JSON-RPC) and similar
  bitcoind-backed adapters.
- `LNDBackendFromLndClientConfig` — Config struct for building `LNDBackend` from
  an `lndclient` client. Includes `PackageSubmitter PackageSubmitter` field (nil
  disables package submission).
- `TxBroadcaster` — Interface for single-tx broadcast (separate from package
  submission).
- `LndClientChainNotifier` — Wraps `lndclient.ChainNotifierClient`. Its
  `RegisterConfirmationsNtfn` runs the underlying RPC in a goroutine with a
  15-second timeout to prevent hanging when LND is slow under block load.

## Relationships

- **Depends on**: `chainsource` (implements `ChainBackend` interface).
- **Depended on by**: `darepod` (instantiates backend), `harness` (provides
  `PackageSubmitter` implementation).

## Invariants

- `LNDBackend` requires an lnd instance (local or remote via lndclient).
- Provides real-time notifications via lnd's chainntnfs package.
- Log messages use canonical txid strings (not reversed byte slices).
- `SubmitPackage` only inspects `result.PackageMsg` after per-tx errors to avoid
  `fmt.Errorf("%w", nil)` producing a malformed error message when `errors.Join`
  returns nil on an empty slice.

## Deep Docs

- [chainbackends/doc.go](doc.go) — Package overview.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
