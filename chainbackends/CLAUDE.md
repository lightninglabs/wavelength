# chainbackends

## Purpose

Concrete implementations of the `chainsource.ChainBackend` interface. Currently
provides `LNDBackend` wrapping lnd's chainntnfs for real-time chain
notifications and fee estimation, plus adapters for lndclient and optional
v3 package relay.

## Key Types

- `LNDBackend` — Full-node backend wrapping lnd's chain notification and fee estimation interfaces. Optionally holds a `PackageSubmitter` for atomic parent+child package relay; call `SetPackageSubmitter` to attach it.
- `PackageSubmitter` — Interface for atomic parent+child transaction package submission (`SubmitPackage`). Implemented by `harness.BitcoindPackageSubmitter` for tests; production typically uses a direct bitcoind RPC client.
- `TxBroadcaster` — Interface for transaction broadcasting, implemented by `LndClientTxBroadcaster`.
- `LndClientTxBroadcaster` — lndclient-backed `TxBroadcaster` adapter.
- `LndClientFeeEstimator` — lndclient-backed fee estimation adapter.
- `LndClientChainNotifier` — lndclient-backed chain notification adapter. `RegisterConfirmationsNtfn` runs the lndclient call in a goroutine with a 15-second timeout to avoid blocking under heavy block load.
- `LNDBackendFromLndClientConfig` — Constructor config for `NewLNDBackendFromLndClient`. Includes optional `PackageSubmitter` field.

## Relationships

- **Depends on**: `chainsource` (implements `ChainBackend` interface).
- **Depended on by**: `darepod` (instantiates backend), `harness` (provides `BitcoindPackageSubmitter`).

## Invariants

- `LNDBackend` requires an lnd instance (local or remote via lndclient).
- `SubmitPackage` returns an error if `packageSubmitter` is nil; callers that need package relay must call `SetPackageSubmitter` or set `PackageSubmitter` in `LNDBackendFromLndClientConfig`.
- Provides real-time notifications via lnd's chainntnfs package.
- Log messages use canonical txid strings (not reversed byte slices).

## Deep Docs

- [chainbackends/doc.go](doc.go) — Package overview.
- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
