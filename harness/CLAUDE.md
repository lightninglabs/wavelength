# harness

## Purpose

Docker-based Bitcoin/LND integration test environment. Manages bitcoind and LND
containers with network isolation for end-to-end testing.

## Key Types

- `Harness` — Top-level test harness owning bitcoind, lnd, and arkd lifecycle.
- `LndInstance` — Manages an LND container's lifecycle and connection.
- `TapdHarness` — Optional Tapd instance for asset-related tests.

## Relationships

- **Depends on**: `chain` (bitcoind RPC), `lndbackend` (LND integration),
  `chainbackends` (PackageSubmitter interface).
- **Depended on by**: `systest` (system-level tests).

## Key Constants

- `numInitialBlocks` = 106, `defaultTimeout` = 30s, `pollInterval` = 200ms.
- `electrsReadyTimeout` = 2 minutes — longer window for electrs Esplora HTTP
  endpoint readiness; CI parallelism occasionally races past `defaultTimeout`
  due to docker layer cache + HTTP listener bring-up, producing spurious
  failures unrelated to the protocol under test.
- Coinbase maturity: 100 blocks + 6-block buffer.
