# harness

## Purpose

Docker-based Bitcoin/LND integration test environment. Manages bitcoind and LND
containers with network isolation for end-to-end testing.

## Key Types

- `Harness` — Top-level test harness owning container lifecycle, network
  isolation, mining, and LND/Tapd coordination.
- `BitcoindPackageSubmitter` — Implements `chainbackends.PackageSubmitter` via
  direct JSON-RPC to bitcoind. Used in itests to provide package submission
  without going through LND. Sets `maxfeerate=0` so high-feerate CPFP children
  are not rejected by the default per-tx limit.
- `LndInstance` — Holds connection details (ports, TLS/macaroon paths) for a
  single LND container in the harness.

## Key Exported Symbols

- `BitcoindRPCUser` / `BitcoindRPCPass` — Regtest credentials used by bitcoind,
  Electrs, and LND. Exported so sub-components (e.g., `BitcoindPackageSubmitter`)
  can reference them without duplicating.
- `GetLNDClientConn` — Creates an authenticated gRPC connection to an LND
  instance using TLS and macaroon paths.

## Relationships

- **Depends on**: `chainbackends` (`PackageSubmitter` interface), `lndbackend`
  (LND integration).
- **Depended on by**: `systest` (system-level tests).

## Invariants

- `numInitialBlocks` = 106 (100 maturity + 6 buffer), `defaultTimeout` = 30s.
- `bitcoindPackageSubmitTimeout` = 30s caps JSON-RPC to bitcoind so a wedged
  node can't stall a test for the full Go test timeout.

## Deep Docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — System-wide package map.
