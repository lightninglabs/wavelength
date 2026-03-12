# harness

## Purpose

Docker-based Bitcoin/LND integration test environment. Manages bitcoind and LND
containers with network isolation for end-to-end testing.

## Relationships

- **Depends on**: `chain` (bitcoind RPC), `lndbackend` (LND integration).
- **Depended on by**: `systest` (system-level tests).

## Key Constants

- `numInitialBlocks` = 106, `defaultTimeout` = 30s, `pollInterval` = 200ms.
- Coinbase maturity: 100 blocks + 6-block buffer.
