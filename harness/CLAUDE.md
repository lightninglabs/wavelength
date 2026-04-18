# harness

## Purpose

Docker-based Bitcoin/LND integration test environment. Manages bitcoind and LND
containers with network isolation for end-to-end testing.

## Relationships

- **Depends on**: `chain` (bitcoind RPC), `lndbackend` (LND integration).
- **Depended on by**: `systest` (system-level tests).

## Key Types

- `BitcoindPackageSubmitter` — Implements `chainbackends.PackageSubmitter` via direct JSON-RPC calls to bitcoind's `submitpackage` method. Used by the integration test harness to enable v3 package relay without going through LND. Constructed via `NewBitcoindPackageSubmitter(host, user, password)`.

## Key Constants

- `numInitialBlocks` = 106, `defaultTimeout` = 30s, `pollInterval` = 200ms.
- Coinbase maturity: 100 blocks + 6-block buffer.
- `BitcoindRPCUser` / `BitcoindRPCPass` — Exported RPC credentials for bitcoind; referenced by `BitcoindPackageSubmitter` and system tests.
