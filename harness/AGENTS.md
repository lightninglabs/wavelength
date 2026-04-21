# harness

## Purpose

Docker-based Bitcoin/LND integration test environment. Manages bitcoind and LND
containers with network isolation for end-to-end testing.

## Key Types

- `Harness` — Top-level test harness owning bitcoind, lnd, and arkd lifecycle.
- `BitcoindPackageSubmitter` — Implements `chainbackends.PackageSubmitter` via
  direct bitcoind JSON-RPC. Injected into `darepod.Config.PackageSubmitter` to
  enable v3 CPFP package submission in integration tests without going through
  LND. Constructor: `NewBitcoindPackageSubmitter(host, user, password)`.
- `LndInstance` — Manages an LND container's lifecycle and connection.
- `TapdHarness` — Optional Tapd instance for asset-related tests.

## Relationships

- **Depends on**: `chain` (bitcoind RPC), `lndbackend` (LND integration),
  `chainbackends` (PackageSubmitter interface).
- **Depended on by**: `systest` (system-level tests).

## Key Constants

- `numInitialBlocks` = 106, `defaultTimeout` = 30s, `pollInterval` = 200ms.
- Coinbase maturity: 100 blocks + 6-block buffer.
