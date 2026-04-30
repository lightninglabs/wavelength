# harness

## Purpose

Docker-based Bitcoin/LND integration test environment. Manages bitcoind and LND
containers with network isolation for end-to-end testing.

## Key Types

- `Harness` — Top-level test harness owning bitcoind, lnd, and arkd lifecycle.
- `LndInstance` — Manages an LND container's lifecycle and connection.
- `TapdHarness` — Optional Tapd instance for asset-related tests.
- `Options` — Configuration struct passed to `NewHarness`. Controls image
  tags, artifact directory, log routing, tapd toggle, `GroupName`, and
  `AlwaysKeepArtifacts`.
- `DefaultOptions()` — Returns a populated `Options` with safe defaults.
- `Block` — Mined block header plus txid list; used by mining helpers.
- `BlockHeader` — Verbose bitcoind `getblockheader` RPC representation.
- `SetPostgresEnabled(enabled bool) bool` — Toggles postgres mode
  programmatically; returns old value for restore-on-cleanup patterns.

## Relationships

- **Depends on**: `chain` (bitcoind RPC), `lndbackend` (LND integration),
  `chainbackends` (PackageSubmitter interface).
- **Depended on by**: `systest` (system-level tests).

## Key Constants

- `numInitialBlocks` = 106, `defaultTimeout` = 30s, `pollInterval` = 200ms.
- `BitcoindRPCUser` / `BitcoindRPCPass` — RPC credentials shared across
  tests.
- `electrsReadyTimeout` = 2 minutes — separate extended timeout for the
  electrs container HTTP readiness check.
- Coinbase maturity: 100 blocks + 6-block buffer.
